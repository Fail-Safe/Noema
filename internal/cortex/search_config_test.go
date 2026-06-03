package cortex_test

import (
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

func TestValidateSearch(t *testing.T) {
	tests := []struct {
		name    string
		m       cortex.Manifest
		wantErr bool
	}{
		{
			name: "absent block is fine",
			m:    cortex.Manifest{},
		},
		{
			name:    "disabled but bad default_mode is rejected",
			m:       cortex.Manifest{Search: &cortex.SearchConfig{DefaultMode: "fuzzy"}},
			wantErr: true,
		},
		{
			name: "disabled with valid default_mode is fine",
			m:    cortex.Manifest{Search: &cortex.SearchConfig{DefaultMode: cortex.SearchModeHybrid}},
		},
		{
			name:    "enabled without model is rejected",
			m:       cortex.Manifest{Search: &cortex.SearchConfig{SemanticEnabled: true, EmbeddingEndpoint: "http://localhost:11434/v1"}},
			wantErr: true,
		},
		{
			name:    "enabled without any endpoint is rejected",
			m:       cortex.Manifest{Search: &cortex.SearchConfig{SemanticEnabled: true, EmbeddingModel: "nomic-embed-text"}},
			wantErr: true,
		},
		{
			name: "enabled, endpoint inherited from consolidation, is fine",
			m: cortex.Manifest{
				Consolidation: &cortex.ConsolidationConfig{LocalLLMEndpoint: "http://localhost:11434/v1"},
				Search:        &cortex.SearchConfig{SemanticEnabled: true, EmbeddingModel: "nomic-embed-text"},
			},
		},
		{
			name: "enabled with own endpoint + model is fine",
			m: cortex.Manifest{Search: &cortex.SearchConfig{
				SemanticEnabled:   true,
				EmbeddingEndpoint: "http://localhost:11434/v1",
				EmbeddingModel:    "nomic-embed-text",
			}},
		},
		{
			name: "hybrid_weight out of range is rejected",
			m: cortex.Manifest{Search: &cortex.SearchConfig{
				SemanticEnabled:   true,
				EmbeddingEndpoint: "http://localhost:11434/v1",
				EmbeddingModel:    "nomic-embed-text",
				HybridWeight:      1.5,
			}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.m.ValidateSearch()
			if tt.wantErr != (err != nil) {
				t.Errorf("ValidateSearch() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestSearchConfigAccessors(t *testing.T) {
	var nilCfg *cortex.SearchConfig
	if nilCfg.SemanticOn() {
		t.Error("nil config should report semantic off")
	}
	if nilCfg.EffectiveDefaultMode() != cortex.SearchModeLexical {
		t.Error("nil config default mode should be lexical")
	}
	if nilCfg.EffectiveHybridWeight() != 0.5 {
		t.Errorf("nil config hybrid weight = %v, want 0.5", nilCfg.EffectiveHybridWeight())
	}
	if nilCfg.EffectiveMaxChars() != 32000 {
		t.Errorf("nil config max chars = %d, want 32000", nilCfg.EffectiveMaxChars())
	}

	// Clamp above 1.
	hi := &cortex.SearchConfig{HybridWeight: 2}
	if hi.EffectiveHybridWeight() != 1 {
		t.Errorf("hybrid weight clamp = %v, want 1", hi.EffectiveHybridWeight())
	}
}

func TestResolvedEmbeddingEndpoint_Inheritance(t *testing.T) {
	// Search block wins when set.
	m := cortex.Manifest{
		Consolidation: &cortex.ConsolidationConfig{LocalLLMEndpoint: "http://consolidation:1", APIKeyEnv: "C_KEY"},
		Search:        &cortex.SearchConfig{EmbeddingEndpoint: "http://search:2", APIKeyEnv: "S_KEY"},
	}
	if got := m.ResolvedEmbeddingEndpoint(); got != "http://search:2" {
		t.Errorf("endpoint = %q, want search override", got)
	}
	if got := m.ResolvedEmbeddingAPIKeyEnv(); got != "S_KEY" {
		t.Errorf("apikeyenv = %q, want search override", got)
	}

	// Falls back to consolidation when search leaves them empty.
	m2 := cortex.Manifest{
		Consolidation: &cortex.ConsolidationConfig{LocalLLMEndpoint: "http://consolidation:1", APIKeyEnv: "C_KEY"},
		Search:        &cortex.SearchConfig{SemanticEnabled: true, EmbeddingModel: "m"},
	}
	if got := m2.ResolvedEmbeddingEndpoint(); got != "http://consolidation:1" {
		t.Errorf("endpoint = %q, want consolidation fallback", got)
	}
	if got := m2.ResolvedEmbeddingAPIKeyEnv(); got != "C_KEY" {
		t.Errorf("apikeyenv = %q, want consolidation fallback", got)
	}
}
