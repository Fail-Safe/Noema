package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// memoryCmd is the parent for tiering-related admin and observability
// operations. See docs/plans/consolidation-plan.md §11 in the
// Noema-design repo for the design of this subcommand namespace.
func memoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Manage and inspect the memory-tiering feature",
		Long: `Subcommands for the three-tier memory model (short, mid, long).

` + "`noema memory purge`" + ` is the sanctioned ceremonious-delete path for
any trace, including long-term ones the immutability trigger otherwise
guards. Use it for GDPR right-to-erasure requests, accidental promotion
of secrets, or any scenario where raw SQL would normally be reached
for — the ceremony is here to keep federation peers consistent and
the audit trail intact.

` + "`noema memory stats`" + ` reports tier counts so you can see how memory
is distributed across the cortex without opening the DB.`,
	}
	cmd.AddCommand(memoryPurgeCmd(), memoryStatsCmd())
	return cmd
}

func memoryPurgeCmd() *cobra.Command {
	var (
		tierFlag    string
		reasonFlag  string
		confirmFlag bool
		hardFlag    bool
	)
	cmd := &cobra.Command{
		Use:   "purge <trace-id>",
		Short: "Destroy a trace with audit trail and federation propagation",
		Long: `Ceremoniously destroys a trace. Default behaviour tombstones the row
(body wiped to "[purged: <reason>]", file deleted, DB row retained so
lineage references keep resolving) and emits a purge event that
propagates through federation. --hard removes the row and all lineage
references outright for GDPR erasure and similar mandates.

The --tier flag is a safety assertion: it must equal the trace's
actual tier or the command aborts before making any change. This
prevents the classic accident of purging a long-term trace while
thinking it's short-term.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirmFlag {
				return fmt.Errorf("refusing to purge without --confirm (this is intentional friction)")
			}
			if reasonFlag == "" {
				return fmt.Errorf("--reason is required (audit trail needs it)")
			}
			if !trace.IsValidTier(tierFlag) {
				return fmt.Errorf("--tier must be one of short, mid, long")
			}

			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			if err := cx.AdminPurge(args[0], reasonFlag, tierFlag, hardFlag, cortex.ActorHuman); err != nil {
				return err
			}
			verb := "tombstoned"
			if hardFlag {
				verb = "hard-deleted"
			}
			fmt.Printf("Trace %s %s (reason: %s).\n", args[0], verb, reasonFlag)
			return nil
		},
	}
	cmd.Flags().StringVar(&tierFlag, "tier", "", "expected tier (short|mid|long) — safety assertion")
	cmd.Flags().StringVar(&reasonFlag, "reason", "", "free-text audit reason (required)")
	cmd.Flags().BoolVar(&confirmFlag, "confirm", false, "required flag to defeat typo accidents")
	cmd.Flags().BoolVar(&hardFlag, "hard", false, "full row removal and lineage cleanup (default: tombstone)")
	_ = cmd.MarkFlagRequired("tier")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

func memoryStatsCmd() *cobra.Command {
	var outputFlag string
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show tier counts for the active cortex",
		Long: `Reports how many traces currently live in each memory tier, plus the
count of purged tombstones. Archived and trashed traces are excluded
so the numbers reflect memory actively in use.

Later phases will expand this dashboard with consolidation quality
metrics (validation pass rate, cohesion confidence, model-tier
profile performance) as the event log accumulates that data.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			stats, err := cx.TierStats()
			if err != nil {
				return err
			}

			switch outputFlag {
			case "json":
				out, _ := json.MarshalIndent(stats, "", "  ")
				fmt.Println(string(out))
			case "text", "":
				w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintf(w, "Tier\tCount\n")
				fmt.Fprintf(w, "short\t%d\n", stats.Short)
				fmt.Fprintf(w, "mid\t%d\n", stats.Mid)
				fmt.Fprintf(w, "long\t%d\n", stats.Long)
				fmt.Fprintf(w, "purged\t%d\n", stats.Purged)
				w.Flush()
			default:
				return fmt.Errorf("unsupported --output %q (try: text, json)", outputFlag)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&outputFlag, "output", "text", "output format: text, json")
	return cmd
}
