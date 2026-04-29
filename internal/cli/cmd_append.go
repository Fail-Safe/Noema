package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

// appendCmd wraps cortex.Append for the human-at-terminal and pipe-
// from-script use cases. The MCP `append_trace` tool already exposes
// the same operation to agents; this fills the matching CLI gap so a
// shell user can run `echo "..." | noema append <id>` without
// reaching for `edit` (which fires up $EDITOR and overshoots the use
// case).
//
// Why not under `noema memory`: the memory subcommand group is for
// tiering admin and observability (promote / demote / purge / stats).
// Append is a body mutation on an existing trace, conceptually
// peers with `add` (new trace) and `edit` (open in $EDITOR), so it
// belongs at top level alongside them.
func appendCmd() *cobra.Command {
	var (
		content string
		force   bool
	)

	cmd := &cobra.Command{
		Use:   "append <id>",
		Short: "Append content to an existing Trace's body",
		Long: `Append content to an existing Trace's body. Pipe-friendly:
useful for running journals, fire-and-forget logging, and any case where
another process needs to add a line or two to a Trace without reading
the full current body first.

Content sources, in priority order:
  --content "text"                       one-liner via flag
  echo "text" | noema append <id>        piped from stdin (no flag needed)
  noema append <id>                      interactive: type, Ctrl+D to save

A newline is inserted between the existing body and the appended
content if the existing body doesn't end with one. The cortex emits an
update event for the append, so the change replicates through
federation like any other body edit.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completionsFor(cortex.ListOptions{}),
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			id := args[0]

			// Resolve content from flag, pipe, or interactive prompt.
			// Flag wins; otherwise we sniff stdin's mode so a piped
			// invocation reads silently while a tty invocation prints
			// a prompt and waits for Ctrl+D — same convention as
			// `noema add` for body input.
			if content == "" && !cmd.Flags().Changed("content") {
				fi, _ := os.Stdin.Stat()
				piped := (fi.Mode() & os.ModeCharDevice) == 0
				if !piped {
					fmt.Println("Content to append (Ctrl+D to save, Ctrl+C to cancel):")
				}
				data, err := readBodyFromStdin()
				if err != nil {
					return err
				}
				content = data
			}
			if content == "" {
				return fmt.Errorf("no content provided to append")
			}

			// Source-lock bypass — mirrors the edit/archive convention
			// so users have one consistent override across mutation
			// commands. Only takes effect for THIS process's call to
			// Append; it does not persist.
			if force {
				cx.SetForceSourceLock(true)
			}

			if err := cx.Append(id, content); err != nil {
				if errors.Is(err, cortex.ErrSourceLocked) {
					return fmt.Errorf("%w (use --force to override)", err)
				}
				return err
			}
			fmt.Printf("Appended to trace %s.\n", id)
			return nil
		},
	}
	cmd.Flags().StringVar(&content, "content", "",
		"content to append (use stdin or interactive prompt if omitted)")
	cmd.Flags().BoolVarP(&force, "force", "f", false,
		"bypass source-lock protection")
	return cmd
}
