package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

func editCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit <id>",
		Short: "Edit a Trace in $EDITOR",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			id := args[0]
			row, err := cx.Get(id)
			if err != nil {
				return fmt.Errorf("trace %q not found", id)
			}

			path := cx.TraceFile(id, row.ArchivedAt != "")
			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = os.Getenv("VISUAL")
			}
			if editor == "" {
				return fmt.Errorf("$EDITOR is not set")
			}

			c := exec.Command(editor, path)
			c.Stdin = os.Stdin
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			if err := c.Run(); err != nil {
				return fmt.Errorf("editor exited with error: %w", err)
			}

			if err := cx.Update(id); err != nil {
				return fmt.Errorf("syncing database: %w", err)
			}
			fmt.Printf("Trace %s updated.\n", id)
			return nil
		},
	}
}
