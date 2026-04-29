package consolidation_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/consolidation"
)

// Pins the swallow-on-error contract that makes auto-distillation safe
// to compose in front of heuristic + graduation. The scheduled agent
// must never abort its chained pass because the LLM endpoint happens
// to be offline — the cheap maintenance work still needs to run.

func TestDistillationPass_SwallowsEmptyEndpoint(t *testing.T) {
	// NewHTTPLLMClient rejects an empty endpoint outright; the returned
	// PassFn must log and return nil rather than propagating.
	cx := setupCortex(t)
	var logged []string
	logger := func(format string, args ...any) {
		logged = append(logged, formatLine(format, args...))
	}

	pass := consolidation.DistillationPass(cx, consolidation.PipelineConfig{
		Window:    time.Hour,
		ModelTier: "large",
		ModelName: "test-model",
	}, "", "", logger)

	err := pass(context.Background(), "cron")
	if err != nil {
		t.Fatalf("expected nil (swallowed), got %v", err)
	}
	if !anyContains(logged, "client build failed") {
		t.Errorf("expected log about client build failure, got: %v", logged)
	}
}

func TestDistillationPass_SwallowsUnreachableEndpoint(t *testing.T) {
	// Seed enough short-term candidates that the pipeline gets past
	// the "nothing to cluster" early return and actually tries the HTTP
	// call. Port 1 on loopback has no listener — the connect fails
	// and DistillationPass must absorb it.
	cx := setupCortex(t)
	seedTraces(t, cx, 3)

	var logged []string
	logger := func(format string, args ...any) {
		logged = append(logged, formatLine(format, args...))
	}

	pass := consolidation.DistillationPass(cx, consolidation.PipelineConfig{
		Window:     time.Hour,
		ModelTier:  "large",
		ModelName:  "test-model",
		MaxRetries: 0,
	}, "http://127.0.0.1:1/v1", "", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := pass(ctx, "cron"); err != nil {
		t.Fatalf("expected nil (swallowed), got %v", err)
	}
	// Either "pipeline error" (most common — connect refused) or "fallback-promoted"
	// is acceptable; both mean the pass completed without propagating failure.
	if !anyContains(logged, "auto-distillation") {
		t.Errorf("expected at least one auto-distillation log line, got: %v", logged)
	}
}

func TestDistillationPass_PropagatesContextCancel(t *testing.T) {
	// Shutdown path: a cancelled context must surface so the agent +
	// election gate unwind cleanly instead of recording a success.
	cx := setupCortex(t)
	seedTraces(t, cx, 3)

	pass := consolidation.DistillationPass(cx, consolidation.PipelineConfig{
		Window:    time.Hour,
		ModelTier: "large",
		ModelName: "test-model",
	}, "http://127.0.0.1:1/v1", "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := pass(ctx, "cron")
	if err == nil {
		t.Fatal("expected context.Canceled, got nil (pass should not swallow cancel)")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Errorf("expected context error, got %v", err)
	}
}

// ---- small test helpers ----

func formatLine(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

func anyContains(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}
