package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

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
		memoryHealthCmd(),
		memoryPopularCmd(),
		memoryPromoteCmd(),
		memoryDemoteCmd(),
	)
	return cmd
}

// memoryPopularReport is the JSON envelope shared between the CLI
// (`noema memory popular --output json`) and the MCP tool
// `search_activity`. Same schema_version namespace as memoryHealthReport
// — separate report types intentionally so consumers can pin to one
// without coupling to the other.
type memoryPopularReport struct {
	SchemaVersion int                   `json:"schema_version"`
	Top           int                   `json:"top"`
	Traces        []cortex.PopularTrace `json:"traces"`
	Tags          []cortex.TagSummary   `json:"tags"`
}

func memoryPopularCmd() *cobra.Command {
	var (
		topFlag    int
		outputFlag string
	)
	cmd := &cobra.Command{
		Use:   "popular",
		Short: "Top traces by search popularity and top tags by aggregate engagement",
		Long: `Reports two leaderboards over the cortex's federation-wide engagement
counters:

  - Top-N traces by search_hit_count (primary) and read_count
    (tiebreaker). search_hit_count is the auto-injection-friendly
    signal — Hermes-style providers that fold search results into a
    context window without ever calling get_trace bump this counter
    but not read_count, so it's the better "what's worth surfacing"
    proxy than reads alone.

  - Top-N tags by aggregate search hits / reads / modifies across
    every active trace carrying the tag. A trace with multiple tags
    contributes its engagement to each tag, so two columns are
    directly comparable.

Cumulative all-time counters — no --since flag. The trace_usage rows
don't carry timestamps so a window filter would need a different
table (per-day usage snapshots), which is deferred to a future
iteration.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()
			return runMemoryPopular(cmd.OutOrStdout(), cx, topFlag, outputFlag)
		},
	}
	cmd.Flags().IntVar(&topFlag, "top", 10, "how many top traces and top tags to return")
	cmd.Flags().StringVar(&outputFlag, "output", "text", "output format: text, json")
	return cmd
}

func runMemoryPopular(w io.Writer, cx *cortex.Cortex, top int, output string) error {
	if top <= 0 {
		top = 10
	}
	traces, err := cx.TopSearchedTraces(top)
	if err != nil {
		return fmt.Errorf("top searched traces: %w", err)
	}
	tags, err := cx.TagActivity(top)
	if err != nil {
		return fmt.Errorf("tag activity: %w", err)
	}
	report := memoryPopularReport{
		SchemaVersion: 1,
		Top:           top,
		Traces:        traces,
		Tags:          tags,
	}
	switch output {
	case "json":
		buf, _ := json.MarshalIndent(report, "", "  ")
		fmt.Fprintln(w, string(buf))
	case "text", "":
		renderMemoryPopularText(w, report)
	default:
		return fmt.Errorf("unsupported --output %q (try: text, json)", output)
	}
	return nil
}

func renderMemoryPopularText(w io.Writer, r memoryPopularReport) {
	fmt.Fprintf(w, "Top %d traces by search popularity\n", r.Top)
	if len(r.Traces) == 0 {
		fmt.Fprintln(w, "  (no traces with engagement yet)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  Hits\tReads\tTier\tType\tTitle")
		for _, p := range r.Traces {
			fmt.Fprintf(tw, "  %d\t%d\t%s\t%s\t%s\n", p.SearchHits, p.ReadCount, p.Tier, p.Type, truncate(p.Title, 60))
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "Top %d tags by aggregate engagement\n", r.Top)
	if len(r.Tags) == 0 {
		fmt.Fprintln(w, "  (no tagged traces with engagement yet)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  Tag\tTraces\tHits\tReads\tModifies")
	for _, t := range r.Tags {
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\t%d\n", t.Tag, t.TraceCount, t.SearchHits, t.ReadCount, t.ModifyCount)
	}
	tw.Flush()
}


// memoryHealthReport wraps the three cortex methods that answer "is
// consolidation actually doing anything, and is anything leaking?"
// into a single JSON envelope with a top-level schema_version (design
// doc §3 — single top-level version per output, not per-section). The
// MCP `consolidation_health` tool returns the same shape modulo field
// naming.
type memoryHealthReport struct {
	SchemaVersion int                          `json:"schema_version"`
	Activity      cortex.ConsolidationActivity `json:"activity"`
	Latency       cortex.PromotionLatency      `json:"latency"`
	OneSourceMid  cortex.OneSourceMidCount     `json:"one_source_mid"`
}

func memoryHealthCmd() *cobra.Command {
	var (
		sinceFlag  string
		outputFlag string
	)
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Show consolidation activity, promotion latency, and the 1-source mid leak detector",
		Long: `Reports the consolidation pipeline's recent behavior — the question
that motivated the observability surface: "how did consolidation fare
overnight, and is anything leaking?"

Three sections:

  - Activity over the --since window: per-day counts of consolidation_claim,
    consolidation_success, consolidation_fail, promote (any tier transition),
    and consolidate (real LLM distillation events). Totals roll up the same
    counters across the window.

  - Promotion latency (all-time): count and p50/p95 of short→mid and
    mid→long transition durations. mid→long measures from when the trace
    entered the mid tier, not its created_at — a trace that lives short
    for 30 days then promotes mid→long the next day shows up here as a
    1-day mid→long latency.

  - 1-source mid leak detector: current count of active mid-tier traces
    with derived_from_count == 1 (the bucket PR #86 and PR #90 closed),
    plus the count of traces that landed in that bucket within the last
    7 days. A healthy gate keeps the recent count at zero.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()
			return runMemoryHealth(cmd.OutOrStdout(), cx, sinceFlag, outputFlag)
		},
	}
	cmd.Flags().StringVar(&sinceFlag, "since", "24h", "lookback window for activity buckets (e.g. 24h, 7d, 2w)")
	cmd.Flags().StringVar(&outputFlag, "output", "text", "output format: text, json")
	return cmd
}

