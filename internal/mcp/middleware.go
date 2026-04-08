package mcp

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

// CORSMiddleware adds permissive CORS headers to every response and
// short-circuits OPTIONS preflight requests with 204 No Content.
//
// mcp-go's StreamableHTTPServer returns 404 for any method other than
// POST/GET/DELETE (streamable_http.go:254-255), so without this layer
// browser-based MCP clients (Zed, custom dashboards, anything running
// in a WebView) fail their preflight and never get to the real request.
// The CORS layer sits OUTSIDE AuthMiddleware because the CORS spec
// forbids sending credentials on preflight — a browser that can't
// complete preflight can't authenticate in the first place.
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// These headers are safe to set on every response. CORS-aware
		// clients use them; non-browser callers ignore them.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Session-Id")
		w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// AuthMiddleware returns a middleware that gates the wrapped handler
// behind a shared bearer key. When sharedKey is empty, the returned
// middleware is a pass-through (open mode): no header is required and
// no comparison runs.
//
// When keyed, the middleware computes a SHA-256 prehash of the
// expected "Bearer <key>" string once at construction, and hashes the
// incoming Authorization header on every request. The two 32-byte
// digests are compared with subtle.ConstantTimeCompare. Prehashing
// both sides closes a subtle timing leak: ConstantTimeCompare returns
// 0 immediately when its inputs differ in length, which would let a
// probing attacker recover the expected-key length bit by bit. After
// prehashing, both sides are always 32 bytes regardless of the
// attacker's guess, so length is never observable.
//
// There are zero MCP-method carve-outs. initialize, tools/list,
// tools/call for every tool, resources/list, ping — all gated. See
// Design Decision #3 in docs/design/mcp-auth-plan.md for why.
func AuthMiddleware(sharedKey string) func(http.Handler) http.Handler {
	if sharedKey == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	expected := sha256.Sum256([]byte("Bearer " + sharedKey))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
			if subtle.ConstantTimeCompare(got[:], expected[:]) != 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized: NOEMA_MCP_KEY / access.shared_key_file required"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
