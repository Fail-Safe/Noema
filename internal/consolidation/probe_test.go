package consolidation_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/consolidation"
)

func TestProbeEndpoint_EmptyEndpoint(t *testing.T) {
	if consolidation.ProbeEndpoint(context.Background(), "") {
		t.Error("empty endpoint: got alive, want dead")
	}
}

func TestProbeEndpoint_Alive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("probe hit wrong path: got %q, want /models", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if !consolidation.ProbeEndpoint(context.Background(), srv.URL) {
		t.Error("200 OK: got dead, want alive")
	}
}

func TestProbeEndpoint_TrailingSlashTolerated(t *testing.T) {
	// User-written base URLs in cortex.md commonly have a trailing
	// slash; the probe must not produce //models.
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if !consolidation.ProbeEndpoint(context.Background(), srv.URL+"/") {
		t.Error("trailing-slash endpoint: got dead, want alive")
	}
	if seenPath != "/models" {
		t.Errorf("path = %q, want /models", seenPath)
	}
}

func TestProbeEndpoint_Dead5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if consolidation.ProbeEndpoint(context.Background(), srv.URL) {
		t.Error("500: got alive, want dead")
	}
}

func TestProbeEndpoint_Dead4xx(t *testing.T) {
	// An endpoint that answers but doesn't serve /models (some minimal
	// runtimes) counts as dead from the coordination layer's view.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if consolidation.ProbeEndpoint(context.Background(), srv.URL) {
		t.Error("404: got alive, want dead")
	}
}

func TestProbeEndpoint_Unreachable(t *testing.T) {
	// A port nothing is listening on. Must fail closed quickly via the
	// probe's own timeout rather than hanging.
	start := time.Now()
	if consolidation.ProbeEndpoint(context.Background(), "http://127.0.0.1:1") {
		t.Error("unreachable: got alive, want dead")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("probe took %s on unreachable endpoint; expected < 10s", elapsed)
	}
}

func TestProbeEndpoint_NilContext(t *testing.T) {
	// Nil context must not panic — it's explicitly supported for callers
	// that don't have a meaningful context to pass. Build the nil via
	// a declared var so staticcheck's SA1012 doesn't trip (the probe
	// genuinely has a documented nil-context contract).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var nilCtx context.Context
	if !consolidation.ProbeEndpoint(nilCtx, srv.URL) {
		t.Error("nil context: got dead, want alive")
	}
}
