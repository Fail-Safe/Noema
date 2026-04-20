package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/consolidation"
	"github.com/Fail-Safe/Noema/internal/cortex"
)

func consolidateCmd() *cobra.Command {
	var (
		modelTierFlag string
		endpointFlag  string
		modelFlag     string
		apiKeyEnvFlag string
		dryRunFlag    bool
		windowFlag    int
		retriesFlag   int
	)

	cmd := &cobra.Command{
		Use:   "consolidate",
		Short: "Run one LLM-driven consolidation pass against the active cortex",
		Long: `Reads short-term candidates within the rolling window, clusters them,
and asks the configured LLM to distill each cluster into a single
mid-tier memory. Each distillation is recorded via
record_consolidation_result with model/profile/confidence metadata.

This is the unattended counterpart to the "connected agent consumes
list_consolidation_candidates" flow. Run it from cron, launchd, or
ad-hoc to exercise the consolidation pipeline without wiring up an
always-connected agent session.

Requires consolidation.enabled and consolidation.llm_enabled to be
true in cortex.md, plus consolidation.local_llm_endpoint set to an
OpenAI-compatible chat-completions base URL (Ollama, LMStudio,
llama.cpp server, vLLM, OpenAI, Azure OpenAI). The model name and
optional API-key env var also come from cortex.md; CLI flags
override per-invocation.`,
		Example: `  noema consolidate
  noema consolidate --dry-run
  noema consolidate --endpoint http://localhost:11434/v1 --model llama3.1:70b`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			m, err := cortex.ReadManifest(cx.Dir)
			if err != nil {
				return fmt.Errorf("reading cortex.md: %w", err)
			}
			cfg := m.Consolidation
			if cfg == nil || !cfg.Enabled {
				return fmt.Errorf("consolidation is not enabled in cortex.md; set consolidation.enabled: true")
			}
			if !cfg.LLMEnabled {
				return fmt.Errorf("consolidation.llm_enabled is false; `noema consolidate` requires the LLM path")
			}

			endpoint := cfg.LocalLLMEndpoint
			if endpointFlag != "" {
				endpoint = endpointFlag
			}
			if endpoint == "" {
				return fmt.Errorf("consolidation.local_llm_endpoint is empty and --endpoint was not provided")
			}

			modelName := cfg.ModelName
			if modelFlag != "" {
				modelName = modelFlag
			}
			if modelName == "" {
				return fmt.Errorf("consolidation.model_name is empty and --model was not provided")
			}

			apiKeyEnv := cfg.APIKeyEnv
			if apiKeyEnvFlag != "" {
				apiKeyEnv = apiKeyEnvFlag
			}

			modelTier := cfg.EffectiveModelTier()
			if modelTierFlag != "" {
				modelTier = modelTierFlag
			}

			window := cfg.EffectiveWindowHours()
			if windowFlag > 0 {
				window = time.Duration(windowFlag) * time.Hour
			}

			llm, err := consolidation.NewHTTPLLMClient(endpoint, apiKeyEnv)
			if err != nil {
				return fmt.Errorf("building llm client: %w", err)
			}

			// Graceful cancellation: Ctrl-C stops the pass at the
			// next cluster boundary without corrupting a
			// distillation mid-write.
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			logger := func(format string, args ...any) {
				fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", args...)
			}
			logger("[consolidate] endpoint=%s model=%q profile=%s window=%s dry-run=%v",
				endpoint, modelName, modelTier, window, dryRunFlag)

			result, err := consolidation.RunLLMPass(ctx, cx, llm, consolidation.PipelineConfig{
				Window:     window,
				ModelTier:  modelTier,
				ModelName:  modelName,
				MaxRetries: retriesFlag,
				DryRun:     dryRunFlag,
			}, logger)
			if err != nil {
				return fmt.Errorf("consolidation pass: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(),
				"Considered %d candidates, attempted %d clusters: %d distilled, %d rejected, %d fallback-promoted, %d skipped.\n",
				result.CandidatesConsidered, result.ClustersAttempted,
				result.DistillationsCreated, result.Rejected,
				result.FallbackPromotions, result.Skipped,
			)
			return nil
		},
	}

	cmd.Flags().StringVar(&modelTierFlag, "model-tier", "", "override consolidation.model_tier: small | large | frontier")
	cmd.Flags().StringVar(&endpointFlag, "endpoint", "", "override consolidation.local_llm_endpoint (OpenAI-compatible base URL)")
	cmd.Flags().StringVar(&modelFlag, "model", "", "override consolidation.model_name")
	cmd.Flags().StringVar(&apiKeyEnvFlag, "api-key-env", "", "override consolidation.api_key_env (env var holding bearer token)")
	cmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "format prompts and parse responses but skip record_consolidation_result")
	cmd.Flags().IntVar(&windowFlag, "window", 0, "override consolidation.window_hours")
	cmd.Flags().IntVar(&retriesFlag, "retries", 1, "retry budget per cluster before heuristic fallback")
	return cmd
}
