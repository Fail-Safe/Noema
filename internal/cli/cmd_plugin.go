package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	pluginpkg "github.com/Fail-Safe/Noema/internal/plugin"
	hermesplugin "github.com/Fail-Safe/Noema/plugins/hermes"
	obsidianplugin "github.com/Fail-Safe/Noema/plugins/obsidian"
)

var errPluginCheck = errors.New("plugin check failed")

func pluginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Install and inspect Noema integrations",
		Long: "Install the Hermes and Obsidian runtime files embedded in this " +
			"Noema binary, or compare an existing installation with the embedded files.",
	}
	cmd.AddCommand(
		pluginListCmd(),
		pluginStatusCmd(),
		pluginHermesCmd(),
		pluginObsidianCmd(),
	)
	return cmd
}

func pluginDefinitions() []pluginpkg.Definition {
	return []pluginpkg.Definition{
		{
			Name:         "hermes",
			Description:  "Hermes memory provider",
			Files:        hermesplugin.Files,
			ManagedFiles: hermesplugin.ManagedFiles,
		},
		{
			Name:         "obsidian",
			Description:  "Obsidian vault plugin",
			Files:        obsidianplugin.Files,
			ManagedFiles: obsidianplugin.ManagedFiles,
		},
	}
}

func pluginDefinition(name string) pluginpkg.Definition {
	for _, def := range pluginDefinitions() {
		if def.Name == name {
			return def
		}
	}
	panic("unknown built-in plugin: " + name)
}

func pluginListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List plugins embedded in this binary",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginList(cmd.OutOrStdout(), pluginDefinitions())
		},
	}
}

func runPluginList(out io.Writer, definitions []pluginpkg.Definition) error {
	definitions = append([]pluginpkg.Definition(nil), definitions...)
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	defer w.Flush()
	for _, def := range definitions {
		fmt.Fprintf(w, "%s\t%s\t%d managed files\n", def.Name, def.Description, len(def.ManagedFiles))
	}
	return nil
}

type pluginStatusTarget struct {
	definition pluginpkg.Definition
	target     string
	specified  bool
}

func pluginStatusCmd() *cobra.Command {
	var (
		check      bool
		hermesHome string
		vault      string
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Check embedded plugins for installation drift",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			hermesTarget, err := resolveHermesTarget(hermesHome)
			if err != nil {
				return err
			}
			targets := []pluginStatusTarget{{
				definition: pluginDefinition("hermes"),
				target:     hermesTarget,
				specified:  true,
			}}
			if vault == "" {
				targets = append(targets, pluginStatusTarget{definition: pluginDefinition("obsidian")})
			} else {
				obsidianTarget, resolveErr := resolveObsidianTarget(vault)
				if resolveErr != nil {
					return resolveErr
				}
				targets = append(targets, pluginStatusTarget{
					definition: pluginDefinition("obsidian"),
					target:     obsidianTarget,
					specified:  true,
				})
			}
			return runPluginStatus(cmd.OutOrStdout(), targets, check)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "exit nonzero when a selected plugin is missing or drifted")
	cmd.Flags().StringVar(&hermesHome, "hermes-home", "", "Hermes installation root (default: HERMES_HOME or $HOME/.hermes/hermes-agent)")
	cmd.Flags().StringVar(&vault, "vault", "", "Obsidian vault to inspect (omitted vaults are not checked)")
	return cmd
}

func runPluginStatus(out io.Writer, targets []pluginStatusTarget, check bool) error {
	failed := false
	for i, target := range targets {
		if i > 0 {
			fmt.Fprintln(out)
		}
		if !target.specified {
			fmt.Fprintf(out, "%s: target not specified\n", target.definition.Name)
			continue
		}
		report, err := pluginpkg.Inspect(target.definition, target.target)
		if err != nil {
			return fmt.Errorf("%s status: %w", target.definition.Name, err)
		}
		renderPluginStatus(out, report)
		if check && report.State != pluginpkg.StateUpToDate {
			failed = true
		}
	}
	if failed {
		return errPluginCheck
	}
	return nil
}

func renderPluginStatus(out io.Writer, report pluginpkg.StatusReport) {
	fmt.Fprintf(out, "%s: %s\n", report.Plugin, report.State)
	fmt.Fprintf(out, "  target: %s\n", report.Target)
	for _, file := range report.Files {
		fmt.Fprintf(out, "  %-13s %s", file.State, file.Path)
		if file.State == pluginpkg.FileChanged {
			fmt.Fprintf(out, "\n    embedded:  %s\n    installed: %s", file.EmbeddedHash, file.InstalledHash)
		}
		fmt.Fprintln(out)
	}
}

func pluginHermesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "hermes", Short: "Manage the Hermes memory-provider plugin"}
	cmd.AddCommand(pluginHermesStatusCmd(), pluginHermesInstallCmd())
	return cmd
}

func pluginHermesStatusCmd() *cobra.Command {
	var (
		check      bool
		hermesHome string
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Compare the installed Hermes plugin with this binary",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveHermesTarget(hermesHome)
			if err != nil {
				return err
			}
			return runPluginStatus(cmd.OutOrStdout(), []pluginStatusTarget{{
				definition: pluginDefinition("hermes"), target: target, specified: true,
			}}, check)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "exit nonzero when the plugin is missing or drifted")
	cmd.Flags().StringVar(&hermesHome, "hermes-home", "", "Hermes installation root (default: HERMES_HOME or $HOME/.hermes/hermes-agent)")
	return cmd
}

