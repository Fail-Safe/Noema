package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func archiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "archive <id>",
		Short: "Archive a Trace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			if err := cx.Archive(args[0]); err != nil {
				return err
			}
			fmt.Printf("Trace %s archived.\n", args[0])
			return nil
		},
	}
}

func unarchiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unarchive <id>",
		Short: "Restore an archived Trace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			if err := cx.Unarchive(args[0]); err != nil {
				return err
			}
			fmt.Printf("Trace %s restored.\n", args[0])
			return nil
		},
	}
}
