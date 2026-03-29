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

func init() {
	rootCmd.PersistentFlags().StringVar(&cortexFlag, "cortex", "", "cortex name to use (overrides NOEMA_CORTEX and config default)")

	rootCmd.AddCommand(
		initCmd(),
		useCmd(),
		cortexCmd(),
		addCmd(),
		listCmd(),
		getCmd(),
		editCmd(),
		removeCmd(),
		searchCmd(),
		archiveCmd(),
		unarchiveCmd(),
		serveCmd(),
		tuiCmd(),
	)
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
