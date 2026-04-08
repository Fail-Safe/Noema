package mcp_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpserver "github.com/Fail-Safe/Noema/internal/mcp"
)

// okHandler is a sentinel downstream handler that records whether it
// was called and returns 200 with a fixed body.
func okHandler(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if called != nil {
			*called = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// ---- AuthMiddleware ----

func TestAuthMiddleware_OpenMode_PassesRequestsThrough(t *testing.T) {
	called := false
	h := mcpserver.AuthMiddleware("")(okHandler(&called))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if !called {
		t.Error("downstream handler was not called in open mode")
	}
}

func TestAuthMiddleware_OpenMode_IgnoresHeaderPresence(t *testing.T) {
	// A random Authorization header should not trip anything in open mode.
	called := false
	h := mcpserver.AuthMiddleware("")(okHandler(&called))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer totally-wrong")
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (open mode ignores header content)", rr.Code)
	}
	if !called {
		t.Error("downstream handler was not called")
	}
}

func TestAuthMiddleware_Keyed_MissingHeader(t *testing.T) {
	called := false
	h := mcpserver.AuthMiddleware("s3cret")(okHandler(&called))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if called {
		t.Error("downstream handler must not be called on 401")
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	body, _ := io.ReadAll(rr.Body)
	if !strings.Contains(string(body), "unauthorized") {
		t.Errorf("body does not mention unauthorized: %q", string(body))
	}
}

func TestAuthMiddleware_Keyed_WrongBearerValue(t *testing.T) {
	called := false
	h := mcpserver.AuthMiddleware("s3cret")(okHandler(&called))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer not-the-right-one")
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if called {
		t.Error("downstream handler must not be called on 401")
	}
}

func TestAuthMiddleware_Keyed_WrongScheme(t *testing.T) {
	// Basic auth, Digest, or a raw token without a scheme must all 401.
	h := mcpserver.AuthMiddleware("s3cret")(okHandler(nil))

	cases := []string{
		"Basic czNjcmV0",   // base64("s3cret") under Basic
		"s3cret",           // raw value, no scheme
		"bearer s3cret",    // lowercase scheme (bearer != Bearer byte-for-byte)
		"Bearer  s3cret",   // extra space
		"Bearer s3cret\r\n", // trailing junk
	}
	for _, h3 := range cases {
		t.Run(h3, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			req.Header.Set("Authorization", h3)
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("Authorization=%q → status %d, want 401", h3, rr.Code)
			}
		})
	}
}

func TestAuthMiddleware_Keyed_CorrectBearer(t *testing.T) {
	called := false
	h := mcpserver.AuthMiddleware("s3cret")(okHandler(&called))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if !called {
		t.Error("downstream handler was not called on valid auth")
	}
}

func TestAuthMiddleware_Keyed_LongKeyStillWorks(t *testing.T) {
	// A 256-char key must still compare correctly after prehash.
	longKey := strings.Repeat("a", 256)
	h := mcpserver.AuthMiddleware(longKey)(okHandler(nil))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+longKey)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for long key", rr.Code)
	}
}

func TestAuthMiddleware_Keyed_EmptyHeaderRejectedButNotCrashing(t *testing.T) {
	// Empty Authorization header (set to "") must be rejected — and
	// must not crash the prehash path with a zero-length input.
	h := mcpserver.AuthMiddleware("s3cret")(okHandler(nil))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "")
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for empty header", rr.Code)
	}
}

// ---- CORSMiddleware ----

func TestCORSMiddleware_SetsHeadersOnRegularRequest(t *testing.T) {
	called := false
	h := mcpserver.CORSMiddleware(okHandler(&called))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	h.ServeHTTP(rr, req)

	if !called {
		t.Error("downstream handler was not called")
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("Access-Control-Allow-Origin missing")
	}
	if !strings.Contains(rr.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Error("Access-Control-Allow-Methods missing POST")
	}
	if !strings.Contains(rr.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Error("Access-Control-Allow-Headers must include Authorization (browsers need it to send Bearer)")
	}
	if !strings.Contains(rr.Header().Get("Access-Control-Allow-Headers"), "Mcp-Session-Id") {
		t.Error("Access-Control-Allow-Headers must include Mcp-Session-Id (MCP session tracking)")
	}
}

func TestCORSMiddleware_OptionsShortCircuits(t *testing.T) {
	// The whole point: OPTIONS must never reach mcp-go (which returns 404)
	// and must succeed with 204 No Content.
	called := false
	h := mcpserver.CORSMiddleware(okHandler(&called))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rr.Code)
	}
	if called {
		t.Error("downstream handler must not be called for OPTIONS")
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("preflight response missing Access-Control-Allow-Origin")
	}
}

// ---- CORS + Auth chain ----

func TestCORSAuthChain_OptionsBypassesAuthEvenInKeyedMode(t *testing.T) {
	// CORS preflight has no Authorization header by spec. If the chain
	// ordering is wrong (auth outside CORS), preflight would 401 and
	// the browser would never send the real request.
	called := false
	inner := mcpserver.AuthMiddleware("s3cret")(okHandler(&called))
	h := mcpserver.CORSMiddleware(inner)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204 (auth must not reject OPTIONS)", rr.Code)
	}
	if called {
		t.Error("downstream handler must not run for preflight")
	}
}

func TestCORSAuthChain_PostStillRequiresAuth(t *testing.T) {
	called := false
	inner := mcpserver.AuthMiddleware("s3cret")(okHandler(&called))
	h := mcpserver.CORSMiddleware(inner)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("POST without auth: status = %d, want 401", rr.Code)
	}
	if called {
		t.Error("downstream handler must not be called without auth")
	}
	// CORS headers should still be present on the 401 so browser
	// clients see a useful error (not a CORS-blocked opaque failure).
	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("401 response missing CORS header — browser will show an opaque error")
	}
}

func TestCORSAuthChain_PostWithAuthSucceeds(t *testing.T) {
	called := false
	inner := mcpserver.AuthMiddleware("s3cret")(okHandler(&called))
	h := mcpserver.CORSMiddleware(inner)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("authenticated POST status = %d, want 200", rr.Code)
	}
	if !called {
		t.Error("downstream handler was not called on authenticated POST")
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("200 response missing CORS header")
	}
}
