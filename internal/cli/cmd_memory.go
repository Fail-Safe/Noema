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
	cmd.AddCommand(
		memoryPurgeCmd(),
		memoryStatsCmd(),
		memoryPromoteCmd(),
		memoryDemoteCmd(),
	)
	return cmd
}

// memoryPromoteCmd surfaces Cortex.Promote as an operator-facing CLI
// verb. The underlying Go function has always supported short→mid and
// mid→long; this command closes the gap where only automatic pathways
// (heuristic promoter, LLM distillation, Phase 15 graduation) could
// reach the long tier. Useful for explicit curation: "this trace is a
// base truth, lock it now" without waiting for the stability bar.
func memoryPromoteCmd() *cobra.Command {
	var toFlag string
	cmd := &cobra.Command{
		Use:   "promote <trace-id>",
		Short: "Advance a trace one tier (short→mid or mid→long)",
		Long: `Promotes the referenced trace to the next memory tier. With no
--to flag, advances by one: short→mid if currently short, mid→long if
currently mid. Pass --to explicitly to assert the target tier as a
safety check.

Emits ActionPromote, which propagates through federation so peers
reach the same tier state on their next sync.

This is the explicit curation path. The automatic path (scheduler's
graduation pass) waits for the stability criteria configured under
` + "`consolidation.graduation`" + ` in cortex.md; use the CLI when you're
confident a trace should be a base truth today.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			row, err := cx.Get(args[0])
			if err != nil {
				return err
			}

			target := toFlag
			if target == "" {
				switch row.Tier {
				case trace.TierShort:
					target = trace.TierMid
				case trace.TierMid:
					target = trace.TierLong
				case trace.TierLong:
					return fmt.Errorf("trace %s is already at long tier (terminal)", args[0])
				default:
					return fmt.Errorf("trace %s is at unknown tier %q", args[0], row.Tier)
				}
			}
			if !trace.IsValidTier(target) {
				return fmt.Errorf("--to must be one of short, mid, long")
			}

			if err := cx.Promote(args[0], target); err != nil {
				return err
			}
			fmt.Printf("Trace %s promoted %s → %s.\n", args[0], row.Tier, target)
			return nil
		},
	}
	cmd.Flags().StringVar(&toFlag, "to", "", "target tier (mid or long); default = next tier up")
	return cmd
}

// memoryDemoteCmd surfaces Cortex.Demote. Only mid → short is allowed
// in routine operation — long-term demotion is the admin-purge path
// (`noema memory purge`) because undoing a base truth should carry the
// same friction as destroying it.
func memoryDemoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "demote <trace-id>",
		Short: "Step a trace back a tier (mid→short only)",
		Long: `Demotes the referenced trace from mid back to short. Long-tier
demotion is deliberately not a CLI verb: once a trace is etched into
the base-truths layer, removing it goes through ` + "`noema memory purge`" + `
so the admin-purge ceremony produces the same audit trail a destructive
change deserves.

Emits ActionDemote, which propagates through federation.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			row, err := cx.Get(args[0])
			if err != nil {
				return err
			}
			if row.Tier == trace.TierLong {
				return fmt.Errorf(
					"trace %s is at long tier — long-term demotion goes through `noema memory purge` (with --hard if you want full removal)",
					args[0],
				)
			}
			if err := cx.Demote(args[0], trace.TierShort); err != nil {
				return err
			}
			fmt.Printf("Trace %s demoted %s → %s.\n", args[0], row.Tier, trace.TierShort)
			return nil
		},
	}
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
