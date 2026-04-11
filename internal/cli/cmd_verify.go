package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/trace"
)

func verifyCmd() *cobra.Command {
	var backfill bool

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Check trace content hashes for integrity",
		Long: `Walks all trace files (active and archived), recomputes the SHA-256 hash
of each body, and compares it against the content_hash in the frontmatter.
Reports any mismatches, which indicate the file was modified outside of Noema.

Use --backfill to populate content_hash for traces that predate the hashing
feature (or were created by an older binary).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			dirs := []string{cx.TracesDir(), cx.ArchiveDir()}
			var checked, mismatches, backfilled int

			for _, dir := range dirs {
				files, err := os.ReadDir(dir)
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					return fmt.Errorf("reading %s: %w", dir, err)
				}
				for _, f := range files {
					if f.IsDir() || filepath.Ext(f.Name()) != ".md" {
						continue
					}
					path := filepath.Join(dir, f.Name())
					t, err := trace.ParseFile(path)
					if err != nil {
						fmt.Fprintf(cmd.OutOrStdout(), "  SKIP     %s (parse error: %v)\n", f.Name(), err)
						continue
					}

					computed := trace.ContentHash(t.Body)
					checked++

					if t.ContentHash == "" {
						if backfill {
							t.ContentHash = computed
							if err := t.Write(path); err != nil {
								return fmt.Errorf("backfilling %s: %w", t.ID, err)
							}
							backfilled++
							fmt.Fprintf(cmd.OutOrStdout(), "  BACKFILL %s\n", t.ID)
						}
						continue
					}

					if t.ContentHash != computed {
						mismatches++
						fmt.Fprintf(cmd.OutOrStdout(), "  MISMATCH %s\n", t.ID)
						fmt.Fprintf(cmd.OutOrStdout(), "           expected: %s\n", t.ContentHash)
						fmt.Fprintf(cmd.OutOrStdout(), "           actual:   %s\n", computed)
						if backfill {
							t.ContentHash = computed
							if err := t.Write(path); err != nil {
								return fmt.Errorf("backfilling %s: %w", t.ID, err)
							}
							backfilled++
							fmt.Fprintf(cmd.OutOrStdout(), "  FIXED    %s\n", t.ID)
						}
					}
				}
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "\nChecked %d trace(s).\n", checked)
			if mismatches > 0 {
				fmt.Fprintf(out, "%d mismatch(es) found.\n", mismatches)
			} else {
				fmt.Fprintln(out, "All hashes OK.")
			}
			if backfill && backfilled > 0 {
				fmt.Fprintf(out, "%d trace(s) backfilled.\n", backfilled)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&backfill, "backfill", false, "write content_hash into traces that are missing it")
	return cmd
}