// runMemoryHealth is the testable core of the memory-health CLI: it
// composes the three cortex observability calls into a single report
// and dispatches to either JSON or text rendering. Split out from the
// cobra RunE so unit tests can drive it with a constructed Cortex and
// a captured writer without spinning up the full command tree.
func runMemoryHealth(w io.Writer, cx *cortex.Cortex, sinceLabel, output string) error {
	since, err := cortex.ParseSince(sinceLabel)
	if err != nil {
		return err
	}
	activity, err := cx.ConsolidationActivity(since)
	if err != nil {
		return fmt.Errorf("consolidation activity: %w", err)
	}
	latency, err := cx.PromotionLatency()
	if err != nil {
		return fmt.Errorf("promotion latency: %w", err)
	}
	leak, err := cx.OneSourceMidCount()
	if err != nil {
		return fmt.Errorf("one-source mid count: %w", err)
	}

	report := memoryHealthReport{
		SchemaVersion: 1,
		Activity:      activity,
		Latency:       latency,
		OneSourceMid:  leak,
	}

	switch output {
	case "json":
		buf, _ := json.MarshalIndent(report, "", "  ")
		fmt.Fprintln(w, string(buf))
	case "text", "":
		renderMemoryHealthText(w, report, sinceLabel)
	default:
		return fmt.Errorf("unsupported --output %q (try: text, json)", output)
	}
	return nil
}

// renderMemoryHealthText formats the report for a terminal. Two
// tabwriter blocks (activity table + totals; latency rows) and a
// final paragraph for the leak detector. Tabwriter rather than
// hand-aligned spaces keeps the columns clean when day counts grow
// to 3-digit numbers under busy cortexes.
func renderMemoryHealthText(w io.Writer, r memoryHealthReport, sinceLabel string) {
	fmt.Fprintf(w, "Consolidation activity — last %s\n", sinceLabel)
	if len(r.Activity.Daily) == 0 {
		fmt.Fprintln(w, "  (no events in window)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  Date\tClaim\tSuccess\tFail\tPromote\tDistill")
		for _, d := range r.Activity.Daily {
			fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\t%d\t%d\n",
				d.Date, d.Claim, d.Success, d.Fail, d.Promote, d.Distill)
		}
		fmt.Fprintf(tw, "  ----\t-----\t-------\t----\t-------\t-------\n")
		fmt.Fprintf(tw, "  Total\t%d\t%d\t%d\t%d\t%d\n",
			r.Activity.Totals.Claim, r.Activity.Totals.Success, r.Activity.Totals.Fail,
			r.Activity.Totals.Promote, r.Activity.Totals.Distill)
		tw.Flush()
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Promotion latency (all-time)")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  Transition\tCount\tp50\tp95")
	fmt.Fprintf(tw, "  short→mid\t%d\t%s\t%s\n",
		r.Latency.ShortToMid.Count, formatDuration(r.Latency.ShortToMid.P50), formatDuration(r.Latency.ShortToMid.P95))
	fmt.Fprintf(tw, "  mid→long\t%d\t%s\t%s\n",
		r.Latency.MidToLong.Count, formatDuration(r.Latency.MidToLong.P50), formatDuration(r.Latency.MidToLong.P95))
	tw.Flush()
	fmt.Fprintln(w)

	fmt.Fprintln(w, "1-source mid leak detector")
	fmt.Fprintf(w, "  Current:           %d trace(s)\n", r.OneSourceMid.Current)
	status := "✓ gate is holding"
	if r.OneSourceMid.PromotedLast7d > 0 {
		status = "⚠ recent leak — investigate"
	}
	fmt.Fprintf(w, "  Promoted last 7d:  %d  %s\n", r.OneSourceMid.PromotedLast7d, status)
}

