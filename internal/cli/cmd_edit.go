package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

func editCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:               "edit <id>",
		Short:             "Edit a Trace in $EDITOR",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completionsFor(cortex.ListOptions{}),
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

			if row.SourceLocked && row.Origin != cx.Name && !force {
				return fmt.Errorf("trace %q is source-locked by origin %q (use --force to override)", id, row.Origin)
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

			if force {
				cx.SetForceSourceLock(true)
			}
			if err := cx.Update(id); err != nil {
				if errors.Is(err, cortex.ErrSourceLocked) {
					return fmt.Errorf("%w (use --force to override)", err)
				}
				return fmt.Errorf("syncing database: %w", err)
			}
			fmt.Printf("Trace %s updated.\n", id)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "bypass source-lock protection")
	return cmd
}
