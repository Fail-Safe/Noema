package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/consolidation"
	"github.com/Fail-Safe/Noema/internal/cortex"
)

func embeddingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "embeddings",
		Short: "Manage semantic-search embeddings",
		Long: "Inspect and rebuild the per-trace embedding index used by " +
			"semantic search. Embeddings are a local index (never federated); " +
			"configure search.embedding_model + an endpoint in cortex.md.",
	}
	cmd.AddCommand(embeddingsStatusCmd(), embeddingsBackfillCmd())
	return cmd
}

func embeddingsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show embedding coverage for the active cortex",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			m, err := cortex.ReadManifest(cx.Dir)
			if err != nil {
				return err
			}
			model := ""
			if m.Search != nil {
				model = m.Search.EmbeddingModel
			}
			st, err := cx.EmbeddingStatus(model)
			if err != nil {
				return err
			}

			if m.Search.SemanticOn() {
				fmt.Printf("Semantic search: enabled (model=%s, endpoint=%s)\n", model, m.ResolvedEmbeddingEndpoint())
			} else {
				fmt.Println("Semantic search: disabled (set search.semantic_enabled + search.embedding_model in cortex.md)")
			}
			fmt.Printf("Embeddable traces: %d\n", st.Embeddable)
			fmt.Printf("  embedded (up-to-date): %d\n", st.Embedded)
			fmt.Printf("  stale (changed or other model): %d\n", st.Stale)
			fmt.Printf("  missing: %d\n", st.Missing)
			return nil
		},
	}
}

func embeddingsBackfillCmd() *cobra.Command {
	var (
		force bool
		limit int
	)
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Embed traces that are missing or stale (for semantic search)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			m, err := cortex.ReadManifest(cx.Dir)
			if err != nil {
				return err
			}
			if m.Search == nil || m.Search.EmbeddingModel == "" {
				return fmt.Errorf("set search.embedding_model in cortex.md before backfilling")
			}
			endpoint := m.ResolvedEmbeddingEndpoint()
			if endpoint == "" {
				return fmt.Errorf("set search.embedding_endpoint (or consolidation.local_llm_endpoint) in cortex.md")
			}
			client, err := consolidation.NewHTTPLLMClient(endpoint, m.ResolvedEmbeddingAPIKeyEnv())
			if err != nil {
				return fmt.Errorf("embedding client: %w", err)
			}

			fmt.Printf("Backfilling embeddings (model=%s, endpoint=%s)...\n", m.Search.EmbeddingModel, endpoint)
			res, err := cx.EmbedBackfill(cmd.Context(), client, m.Search.EmbeddingModel, cortex.EmbedBackfillOpts{
				Force:    force,
				Limit:    limit,
				MaxChars: m.Search.EffectiveMaxChars(),
			})
			if err != nil {
				return fmt.Errorf("backfill failed after embedding %d: %w", res.Embedded, err)
			}
			fmt.Printf("Done: %d considered, %d embedded.\n", res.Considered, res.Embedded)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "re-embed all traces, ignoring staleness")
	cmd.Flags().IntVar(&limit, "limit", 0, "max traces to process (0 = all)")
	return cmd
}
