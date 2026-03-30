package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"os"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

func listCmd() *cobra.Command {
	var (
		filterType   string
		filterAuthor string
		filterTag    string
		archived     bool
		trashed      bool
		all          bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Traces",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			rows, err := cx.List(cortex.ListOptions{
				Type:     filterType,
				Author:   filterAuthor,
				Tag:      filterTag,
				Archived: archived,
				Trashed:  trashed,
				All:      all,
			})
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				fmt.Println("No traces found.")
				return nil
			}
			printTable(rows)
			return nil
		},
	}

	cmd.Flags().StringVar(&filterType, "type", "", "filter by type")
	cmd.Flags().StringVar(&filterAuthor, "author", "", "filter by author")
	cmd.Flags().StringVar(&filterTag, "tag", "", "filter by tag")
	cmd.Flags().BoolVar(&archived, "archived", false, "show only archived traces")
	cmd.Flags().BoolVar(&trashed, "trashed", false, "show only trashed traces")
	cmd.Flags().BoolVar(&all, "all", false, "show active and archived traces")
	return cmd
}

func printTable(rows []cortex.Row) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tTYPE\tAUTHOR\tTAGS\tCREATED")
	fmt.Fprintln(w, strings.Repeat("-", 26)+"\t"+strings.Repeat("-", 28)+"\t"+strings.Repeat("-", 11)+"\t"+strings.Repeat("-", 12)+"\t"+strings.Repeat("-", 16)+"\t"+strings.Repeat("-", 10))
	for _, r := range rows {
		created := r.CreatedAt
		if len(created) > 10 {
			created = created[:10]
		}
		id := r.ID
		if r.ArchivedAt != "" {
			id = "[a] " + id
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			id,
			truncate(r.Title, 28),
			r.Type,
			r.Author,
			strings.Join(r.Tags, ", "),
			created,
		)
	}
	w.Flush()
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
