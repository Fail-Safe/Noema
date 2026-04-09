package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// versionCmd is a dedicated subcommand that prints the build metadata
// injected by GoReleaser (version + commit + date) on a tagged release.
// Cobra's built-in --version flag on the root command only shows the
// version string; this subcommand exists so operators can answer
// "exactly which build is this?" without needing to scrape logs.
//
// Output adapts to what ldflags populated:
//
//   - Release build (GoReleaser): all three lines present
//   - Makefile build: just the version string (git describe output)
//   - `go install` / `go run`: module path version or "dev"
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print noema version, commit, and build date",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("noema %s\n", version())
			if Commit != "" {
				fmt.Printf("  commit: %s\n", Commit)
			}
			if Date != "" {
				fmt.Printf("  built:  %s\n", Date)
			}
		},
	}
}
