package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/config"
)

func useCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "use <name>",
		Short:             "Set the default Cortex",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: cortexNameCompletions,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if _, ok := cfg.Cortexes[name]; !ok {
				return fmt.Errorf("unknown cortex %q — run `noema init --name %s` first", name, name)
			}
			cfg.Default = name
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Printf("Default cortex set to %q\n", name)
			return nil
		},
	}
}
