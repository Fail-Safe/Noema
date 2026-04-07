package cli

import (
	"strings"
	"testing"
)

// TestValidateSSEServe pins the flag invariants for `noema serve --transport
// sse`. The most important case is the explicit-cortex requirement: SSE
// silently inheriting the cortex from cfg.Default is the failure mode that
// motivated this guard, and the test must keep firing if anyone weakens
// the check (e.g. re-adding a "but only if peers are configured" condition).
func TestValidateSSEServe(t *testing.T) {
	const cortexName = "ai-1"

	cases := []struct {
		name           string
		host           string
		tlsCert        string
		tlsKey         string
		cortexExplicit bool
		wantErrSubstr  string // empty = expect nil
	}{
		{
			name:           "happy path no TLS",
			host:           "127.0.0.1",
			cortexExplicit: true,
		},
		{
			name:           "happy path with TLS",
			host:           "ai-1.example.com",
			tlsCert:        "/etc/ssl/cert.pem",
			tlsKey:         "/etc/ssl/key.pem",
			cortexExplicit: true,
		},
		{
			name:           "missing host",
			host:           "",
			cortexExplicit: true,
			wantErrSubstr:  "--host is required",
		},
		{
			name:           "0.0.0.0 unspecified IPv4",
			host:           "0.0.0.0",
			cortexExplicit: true,
			wantErrSubstr:  "binding to 0.0.0.0 is not allowed",
		},
		{
			name:           "IPv6 unspecified",
			host:           "::",
			cortexExplicit: true,
			wantErrSubstr:  "is not allowed",
		},
		{
			name:           "TLS cert without key",
			host:           "127.0.0.1",
			tlsCert:        "/etc/ssl/cert.pem",
			cortexExplicit: true,
			wantErrSubstr:  "--tls-cert and --tls-key must be provided together",
		},
		{
			name:           "TLS key without cert",
			host:           "127.0.0.1",
			tlsKey:         "/etc/ssl/key.pem",
			cortexExplicit: true,
			wantErrSubstr:  "--tls-cert and --tls-key must be provided together",
		},
		{
			// The regression case: a cortex is bound (here "ai-1") but
			// --cortex was not on the command line. Even though host and
			// TLS look fine, the implicit binding is a federation
			// footgun and must be rejected.
			name:           "implicit cortex on SSE",
			host:           "ai-1.example.com",
			tlsCert:        "/etc/ssl/cert.pem",
			tlsKey:         "/etc/ssl/key.pem",
			cortexExplicit: false,
			wantErrSubstr:  "without an explicit --cortex flag",
		},
		{
			// Specifically the agentbrain-on-ai-1 case from the field:
			// the bound cortex has no peers of its own, but is being
			// served on a host where another cortex's peers expect to
			// find a different identity. The guard must fire regardless
			// of the bound cortex's federation config.
			name:           "implicit cortex on SSE includes cortex name in error",
			host:           "ai-1-tb.home-dns.com",
			cortexExplicit: false,
			wantErrSubstr:  "\"ai-1\"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSSEServe(tc.host, tc.tlsCert, tc.tlsKey, cortexName, tc.cortexExplicit)
			if tc.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("expected nil, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

// TestValidateSSEServe_ImplicitCortexErrorMessage pins the specific shape of
// the explicit-cortex error message. The error has to (a) name the bound
// cortex so the operator can sanity-check it, (b) suggest the exact
// command they should re-run, and (c) explain the *why* (network
// exposure / silent failure mode) — not just say "no". A bare "missing
// --cortex" error would be technically correct but would put the operator
// right back in the same diagnostic loop the guard exists to short-circuit.
func TestValidateSSEServe_ImplicitCortexErrorMessage(t *testing.T) {
	err := validateSSEServe("ai-1.example.com", "", "", "ai-1", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		`"ai-1"`,                            // names the bound cortex
		"--cortex ai-1",                     // suggests the exact fix
		"--transport sse",                   // includes the transport flag
		"--host ai-1.example.com",           // includes the host
		"silent failures on the peer side",  // explains *why*
		"NOEMA_CORTEX or the config default", // names the implicit paths
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing required fragment %q\nfull message:\n%s", want, msg)
		}
	}
}
