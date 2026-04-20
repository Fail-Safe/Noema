package cortex

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// ErrDistillSourcesInsufficient is returned when a caller tries to
// record a consolidation result with fewer than two source traces.
// Below that threshold the operation is a rename, not a
// consolidation — the event log records the category via the action
// constant, so this constraint protects the distinction.
var ErrDistillSourcesInsufficient = errors.New("distilled trace requires >= 2 source IDs")

// ErrDistillSourceMissing is returned when one of the declared source
// trace IDs does not exist in the cortex. Keeps the derived_from
// lineage honest — a consolidation event that points at a ghost trace
// breaks retrospective source-recall queries downstream.
var ErrDistillSourceMissing = errors.New("source trace not found")

// DistilledTraceSpec is the payload record_consolidation_result
// (Phase 9 MCP tool) hands to Cortex. The fields mirror the event
// data schema in docs/plans/consolidation-plan.md §9 so event
// replay on a federation peer can reconstruct the same distillation.
type DistilledTraceSpec struct {
	Title              string
	Body               string
	Tags               []string
	Author             string
	SourceIDs          []string
	ModelName          string
	ModelTierProfile   string
	CohesionConfidence float64
}

// CreateDistilledTrace materialises the result of an LLM-driven
// consolidation pass: a new mid-tier trace whose derived_from lineage
// points at the short-term sources that fed the distillation. Emits
// ActionConsolidate alongside the standard ActionCreate so the event
// log carries both "a new trace exists" and "this is why it exists /
// which model produced it / how confident" as separate, replayable
// records.
//
// Source handling (v1 — "net-add with source promotion"): every
// short-tier source is promoted to mid once the distillation lands.
// The distilled trace is the retrievable summary; sources remain
// individually addressable at mid so agents can pull the full
// detail via derived_from when the distillation's compression is
// lossy. This also prevents the next consolidation pass from
// re-clustering the same sources into a duplicate distillation —
// the candidate query filters to tier='short', so promoted sources
// drop out of the candidate pool.
//
// FUTURE (v2+): "source archival" — instead of promoting sources
// to mid, move them to archived once the distillation is trusted
// (confidence threshold + retention window). That keeps mid-tier
// curated and closer to the biological metaphor where the detailed
// memory fades as the distillation takes its place. Not v1 because
// the grace-period machinery adds a new daemon and needs a
// confidence-calibration pass first. The current design's derived_from
// lineage already supports the retrieval path that option 2 would
// need, so the switch is local to this function + a new archival
// sweep — no schema changes.
func (c *Cortex) CreateDistilledTrace(spec DistilledTraceSpec) (string, error) {
	if spec.Title == "" {
		return "", fmt.Errorf("distilled trace: title is required")
	}
	if spec.Body == "" {
		return "", fmt.Errorf("distilled trace: body is required")
	}
	if len(spec.SourceIDs) < 2 {
		return "", ErrDistillSourcesInsufficient
	}
	// Verify every declared source exists. The row reads go outside the
	// CreateDistilledTrace transaction, but that's fine: a source getting
	// deleted between this check and the Add below would surface as a
	// foreign-key-like failure at commit time via trace_lineage, which
	// references traces(id). The check here is just to return a nicer
	// error for the common misconfiguration case.
	for _, id := range spec.SourceIDs {
		if _, err := c.Get(id); err != nil {
			return "", fmt.Errorf("%w: %s", ErrDistillSourceMissing, id)
		}
	}

	t := trace.New(spec.Title, string(trace.TypeObservation), spec.Author, spec.Tags, spec.Body)
	t.Tier = trace.TierMid
	t.DerivedFrom = append([]string{}, spec.SourceIDs...)
	if err := c.Add(t); err != nil {
		return "", fmt.Errorf("persisting distilled trace: %w", err)
	}

	// ActionConsolidate sits alongside the ActionCreate that Add
	// emitted. A separate transaction is fine: a missing
	// ActionConsolidate event (if this Exec fails) does not corrupt
	// the trace itself; it only loses the quality-telemetry signal,
	// and the ActionCreate event still records the new trace's
	// appearance for federation replay.
	now := time.Now().UTC().Format(time.RFC3339)
	data, _ := json.Marshal(struct {
		SourceIDs          []string `json:"source_ids"`
		DistilledID        string   `json:"distilled_id"`
		ModelName          string   `json:"model_name,omitempty"`
		ModelTierProfile   string   `json:"model_tier_profile,omitempty"`
		CohesionConfidence float64  `json:"cohesion_confidence,omitempty"`
	}{
		SourceIDs:          spec.SourceIDs,
		DistilledID:        t.ID,
		ModelName:          spec.ModelName,
		ModelTierProfile:   spec.ModelTierProfile,
		CohesionConfidence: spec.CohesionConfidence,
	})

	tx, err := c.DB.Begin()
	if err != nil {
		return t.ID, fmt.Errorf("opening consolidation-event tx: %w", err)
	}
	defer tx.Rollback()
	if err := c.emitEvent(tx, event.ActionConsolidate, t.ID, now, data); err != nil {
		return t.ID, fmt.Errorf("emitting consolidate event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return t.ID, fmt.Errorf("committing consolidate event: %w", err)
	}

	// Promote each source from short to mid. Best-effort per source —
	// the distillation itself has already landed, so a partial
	// promotion failure doesn't invalidate anything, it just leaves
	// one or more sources at short. They'd surface again in the next
	// consolidation pass, which is suboptimal but self-correcting.
	// Sources that are not at 'short' are skipped quietly: they may
	// already have been promoted by a previous run (re-consolidation
	// after a crash), or they're at a tier this function shouldn't
	// touch (mid/long — isValidPromotion would reject anyway).
	for _, sid := range spec.SourceIDs {
		srow, err := c.Get(sid)
		if err != nil {
			// Source vanished between the earlier existence check and
			// now — log and skip rather than abort the whole operation.
			// The distilled trace is already committed; lineage is
			// intact.
			continue
		}
		if srow.Tier != trace.TierShort {
			continue
		}
		if err := c.Promote(sid, trace.TierMid); err != nil {
			// Non-fatal per the "best-effort" contract. The ActionPromote
			// event is what keeps federation peers consistent; if this
			// fails here it fails everywhere, and the next run will
			// retry.
			continue
		}
	}

	return t.ID, nil
}