// formatDuration renders a duration in days/hours/minutes/seconds for
// operator-friendly text output. JSON output keeps the raw nanosecond
// integer so scripts can do whatever bucketing they want.
func formatDuration(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	if d >= 24*time.Hour {
		return fmt.Sprintf("%.1fd", float64(d)/float64(24*time.Hour))
	}
	if d >= time.Hour {
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	if d >= time.Minute {
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
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

// statsReport bundles every metric the stats command can surface.
// Flat structure so JSON output stays predictable and scriptable —
// downstream tooling can grep for specific fields without descending
// into nested objects.
type statsReport struct {
	Tiers      cortex.TierStats             `json:"tiers"`
	Engagement *cortex.EngagementStats      `json:"engagement,omitempty"`
	Lineage    *cortex.MidLineageBreakdown  `json:"mid_lineage,omitempty"`
	MidHealth  *cortex.MidEngagementSnapshot `json:"mid_engagement,omitempty"`
}

func memoryStatsCmd() *cobra.Command {
	var outputFlag string
	var detailedFlag bool
	var zeroEngagementAgeDays int
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show tier counts and (with --detailed) engagement signal for the active cortex",
		Long: `Reports how many traces currently live in each memory tier, plus the
count of purged tombstones. Archived and trashed traces are excluded
so the numbers reflect memory actively in use.

Pass --detailed to add the engagement dashboard:

  - total reads / search hits / modifies across active traces (the
    federation-wide signal the consolidation heuristic and graduation
    gate evaluate against)
  - mid-tier lineage breakdown: 0 sources / 1 source / >=2 sources.
    Real consolidations live in the >=2 bucket; a growing 1-source
    bucket usually indicates a writeback pattern emitting "summary"
    traces that aren't really summarising.
  - mid-tier zero-engagement count: traces with no reads, no search
    hits, no modifies. The older-than-14d subset is a candidate pool
    for archival — the graduation min-age is the same threshold so
    these traces had a fair chance to accumulate signal.

Later phases will expand this dashboard with consolidation quality
metrics (validation pass rate, cohesion confidence, model-tier
profile performance) as the event log accumulates that data.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			tiers, err := cx.TierStats()
			if err != nil {
				return err
			}
			report := statsReport{Tiers: tiers}

			if detailedFlag {
				eng, err := cx.EngagementStats()
				if err != nil {
					return fmt.Errorf("engagement stats: %w", err)
				}
				lineage, err := cx.MidLineageBreakdown()
				if err != nil {
					return fmt.Errorf("lineage breakdown: %w", err)
				}
				olderThan := time.Duration(zeroEngagementAgeDays) * 24 * time.Hour
				health, err := cx.MidEngagementSnapshot(olderThan)
				if err != nil {
					return fmt.Errorf("mid engagement snapshot: %w", err)
				}
				report.Engagement = &eng
				report.Lineage = &lineage
				report.MidHealth = &health
			}

			switch outputFlag {
			case "json":
				out, _ := json.MarshalIndent(report, "", "  ")
				fmt.Println(string(out))
			case "text", "":
				w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintf(w, "Tier\tCount\n")
				fmt.Fprintf(w, "short\t%d\n", report.Tiers.Short)
				fmt.Fprintf(w, "mid\t%d\n", report.Tiers.Mid)
				fmt.Fprintf(w, "long\t%d\n", report.Tiers.Long)
				fmt.Fprintf(w, "purged\t%d\n", report.Tiers.Purged)
				w.Flush()

				if detailedFlag {
					fmt.Fprintln(cmd.OutOrStdout())
					ew := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
					fmt.Fprintf(ew, "Signal\tTotal\n")
					fmt.Fprintf(ew, "reads\t%d\n", report.Engagement.TotalReads)
					fmt.Fprintf(ew, "search hits\t%d\n", report.Engagement.TotalSearchHits)
					fmt.Fprintf(ew, "modifies\t%d\n", report.Engagement.TotalModifies)
					ew.Flush()

					fmt.Fprintln(cmd.OutOrStdout())
					lw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
					fmt.Fprintf(lw, "Mid lineage\tCount\n")
					fmt.Fprintf(lw, "no derived_from\t%d\n", report.Lineage.NoSources)
					fmt.Fprintf(lw, "1 source\t%d\n", report.Lineage.SingleSource)
					fmt.Fprintf(lw, ">=2 sources\t%d\n", report.Lineage.MultiSource)
					lw.Flush()

					fmt.Fprintln(cmd.OutOrStdout())
					fmt.Fprintf(cmd.OutOrStdout(),
						"Mid traces with zero engagement: %d (%d older than %dd)\n",
						report.MidHealth.ZeroEngagement,
						report.MidHealth.ZeroEngagementOlder,
						zeroEngagementAgeDays,
					)
				}
			default:
				return fmt.Errorf("unsupported --output %q (try: text, json)", outputFlag)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&outputFlag, "output", "text", "output format: text, json")
	cmd.Flags().BoolVar(&detailedFlag, "detailed", false, "include engagement signal, lineage breakdown, and zero-engagement counts")
	cmd.Flags().IntVar(&zeroEngagementAgeDays, "zero-engagement-age-days", 14, "age threshold (days) for the zero-engagement-older count under --detailed")
	return cmd
}
