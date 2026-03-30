package cli

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/config"
	"github.com/Fail-Safe/Noema/internal/cortex"
)

// Version is set via -ldflags at build time; falls back to module info.
var Version = ""

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
	)
	addGrouped(groupCortex,
		initCmd(), useCmd(), cortexCmd(),
	)
	addGrouped(groupIface,
		serveCmd(), tuiCmd(), completionCmd(),
	)
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
		return nil, fmt.Errorf("no cortex specified: use --cortex, set NOEMA_CORTEX, or run `noema use <name>`")
	}

	entry, ok := cfg.Cortexes[name]
	if !ok {
		return nil, fmt.Errorf("unknown cortex %q — run `noema init --name %s` first", name, name)
	}
	return cortex.Open(name, entry.Path)
}
