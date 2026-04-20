package federation

import (
	"errors"
	"strings"

	"github.com/Fail-Safe/Noema/internal/trace"
)

// PeerHealth is the runtime diagnostic state recorded for each peer.
// It is persisted per-peer as JSON in the federation_state KV store
// and read by `noema federation status` to surface version skew and
// stalled-sync conditions.
//
// Intentionally free of free-form error strings: error classification
// collapses any replay/sync failure into a small fixed enum
// (PeerError.Reason) plus a few structured fields (event_id,
// trace_id). The rendered output on the CLI turns the enum back into
// human-readable text at display time. See the design discussion in
// this commit's message for the sensitive-data reasoning.
type PeerHealth struct {
	// Version is the peer's binary version advertised via MCP
	// initialize (serverInfo.Version). Updated on every successful
	// connection, left blank until first contact.
	Version string `json:"version,omitempty"`

	// VersionObservedAt is the RFC3339 timestamp of the most recent
	// successful MCP initialize where the peer reported Version.
	VersionObservedAt string `json:"version_observed_at,omitempty"`

	// LastSuccess is the RFC3339 timestamp of the most recent poll
	// iteration that completed without any error (connect, identity
	// check, fetch, and replay all succeeded).
	LastSuccess string `json:"last_success,omitempty"`

	// ConsecutiveFailures counts poll iterations since the last
	// success that produced any error. Zero when the last poll
	// succeeded.
	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`

	// LastError is present when the most recent poll failed. Absent
	// when LastSuccess is newer than the last failure.
	LastError *PeerError `json:"last_error,omitempty"`
}

// PeerError is a structured record of one poll failure. Unlike a raw
// Go error it carries no free-form error text — Reason is a fixed
// enum and the other fields are limited to stable references
// (event/trace IDs that already appear in federation_state anyway).
type PeerError struct {
	// Reason is one of the Reason* constants below.
	Reason string `json:"reason"`
	// EventID is the ULID of the event that failed to replay, if
	// applicable. Empty for failures that occurred before any event
	// was fetched.
	EventID string `json:"event_id,omitempty"`
	// TraceID is the trace ID carried by the failing event, if any.
	// Derived from titles via slugification but already exposed
	// elsewhere in federation_state (peer cursors index the event
	// log), so storing it here adds no new attack surface.
	TraceID string `json:"trace_id,omitempty"`
	// ObservedAt is the RFC3339 timestamp when the failure happened.
	ObservedAt string `json:"observed_at"`
}

// Reason enum. Values are stable strings so old health records stay
// parseable when new values are added.
const (
	// Schema-widening reasons: these three signal that the peer is
	// running a binary that predates the event shapes being produced
	// elsewhere on the ring. `noema federation status` highlights
	// these specifically with an "upgrade this peer" hint.
	ReasonInvalidTraceID     = "invalid_trace_id"
	ReasonInvalidFrontmatter = "invalid_frontmatter"
	ReasonUnknownAction      = "unknown_action"
	ReasonUnknownType        = "unknown_type"

	// Network reasons mirror the syncer's existing categorizeError
	// tags (refused / timeout / dns / tls / reset / eof), prefixed
	// so the CLI can group them as "network problems" without
	// confusing them with replay failures.
	ReasonNetworkRefused = "network_refused"
	ReasonNetworkTimeout = "network_timeout"
	ReasonNetworkDNS     = "network_dns"
	ReasonNetworkTLS     = "network_tls"
	ReasonNetworkReset   = "network_reset"
	ReasonNetworkEOF     = "network_eof"

	// Auth / identity issues.
	ReasonAuth             = "auth"
	ReasonIdentityMismatch = "identity_mismatch"
	ReasonIdentityMissing  = "identity_missing"

	// Fallback bucket for anything we can't classify yet. The CLI
	// shows "other" verbatim and points the operator at the peer's
	// logs for detail.
	ReasonOther = "other"
)

// IsSchemaWidening reports whether a reason indicates that the peer
// binary predates schema changes that newer peers are producing. The
// CLI uses this to inject a specific "upgrade this peer" suggestion.
func IsSchemaWidening(reason string) bool {
	switch reason {
	case ReasonInvalidTraceID, ReasonUnknownAction, ReasonUnknownType:
		return true
	}
	return false
}

// IsNetwork reports whether a reason belongs to the network-problem
// family. Kept as a family so the CLI can render a shorter grouped
// hint rather than a bespoke line per tag.
func IsNetwork(reason string) bool {
	return strings.HasPrefix(reason, "network_")
}

// PollError wraps an error with classification metadata. The syncer
// returns it from per-peer poll iterations so the outer loop can
// record the structured outcome without re-parsing the error text.
type PollError struct {
	Reason  string
	EventID string
	TraceID string
	Err     error
}

func (e *PollError) Error() string { return e.Err.Error() }
func (e *PollError) Unwrap() error { return e.Err }

// ClassifyNetworkError maps connect/fetch-phase errors into the
// network_* reason set. Returns ReasonOther for anything that doesn't
// match a known pattern. Kept side-by-side with the syncer's
// categorizeError (which returns the short tag) to avoid a pointless
// second categorization.
func ClassifyNetworkError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return ReasonNetworkRefused
	case strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "context deadline exceeded"),
		strings.Contains(msg, "deadline exceeded"):
		return ReasonNetworkTimeout
	case strings.Contains(msg, "no such host"),
		strings.Contains(msg, "server misbehaving"),
		strings.Contains(msg, "no route to host"):
		return ReasonNetworkDNS
	case strings.Contains(msg, "x509:"),
		strings.Contains(msg, "tls:"),
		strings.Contains(msg, "certificate"):
		return ReasonNetworkTLS
	case strings.Contains(msg, "connection reset"):
		return ReasonNetworkReset
	case strings.Contains(msg, "EOF"):
		return ReasonNetworkEOF
	case strings.Contains(msg, "401"), strings.Contains(msg, "Unauthorized"):
		return ReasonAuth
	}
	return ReasonOther
}

// ClassifyReplayError maps an error returned from Cortex.ReplayEvent
// into the appropriate reason. The sentinel trace errors drive the
// three schema-widening reasons; anything else falls to ReasonOther.
// Kept in a single function so additions to the trace package's
// error vocabulary have exactly one place to update here too.
func ClassifyReplayError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, trace.ErrInvalidTraceID):
		return ReasonInvalidTraceID
	case errors.Is(err, trace.ErrInvalidFrontmatter):
		return ReasonInvalidFrontmatter
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unknown action"):
		return ReasonUnknownAction
	case strings.Contains(msg, "unrecognized type"),
		strings.Contains(msg, "unknown trace type"):
		return ReasonUnknownType
	}
	return ReasonOther
}
