package federation

import (
	"errors"
	"testing"

	"github.com/Fail-Safe/Noema/internal/trace"
)

func TestClassifyReplayError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"invalid trace id sentinel", trace.ErrInvalidTraceID, ReasonInvalidTraceID},
		{"wrapped invalid trace id", errorsWrap(trace.ErrInvalidTraceID, "rejecting: %w"), ReasonInvalidTraceID},
		{"invalid frontmatter sentinel", trace.ErrInvalidFrontmatter, ReasonInvalidFrontmatter},
		{"unknown action text", errors.New("unknown action foo"), ReasonUnknownAction},
		{"unrecognized type text", errors.New("unrecognized type bar"), ReasonUnknownType},
		{"something else", errors.New("disk full"), ReasonOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyReplayError(tc.err); got != tc.want {
				t.Errorf("ClassifyReplayError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func errorsWrap(inner error, format string) error {
	return &wrappedErr{inner: inner, msg: format}
}

type wrappedErr struct {
	inner error
	msg   string
}

func (e *wrappedErr) Error() string { return e.msg }
func (e *wrappedErr) Unwrap() error { return e.inner }

func TestClassifyNetworkError(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{"refused", "dial tcp 1.2.3.4:3000: connection refused", ReasonNetworkRefused},
		{"timeout", "Get \"...\": context deadline exceeded", ReasonNetworkTimeout},
		{"dns", "dial tcp: lookup foo: no such host", ReasonNetworkDNS},
		{"tls", "x509: certificate signed by unknown authority", ReasonNetworkTLS},
		{"reset", "read tcp: connection reset by peer", ReasonNetworkReset},
		{"eof", "unexpected EOF", ReasonNetworkEOF},
		{"401", "401 Unauthorized", ReasonAuth},
		{"other", "something random", ReasonOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyNetworkError(errors.New(tc.msg)); got != tc.want {
				t.Errorf("ClassifyNetworkError(%q) = %q, want %q", tc.msg, got, tc.want)
			}
		})
	}
}

func TestIsSchemaWidening(t *testing.T) {
	wide := []string{ReasonInvalidTraceID, ReasonUnknownAction, ReasonUnknownType}
	for _, r := range wide {
		if !IsSchemaWidening(r) {
			t.Errorf("%q should be schema-widening", r)
		}
	}
	narrow := []string{
		ReasonAuth, ReasonNetworkTLS, ReasonNetworkRefused,
		ReasonIdentityMismatch, ReasonInvalidFrontmatter, ReasonOther,
	}
	for _, r := range narrow {
		if IsSchemaWidening(r) {
			t.Errorf("%q should NOT be schema-widening", r)
		}
	}
}

func TestIsNetwork(t *testing.T) {
	if !IsNetwork(ReasonNetworkTimeout) {
		t.Error("network_timeout should be classified as network")
	}
	if IsNetwork(ReasonInvalidTraceID) {
		t.Error("invalid_trace_id should not be classified as network")
	}
}
