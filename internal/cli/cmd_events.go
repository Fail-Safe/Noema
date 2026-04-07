package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

func eventsCmd() *cobra.Command {
	var (
		all   bool
		since string
		limit int
	)

	cmd := &cobra.Command{
		Use:   "events [trace-id]",
		Short: "Show the event log (audit trail)",
		Long: `Without arguments, shows recent events across all traces.
With a trace ID, shows all events for that specific trace.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completionsFor(cortex.ListOptions{All: true}),
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			if len(args) == 1 {
				events, err := cx.Events(args[0])
				if err != nil {
					return err
				}
				if len(events) == 0 {
					fmt.Println("No events found.")
					return nil
				}
				for _, e := range events {
					fmt.Printf("%s  %-10s  %s  origin=%s\n", e.ID, e.Action, e.Timestamp, e.Origin)
				}
				return nil
			}

			// All events with pagination.
			if limit <= 0 {
				limit = 50
			}
			events, err := cx.EventsSince(since, limit)
			if err != nil {
				return err
			}
			if len(events) == 0 {
				fmt.Println("No events found.")
				return nil
			}
			for _, e := range events {
				fmt.Printf("%s  %-10s  %-40s  %s  origin=%s\n", e.ID, e.Action, e.TraceID, e.Timestamp, e.Origin)
			}
			if len(events) == limit {
				fmt.Printf("\n(showing %d events — use --since %s to see more)\n", limit, events[len(events)-1].ID)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "show events across all traces (default when no trace-id given)")
	cmd.Flags().StringVar(&since, "since", "", "show events after this ULID cursor")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum number of events to show")

	cmd.AddCommand(eventsBackfillCmd())
	return cmd
}

func eventsBackfillCmd() *cobra.Command {
	var (
		assumeYes bool
		dryRun    bool
	)

	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Synthesize create events for active traces missing one",
		Long: `Walks the local trace table and emits a synthetic 'create' event for
every active trace that does not yet have one in the event log. This is the
recovery path for traces that pre-date the event log or that landed via
` + "`noema sync`" + ` (which intentionally emits no events because reconciliation
is not a semantic mutation).

Once backfilled, those traces propagate to federation peers on the next
sync poll just like any normal create. Each backfill event is stamped with
the current wall-clock time and a fresh ULID — the trace's own 'created'
frontmatter field is left untouched, so the audit trail still shows the
real authorship date.

Archived and trashed traces are skipped: a create-only event for them
would diverge federation (peers would materialise the trace as active).
Recover or unarchive the trace first if it needs to federate.

The iteration is idempotent — running this twice in a row is a no-op on
the second pass because the SQL filter excludes traces that already have
a create event in the log (whether locally emitted or replayed).`,
		Example: "  noema events backfill --dry-run\n  noema events backfill --yes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			return runEventsBackfill(cmd.OutOrStdout(), cmd.InOrStdin(), cx, dryRun, assumeYes)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print which traces would be backfilled without writing events")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

// runEventsBackfill is split out so tests can drive it without going through
// cobra. The dry-run path runs the same iteration as the real backfill so the
// preview reflects exactly what the operator would commit, with no chance of
// the two queries drifting apart.
func runEventsBackfill(out io.Writer, in io.Reader, cx *cortex.Cortex, dryRun, assumeYes bool) error {
	// Preview pass — always runs first so the operator sees the candidate
	// list and skipped IDs *before* anything is written. The result of the
	// dry-run pass is also what we hand back when --dry-run is set.
	preview, err := cx.BackfillCreateEvents(true)
	if err != nil {
		return fmt.Errorf("scanning candidate traces: %w", err)
	}

	if len(preview.BackfilledIDs) == 0 && len(preview.SkippedIDs) == 0 {
		fmt.Fprintln(out, "Nothing to backfill — every trace already has a create event in the log.")
		return nil
	}

	fmt.Fprintf(out, "Cortex %q: %d trace(s) would receive a backfill create event.\n", cx.Name, len(preview.BackfilledIDs))
	for _, id := range preview.BackfilledIDs {
		fmt.Fprintf(out, "  + %s\n", id)
	}
	if len(preview.SkippedIDs) > 0 {
		fmt.Fprintf(out, "\n%d archived/trashed trace(s) lack a create event but will be skipped\n", len(preview.SkippedIDs))
		fmt.Fprintln(out, "(recover or unarchive them first if they need to federate):")
		for _, id := range preview.SkippedIDs {
			fmt.Fprintf(out, "  - %s\n", id)
		}
	}

	if dryRun {
		fmt.Fprintln(out, "\n(dry run — no events written)")
		return nil
	}

	if len(preview.BackfilledIDs) == 0 {
		// Nothing actionable; the skipped list was already shown above so
		// the operator knows why. Bail without prompting for a no-op.
		fmt.Fprintln(out, "\nNo active traces to backfill.")
		return nil
	}

	if !assumeYes {
		fmt.Fprint(out, "\nProceed? [y/N]: ")
		var resp string
		_, _ = fmt.Fscanln(in, &resp)
		if resp != "y" && resp != "Y" && resp != "yes" {
			return fmt.Errorf("aborted by user")
		}
	}

	result, err := cx.BackfillCreateEvents(false)
	if err != nil {
		return fmt.Errorf("backfilling events: %w", err)
	}

	fmt.Fprintf(out, "\nBackfilled %d create event(s). Peers will replay them on the next sync poll.\n", len(result.BackfilledIDs))
	return nil
}
