package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

func driftCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Check federated traces for drift from their source hash",
		Long: `Walks all traces with a foreign origin and compares the current body hash
against the source_hash recorded in the frontmatter. Reports traces whose
local content has diverged from the publisher's version.

Only traces with a non-empty source_hash are checked — local-origin traces
and traces from peers that predate the hashing feature are skipped.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			rows, err := cx.List(cortex.ListOptions{All: true})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			var checked, drifted int

			for _, r := range rows {
				if r.Origin == cx.Name || r.Origin == "" {
					continue // local-origin
				}
				if r.SourceHash == "" {
					continue // no source hash to compare against
				}

				path := cx.TraceFile(r.ID, r.ArchivedAt != "")
				t, err := trace.ParseFile(path)
				if err != nil {
					fmt.Fprintf(out, "  SKIP     %s (parse error: %v)\n", r.ID, err)
					continue
				}

				checked++
				current := trace.ContentHash(t.Body)
				if current != r.SourceHash {
					drifted++
					lock := "no"
					if r.SourceLocked {
						lock = "yes"
					}
					fmt.Fprintf(out, "  DRIFTED  %s (source: %s, locked: %s)\n", r.ID, r.Origin, lock)
					fmt.Fprintf(out, "           local:  %s\n", current)
					fmt.Fprintf(out, "           source: %s\n", r.SourceHash)
				}
			}

			fmt.Fprintf(out, "\nChecked %d federated trace(s).\n", checked)
			if drifted > 0 {
				fmt.Fprintf(out, "%d trace(s) have drifted from their source.\n", drifted)
			} else if checked > 0 {
				fmt.Fprintln(out, "No drift detected.")
			} else {
				fmt.Fprintln(out, "No federated traces with source hashes found.")
			}
			return nil
		},
	}
	return cmd
}
