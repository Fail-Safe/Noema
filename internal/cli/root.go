package cli

import (
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/config"
	"github.com/Fail-Safe/Noema/internal/cortex"
)

// Version, Commit, and Date are set via -ldflags at build time.
//
// Version falls back to Go module build info when the binary was produced
// without ldflags (e.g. `go install`, `go run`), and finally to "dev" for
// a working-copy build. Commit and Date have no useful runtime fallback —
// an empty string means "unknown" and callers render it as such.
//
// The Makefile injects Version only; GoReleaser injects all three on
// tagged releases.
var (
	Version = ""
	Commit  = ""
	Date    = ""
)

func version() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

var cortexFlag string

var rootCmd = &cobra.Command{
	Use:     "noema",
	Short:   "The intentional memory layer for your AI agents",
	Version: version(),
	CompletionOptions: cobra.CompletionOptions{
		DisableDefaultCmd: true,
	},
	// Errors from RunE are operational failures (upgrade required, network
	// issues, missing cortex), not flag-usage mistakes. Cobra walks up to the
	// root to find SilenceUsage, so setting it once here suppresses the flag
	// wall across every subcommand.
	SilenceUsage: true,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

const (
	groupTrace   = "trace"
	groupCortex  = "cortex"
	groupIface   = "interface"
)

func init() {
	rootCmd.PersistentFlags().StringVar(&cortexFlag, "cortex", "", "cortex name to use (overrides NOEMA_CORTEX and config default)")
	rootCmd.RegisterFlagCompletionFunc("cortex", cortexNameCompletions)

	rootCmd.AddGroup(
		&cobra.Group{ID: groupTrace, Title: "Trace commands:"},
		&cobra.Group{ID: groupCortex, Title: "Cortex management:"},
		&cobra.Group{ID: groupIface, Title: "Interface:"},
	)

	addGrouped(groupTrace,
		addCmd(), listCmd(), getCmd(), editCmd(), removeCmd(),
		searchCmd(), archiveCmd(), unarchiveCmd(), recoverCmd(), purgeCmd(), syncCmd(),
		eventsCmd(), resolveCmd(), verifyCmd(), driftCmd(), memoryCmd(),
	)
	addGrouped(groupCortex,
		initCmd(), useCmd(), cortexCmd(), federationCmd(), migrateCmd(),
	)
	addGrouped(groupIface,
		serveCmd(), tuiCmd(), completionCmd(),
	)

	// versionCmd and configCmd are intentionally ungrouped — they're
	// meta (about the binary or user settings, not Traces or
	// Cortexes), so they land under cobra's "Additional Commands:"
	// section at the bottom of help.
	rootCmd.AddCommand(versionCmd(), configCmd())
}

func addGrouped(group string, cmds ...*cobra.Command) {
	for _, cmd := range cmds {
		cmd.GroupID = group
		rootCmd.AddCommand(cmd)
	}
}

// cortexNameCompletions is a ValidArgsFunction / RegisterFlagCompletionFunc
// that returns the names of all known cortexes (with their paths as descriptions).
func cortexNameCompletions(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	cfg, err := config.Load()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	out := make([]string, 0, len(cfg.Cortexes))
	for name, entry := range cfg.Cortexes {
		out = append(out, name+"\t"+entry.Path)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// completionsFor returns a ValidArgsFunction that lists trace IDs (with titles
// as descriptions) from the default Cortex, filtered by opts.
func completionsFor(opts cortex.ListOptions) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		cx, err := resolveCortex()
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		defer cx.Close()
		rows, err := cx.List(opts)
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = r.ID + "\t" + r.Title
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// resolveCortex returns an open Cortex using the priority chain:
// --cortex flag → NOEMA_CORTEX env → config default.
//
// If none of those produce a name AND exactly one cortex is registered,
// that cortex is auto-promoted to default and persisted. This repairs
// configs that lost their default field via a manual edit, a legacy
// binary, or a cross-machine sync — rather than erroring out on a
// one-answer question. Zero or many registered cortexes still produce
// a hard error, but one that actually names the fix.
func resolveCortex() (*cortex.Cortex, error) {
	name := cortexFlag
	if name == "" {
		name = os.Getenv("NOEMA_CORTEX")
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	if name == "" {
		name = cfg.Default
	}
	if name == "" {
		resolved, promoted, rerr := resolveDefaultCortex(cfg)
		if rerr != nil {
			return nil, rerr
		}
		name = resolved
		if promoted {
			cfg.Default = name
			if saveErr := cfg.Save(); saveErr != nil {
				// Promotion is already correct in memory, so the
				// command can still run. We just couldn't persist
				// the repair — warn loudly and keep going.
				fmt.Fprintf(os.Stderr,
					"warning: auto-promoted %q as default, but could not persist config (%v)\n",
					name, saveErr,
				)
			} else {
				fmt.Fprintf(os.Stderr,
					"notice: Promoted %q as the default cortex (was unset).\n",
					name,
				)
			}
		}
	}

	entry, ok := cfg.Cortexes[name]
	if !ok {
		return nil, fmt.Errorf("unknown cortex %q — run `noema init --name %s` first", name, name)
	}
	cx, err := cortex.Open(name, entry.Path)
	if err != nil {
		return nil, err
	}

	// If the config remembers a ULID for this entry but the manifest now
	// reports a different one, the on-disk cortex is not the one we
	// registered. This typically means someone copied or replaced the
	// directory out of band, or ran `noema migrate cortex-id` against a
	// directory that the config still believes is something else. Warn
	// loudly but don't fail — the operator may know what they're doing.
	if entry.ID != "" && cx.ID != "" && entry.ID != cx.ID {
		fmt.Fprintf(os.Stderr,
			"warning: cortex %q config id (%s) does not match the manifest id on disk (%s).\n"+
				"  This usually means the directory at %s was replaced, copied, or migrated\n"+
				"  out of band. Federation peers that pinned the old id will refuse to talk to\n"+
				"  this cortex until they re-pair. To accept the on-disk identity as canonical,\n"+
				"  edit ~/.config/noema/config.yaml and update the id for entry %q.\n",
			name, entry.ID, cx.ID, entry.Path, name,
		)
	}
	return cx, nil
}

// resolveDefaultCortex decides what to do when the caller supplied no
// --cortex flag, no NOEMA_CORTEX env, and no cfg.Default. It is the
// three-branch policy that mirrors the rule in `cortex remove`:
//
//   - 0 cortexes: nothing to do — error and point at `noema init`
//   - 1 cortex:   the answer is unambiguous — return it with promoted=true
//     so the caller persists cfg.Default and prints a notice
//   - 2+ cortexes: guessing would be wrong — error with the full list
//     and the exact fix command
//
// Side-effect free by design so it can be tested without touching the
// filesystem or HOME — the caller owns the persistence decision.
func resolveDefaultCortex(cfg *config.Config) (name string, promoted bool, err error) {
	switch len(cfg.Cortexes) {
	case 0:
		return "", false, fmt.Errorf("no cortexes configured — run `noema init --name <name>` to create one")
	case 1:
		for onlyName := range cfg.Cortexes {
			return onlyName, true, nil
		}
	}
	names := make([]string, 0, len(cfg.Cortexes))
	for n := range cfg.Cortexes {
		names = append(names, n)
	}
	sort.Strings(names)
	return "", false, fmt.Errorf(
		"no default cortex set. Pick one with:\n"+
			"  noema use <name>\n"+
			"Registered cortexes: %s",
		strings.Join(names, ", "),
	)
}
