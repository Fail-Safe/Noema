package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

func similarCmd() *cobra.Command {
	var (
		limit    int
		archived bool
	)

	cmd := &cobra.Command{
		Use:   "similar <trace-id>",
		Short: "Find traces with overlapping vocabulary to a given trace",
		Long: "Surface related traces by FTS5 BM25 ranking against the source trace's most\n" +
			"distinctive vocabulary. No embedding model required — pure SQLite full-text\n" +
			"search.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			matches, err := cx.FindSimilar(args[0], cortex.SimilarOpts{
				Limit:           limit,
				IncludeArchived: archived,
			})
			if err != nil {
				return fmt.Errorf("find similar: %w", err)
			}
			if len(matches) == 0 {
				fmt.Println("No similar traces found.")
				return nil
			}
			printSimilarTable(matches)
			return nil
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 10, "maximum matches to return")
	cmd.Flags().BoolVar(&archived, "archived", false, "include archived traces in results")
	return cmd
}

func printSimilarTable(matches []cortex.SimilarMatch) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SCORE\tID\tTYPE\tTITLE\tTAGS")
	fmt.Fprintln(w, strings.Repeat("-", 6)+"\t"+strings.Repeat("-", 26)+"\t"+strings.Repeat("-", 11)+"\t"+strings.Repeat("-", 28)+"\t"+strings.Repeat("-", 16))
	for _, m := range matches {
		id := m.ID
		if m.ArchivedAt != "" {
			id = "[a] " + id
		}
		fmt.Fprintf(w, "%.2f\t%s\t%s\t%s\t%s\n",
			m.Score,
			id,
			m.Type,
			truncate(m.Title, 28),
			strings.Join(m.Tags, ", "),
		)
	}
	w.Flush()
}
