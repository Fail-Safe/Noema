package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func removeCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:     "remove <id>",
		Aliases: []string{"rm", "delete"},
		Short:   "Permanently remove a Trace",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			id := args[0]
			if _, err := cx.Get(id); err != nil {
				return fmt.Errorf("trace %q not found", id)
			}

			if !force {
				fmt.Printf("Remove trace %q? This cannot be undone. [y/N]: ", id)
				scanner := bufio.NewScanner(os.Stdin)
				if scanner.Scan() {
					if strings.ToLower(strings.TrimSpace(scanner.Text())) != "y" {
						fmt.Println("Cancelled.")
						return nil
					}
				}
			}

			if err := cx.Remove(id); err != nil {
				return err
			}
			fmt.Printf("Trace %s removed.\n", id)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "skip confirmation prompt")
	return cmd
}
