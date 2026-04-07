package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func resolveCmd() *cobra.Command {
	var (
		acceptOrigin string
		customBody   string
	)

	cmd := &cobra.Command{
		Use:   "resolve <divergence-id>",
		Short: "Resolve a divergence (concurrent edit conflict)",
		Long: `Resolve a divergence trace by either accepting one of the recorded
versions (by origin name) or supplying a custom merged body.

Examples:
  noema resolve 20260406-divergence-... --accept ai-1
  noema resolve 20260406-divergence-... --custom "merged content here"

Use 'noema get <divergence-id>' to see the available origins listed under
'**Conflicting origins:**' in the divergence body.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if acceptOrigin == "" && customBody == "" {
				return fmt.Errorf("specify --accept <origin> or --custom <body>")
			}
			if acceptOrigin != "" && customBody != "" {
				return fmt.Errorf("specify only one of --accept or --custom")
			}

			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			if err := cx.ResolveDivergence(args[0], acceptOrigin, customBody); err != nil {
				return err
			}
			if acceptOrigin != "" {
				fmt.Printf("Divergence %s resolved (accepted %s).\n", args[0], acceptOrigin)
			} else {
				fmt.Printf("Divergence %s resolved (custom merge).\n", args[0])
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&acceptOrigin, "accept", "", "Origin name whose version to accept (see 'Conflicting origins' in the divergence body)")
	cmd.Flags().StringVar(&customBody, "custom", "", "Custom merged body to apply to the original trace")
	return cmd
}
