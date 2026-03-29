package cli

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/config"
)

func cortexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cortex",
		Short: "Manage cortexes",
	}
	cmd.AddCommand(cortexListCmd())
	return cmd
}

func cortexListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List all known cortexes",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if len(cfg.Cortexes) == 0 {
				fmt.Println("No cortexes configured. Run `noema init --name <name>` to create one.")
				return nil
			}

			// Sort names for stable output.
			names := make([]string, 0, len(cfg.Cortexes))
			for name := range cfg.Cortexes {
				names = append(names, name)
			}
			sort.Strings(names)

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tPATH\t")
			for _, name := range names {
				marker := " "
				if name == cfg.Default {
					marker = "*"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", name, cfg.Cortexes[name].Path, marker)
			}
			w.Flush()
			return nil
		},
	}
}
