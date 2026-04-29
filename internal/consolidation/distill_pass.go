package consolidation

import (
	"context"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

// DistillationPass returns a PassFn that runs the full LLM distillation
// pipeline (the same code path as `noema consolidate`) on every scheduled
// trigger, suitable for composing into the agent's chained pass via
// ChainPasses(distill, heuristic) → graduation.
//
// Failure semantics: the returned PassFn never propagates an error from
// the LLM pipeline. Endpoint unreachable, client-build failure, or pass
// error are logged and swallowed so the caller can still chain cheap
// heuristic + graduation work behind it. Context cancellation does
// propagate so shutdown still aborts the pass.
//
// Used from cmd_serve.go when ConsolidationConfig.AutoDistillationEnabled
// is true. The stable CLI-triggered `noema consolidate` path continues to
// build its own client and call RunLLMPass directly because it wants
// endpoint errors surfaced to the operator.
func DistillationPass(cx *cortex.Cortex, cfg PipelineConfig, endpoint, apiKeyEnv string, log func(format string, args ...any)) PassFn {
	if log == nil {
		log = func(string, ...any) {}
	}
	return func(ctx context.Context, trigger string) error {
		llm, err := NewHTTPLLMClient(endpoint, apiKeyEnv)
		if err != nil {
			log("[consolidation] auto-distillation skipped (client build failed, trigger=%s): %v", trigger, err)
			return nil
		}
		result, err := RunLLMPass(ctx, cx, llm, cfg, log)
		if err != nil {
			// Context cancel surfaces as the outer pass loop is shutting
			// down — propagate it so the election gate and agent both
			// unwind cleanly instead of reporting a successful pass.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log("[consolidation] auto-distillation skipped (pipeline error, trigger=%s): %v", trigger, err)
			return nil
		}
		log("[consolidation] auto-distillation (trigger=%s): considered=%d attempted=%d distilled=%d rejected=%d fallback=%d skipped=%d",
			trigger,
			result.CandidatesConsidered,
			result.ClustersAttempted,
			result.DistillationsCreated,
			result.Rejected,
			result.FallbackPromotions,
			result.Skipped,
		)
		return nil
	}
}