func pluginHermesInstallCmd() *cobra.Command {
	var (
		check      bool
		force      bool
		hermesHome string
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the embedded Hermes runtime files",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveHermesTarget(hermesHome)
			if err != nil {
				return err
			}
			if err := validateHermesTarget(target); err != nil {
				return err
			}
			return runPluginInstall(cmd.OutOrStdout(), pluginDefinition("hermes"), target, check, force)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "report required changes without writing files")
	cmd.Flags().BoolVar(&force, "force", false, "replace changed managed files")
	cmd.Flags().StringVar(&hermesHome, "hermes-home", "", "Hermes installation root (default: HERMES_HOME or $HOME/.hermes/hermes-agent)")
	return cmd
}

func pluginObsidianCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "obsidian", Short: "Manage the Obsidian vault plugin"}
	cmd.AddCommand(pluginObsidianStatusCmd(), pluginObsidianInstallCmd())
	return cmd
}

func pluginObsidianStatusCmd() *cobra.Command {
	var (
		check bool
		vault string
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Compare an installed Obsidian plugin with this binary",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveObsidianTarget(vault)
			if err != nil {
				return err
			}
			return runPluginStatus(cmd.OutOrStdout(), []pluginStatusTarget{{
				definition: pluginDefinition("obsidian"), target: target, specified: true,
			}}, check)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "exit nonzero when the plugin is missing or drifted")
	cmd.Flags().StringVar(&vault, "vault", "", "Obsidian vault path (required)")
	cmd.MarkFlagRequired("vault")
	return cmd
}

func pluginObsidianInstallCmd() *cobra.Command {
	var (
		check bool
		force bool
		vault string
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the embedded Obsidian runtime files",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveObsidianTarget(vault)
			if err != nil {
				return err
			}
			if err := validateObsidianTarget(target); err != nil {
				return err
			}
			return runPluginInstall(cmd.OutOrStdout(), pluginDefinition("obsidian"), target, check, force)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "report required changes without writing files")
	cmd.Flags().BoolVar(&force, "force", false, "replace changed managed files")
	cmd.Flags().StringVar(&vault, "vault", "", "Obsidian vault path (required)")
	cmd.MarkFlagRequired("vault")
	return cmd
}

func runPluginInstall(out io.Writer, def pluginpkg.Definition, target string, check, force bool) error {
	report, err := pluginpkg.Install(def, target, pluginpkg.InstallOptions{Check: check, Force: force})
	if err != nil {
		return fmt.Errorf("%s install: %w", def.Name, err)
	}

	fmt.Fprintf(out, "%s: %s\n", def.Name, map[bool]string{true: "check", false: "install"}[check])
	fmt.Fprintf(out, "  target: %s\n", report.Target)
	counts := make(map[pluginpkg.InstallAction]int)
	for _, file := range report.Files {
		counts[file.Action]++
		fmt.Fprintf(out, "  %-13s %s\n", file.Action, file.Path)
	}
	fmt.Fprintf(out, "summary: installed=%d replaced=%d unchanged=%d refused=%d would_install=%d would_replace=%d\n",
		counts[pluginpkg.ActionInstalled], counts[pluginpkg.ActionReplaced], counts[pluginpkg.ActionUnchanged],
		counts[pluginpkg.ActionRefused], counts[pluginpkg.ActionWouldInstall], counts[pluginpkg.ActionWouldReplace])

	if report.Refused() || (check && report.Pending()) {
		return errPluginCheck
	}
	return nil
}

func resolveHermesTarget(flagValue string) (string, error) {
	home := flagValue
	if home == "" {
		home = os.Getenv("HERMES_HOME")
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home directory: %w", err)
		}
		home = filepath.Join(userHome, ".hermes", "hermes-agent")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolving Hermes home %q: %w", home, err)
	}
	return filepath.Join(filepath.Clean(abs), "plugins", "memory", "noema"), nil
}

func resolveObsidianTarget(vault string) (string, error) {
	if vault == "" {
		return "", fmt.Errorf("--vault is required for the Obsidian plugin")
	}
	abs, err := filepath.Abs(vault)
	if err != nil {
		return "", fmt.Errorf("resolving Obsidian vault %q: %w", vault, err)
	}
	return filepath.Join(filepath.Clean(abs), ".obsidian", "plugins", "noema"), nil
}

func validateHermesTarget(target string) error {
	parent := filepath.Dir(target)
	return requireDirectory(parent, "Hermes plugin parent")
}

func validateObsidianTarget(target string) error {
	vaultConfig := filepath.Dir(filepath.Dir(target))
	return requireDirectory(vaultConfig, "Obsidian vault configuration")
}

func requireDirectory(path, label string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("%s not found at %s", label, path)
	}
	if err != nil {
		return fmt.Errorf("inspecting %s at %s: %w", label, path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s at %s is not a directory", label, path)
	}
	return nil
}
