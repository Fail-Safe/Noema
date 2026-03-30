package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

func getCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "get <id>",
		Short:             "Show a Trace",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completionsFor(cortex.ListOptions{All: true}),
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
			tr, err := trace.ParseFile(path)
			if err != nil {
				return fmt.Errorf("reading trace file: %w", err)
			}

			fmt.Printf("ID:      %s\n", row.ID)
			fmt.Printf("Title:   %s\n", row.Title)
			fmt.Printf("Type:    %s\n", row.Type)
			if row.Author != "" {
				fmt.Printf("Author:  %s\n", row.Author)
			}
			if len(row.Tags) > 0 {
				fmt.Printf("Tags:    %s\n", strings.Join(row.Tags, ", "))
			}
			fmt.Printf("Created: %s\n", row.CreatedAt)
			fmt.Printf("Updated: %s\n", row.UpdatedAt)
			if row.ArchivedAt != "" {
				fmt.Printf("Archived: %s\n", row.ArchivedAt)
			}
			fmt.Println()
			fmt.Println(tr.Body)
			return nil
		},
	}
}
