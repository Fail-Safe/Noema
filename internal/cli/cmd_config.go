package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/config"
)

// configCmd exposes a `get`/`set` surface over a small whitelist of
// user-level settings in ~/.config/noema/config.yaml. Keys are flat
// dotted names (`ui.theme`, `trash_days`) — we refuse unknown keys
// rather than letting users scribble arbitrary fields into the file,
// so the YAML stays parseable by older binaries.
func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Get or set user-level settings",
	}
	cmd.AddCommand(configGetCmd(), configSetCmd(), configListCmd())
	return cmd
}

// configKey describes one settable field. Parse validates + coerces
// the user's raw string into the Go type the Config struct expects.
// Apply writes the parsed value into the loaded config. Format turns
// the current value back into a user-facing display string.
type configKey struct {
	name     string
	describe string
	parse    func(raw string) (any, error)
	apply    func(cfg *config.Config, value any)
	format   func(cfg *config.Config) string
}

// configKeys is the whitelist. Adding a new setting is a three-field
// entry here; nothing else about `noema config` has to change.
var configKeys = []configKey{
	{
		name:     "ui.theme",
		describe: `TUI color scheme — "auto", "dark", or "light"`,
		parse: func(raw string) (any, error) {
			switch raw {
			case "auto", "dark", "light":
				return raw, nil
			case "":
				// Treat empty as "clear the preference" — handled in apply
				// by passing the empty string through to SetTheme.
				return "", nil
			default:
				return nil, fmt.Errorf(`invalid theme %q: must be "auto", "dark", or "light"`, raw)
			}
		},
		apply: func(cfg *config.Config, value any) {
			cfg.SetTheme(value.(string))
		},
		format: func(cfg *config.Config) string {
			return cfg.Theme()
		},
	},
	{
		name:     "trash_days",
		describe: "How many days trashed traces are kept before auto-purge (0 = default of 30)",
		parse: func(raw string) (any, error) {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid trash_days %q: must be a non-negative integer", raw)
			}
			if n < 0 {
				return nil, fmt.Errorf("invalid trash_days %d: must be non-negative", n)
			}
			return n, nil
		},
		apply: func(cfg *config.Config, value any) {
			cfg.TrashDays = value.(int)
		},
		format: func(cfg *config.Config) string {
			if cfg.TrashDays == 0 {
				return "0 (default: 30)"
			}
			return strconv.Itoa(cfg.TrashDays)
		},
	},
}

func lookupConfigKey(name string) (configKey, bool) {
	for _, k := range configKeys {
		if k.name == name {
			return k, true
		}
	}
	return configKey{}, false
}

func knownKeyNames() string {
	names := make([]string, 0, len(configKeys))
	for _, k := range configKeys {
		names = append(names, k.name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func configGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print the current value of a config key",
		Args:  cobra.ExactArgs(1),
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			names := make([]string, 0, len(configKeys))
			for _, k := range configKeys {
				names = append(names, k.name+"\t"+k.describe)
			}
			return names, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			key, ok := lookupConfigKey(args[0])
			if !ok {
				return fmt.Errorf("unknown config key %q — known keys: %s", args[0], knownKeyNames())
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), key.format(cfg))
			return nil
		},
	}
}

func configSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Update a config key and persist it",
		Args:  cobra.ExactArgs(2),
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				names := make([]string, 0, len(configKeys))
				for _, k := range configKeys {
					names = append(names, k.name+"\t"+k.describe)
				}
				return names, cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			key, ok := lookupConfigKey(args[0])
			if !ok {
				return fmt.Errorf("unknown config key %q — known keys: %s", args[0], knownKeyNames())
			}
			parsed, err := key.parse(args[1])
			if err != nil {
				return err
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			key.apply(cfg, parsed)
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s = %s\n", key.name, key.format(cfg))
			return nil
		},
	}
}

func configListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List every known config key with its current value",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			sorted := make([]configKey, len(configKeys))
			copy(sorted, configKeys)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })

			w := cmd.OutOrStdout()
			for _, k := range sorted {
				fmt.Fprintf(w, "%-12s %s\n", k.name, k.format(cfg))
				fmt.Fprintf(w, "             %s\n", k.describe)
			}
			return nil
		},
	}
}
