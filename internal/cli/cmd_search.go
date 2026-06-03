package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/consolidation"
	"github.com/Fail-Safe/Noema/internal/cortex"
)

func searchCmd() *cobra.Command {
	var (
		archived bool
		trashed  bool
		all      bool
		semantic bool
		typ      string
	)

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search across Traces",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			if semantic {
				return runSemanticSearch(cmd, cx, args[0], all || archived)
			}

			rows, err := cx.Search(args[0], cortex.ListOptions{
				Archived: archived,
				Trashed:  trashed,
				All:      all,
				Type:     typ,
			})
			if err != nil {
				return fmt.Errorf("search failed: %w", err)
			}
			if len(rows) == 0 {
				fmt.Println("No matching traces.")
				return nil
			}
			printTable(rows)
			return nil
		},
	}

	cmd.Flags().BoolVar(&archived, "archived", false, "search only archived traces")
	cmd.Flags().BoolVar(&trashed, "trashed", false, "search only trashed traces")
	cmd.Flags().BoolVar(&all, "all", false, "search active and archived traces")
	cmd.Flags().BoolVar(&semantic, "semantic", false, "rank by embedding similarity (needs a configured search: block + `noema embeddings backfill`)")
	cmd.Flags().StringVar(&typ, "type", "", "filter results by type")
	return cmd
}

// runSemanticSearch builds the embedder from the cortex manifest and ranks
// traces by embedding similarity. Kept separate so the lexical path stays
// untouched. The embedder reuses the consolidation endpoint config.
func runSemanticSearch(cmd *cobra.Command, cx *cortex.Cortex, query string, includeArchived bool) error {
	m, err := cortex.ReadManifest(cx.Dir)
	if err != nil {
		return err
	}
	if m.Search == nil || m.Search.EmbeddingModel == "" {
		return fmt.Errorf("semantic search needs search.embedding_model in cortex.md (then: noema embeddings backfill)")
	}
	endpoint := m.ResolvedEmbeddingEndpoint()
	if endpoint == "" {
		return fmt.Errorf("semantic search needs search.embedding_endpoint (or consolidation.local_llm_endpoint) in cortex.md")
	}
	client, err := consolidation.NewHTTPLLMClient(endpoint, m.ResolvedEmbeddingAPIKeyEnv())
	if err != nil {
		return fmt.Errorf("embedding client: %w", err)
	}
	res, err := cx.SemanticSearch(cmd.Context(), client, query, cortex.SemanticOpts{
		Model:           m.Search.EmbeddingModel,
		IncludeArchived: includeArchived,
	})
	if err != nil {
		return fmt.Errorf("semantic search failed: %w", err)
	}
	if len(res) == 0 {
		fmt.Println("No matching traces.")
		return nil
	}
	rows := make([]cortex.Row, len(res))
	for i, r := range res {
		rows[i] = r.Row
	}
	printTable(rows)
	return nil
}
