package cli

import (
	"fmt"

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
	return cmd
}
