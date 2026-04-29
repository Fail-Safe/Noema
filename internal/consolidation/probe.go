package consolidation

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// defaultProbeTimeout bounds a single liveness check. A local LLM
// runtime (Ollama, LMStudio, llama.cpp, vLLM) returns /models
// essentially instantly; a five-second cap covers the cold-start
// startup window of vLLM/GPU backends while still failing closed on a
// dead endpoint within the user's patience.
const defaultProbeTimeout = 5 * time.Second

// ProbeEndpoint returns true when the given OpenAI-compatible endpoint
// is reachable and responding on /models. It issues an unauthenticated
// GET {endpoint}/models and treats any 2xx as alive.
//
// Auth headers are deliberately not attached. The check is a liveness
// signal for the coordination layer ("is this peer's LLM reachable at
// all"), not a full auth handshake. The actual consolidation pass
// performs its own authenticated request when the time comes; an
// endpoint that's up but rejecting our credentials will surface that
// at pass time, not here.
//
// A nil context is treated as context.Background(). The returned bool
// captures "probe succeeded"; an underlying network error is not
// propagated because callers only need a yes/no signal to gate rank
// eligibility.
func ProbeEndpoint(ctx context.Context, endpoint string) bool {
	if endpoint == "" {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Base URLs in cortex.md are typically written with a trailing slash
	// or not; strip either form before appending /models so we don't
	// accidentally produce //models.
	url := strings.TrimRight(endpoint, "/") + "/models"

	probeCtx, cancel := context.WithTimeout(ctx, defaultProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	// 2xx is alive. Anything else (4xx "endpoint there but /models not
	// served", 5xx, network errors routed here via transport wrappers)
	// is treated as ineligible. Conservative by design — a miscounting
	// would send us into a useless election that burns tokens on no one.
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
