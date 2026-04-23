package federation

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// EventReplayer materializes a remote event on the local cortex and
// merges peer-owned tier-usage rows into the local trace_usage table.
// Named for historical reasons (used to be events-only); the merge
// hook was added when the federation started carrying read-signal
// deltas alongside events. Implemented by *cortex.Cortex.
type EventReplayer interface {
	ReplayEvent(event.Event) error
	MergeClock(VClock) error
	MergeRemoteUsage([]TraceUsage) error
}

// Syncer polls remote peers for new events and replays them locally.
type Syncer struct {
	replayer EventReplayer
	state    *State
	config   Config
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func NewSyncer(replayer EventReplayer, state *State, cfg Config) *Syncer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Syncer{
		replayer: replayer,
		state:    state,
		config:   cfg,
		ctx:      ctx,
		cancel:   cancel,
	}
}

func (s *Syncer) Start() {
	for _, peer := range s.config.Peers {
		if peer.Mode == PeerModePaused {
			log.Printf("[federation] peer %q is paused, skipping", peer.Name)
			continue
		}
		s.wg.Add(1)
		go s.syncLoop(peer)
	}
}

func (s *Syncer) Stop() {
	s.cancel()
	s.wg.Wait()
}

func (s *Syncer) syncLoop(peer PeerConfig) {
	defer s.wg.Done()

	interval := s.config.Interval
	if interval == 0 {
		interval = DefaultInterval
	}
	backoff := interval
	var lastErrCategory string

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		version, err := s.syncPeer(peer)
		s.recordPollOutcome(peer, version, err)
		if err != nil {
			category := categorizeError(err)
			backoff = min(backoff*2, 5*time.Minute)
			if category != lastErrCategory {
				log.Print(friendlyPeerError(peer, category, err, backoff))
			} else {
				log.Print(briefPeerError(peer, category, backoff))
			}
			lastErrCategory = category
		} else {
			if lastErrCategory != "" {
				log.Printf("[federation] peer %q reachable again", peer.Name)
			}
			lastErrCategory = ""
			backoff = interval
		}

		select {
		case <-s.ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// recordPollOutcome writes the structured health snapshot for a peer
// after every poll iteration. Reads the previous snapshot so success
// preserves the most-recent version (blank version would overwrite
// the known-good value on a transient network failure) and failures
// carry the right consecutive-failure count. Treats persistence errors
// as advisory — a health-write failure should not derail the next
// poll.
func (s *Syncer) recordPollOutcome(peer PeerConfig, version string, err error) {
	prev, loadErr := s.state.GetPeerHealth(peer.Name)
	if loadErr != nil {
		log.Printf("[federation] peer %q: could not load prior health: %v", peer.Name, loadErr)
	}
	next := prev

	if version != "" {
		next.Version = version
		next.VersionObservedAt = time.Now().UTC().Format(time.RFC3339)
	}

	if err == nil {
		next.LastSuccess = time.Now().UTC().Format(time.RFC3339)
		next.ConsecutiveFailures = 0
		next.LastError = nil
	} else {
		next.ConsecutiveFailures = prev.ConsecutiveFailures + 1
		next.LastError = classifyPollError(err)
	}

	if saveErr := s.state.SetPeerHealth(peer.Name, next); saveErr != nil {
		log.Printf("[federation] peer %q: could not persist health: %v", peer.Name, saveErr)
	}
}

// classifyPollError maps any error from syncPeer into the structured
// PeerError recorded in health. Replay failures surfaced through
// PollError carry their own classification and event/trace refs;
// everything else is treated as a network-or-initialize failure
// classified by ClassifyNetworkError.
func classifyPollError(err error) *PeerError {
	if err == nil {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var pe *PollError
	if errors.As(err, &pe) {
		reason := pe.Reason
		if reason == "" {
			reason = ReasonOther
		}
		return &PeerError{
			Reason:     reason,
			EventID:    pe.EventID,
			TraceID:    pe.TraceID,
			ObservedAt: now,
		}
	}
	return &PeerError{
		Reason:     ClassifyNetworkError(err),
		ObservedAt: now,
	}
}

// categorizeError classifies common network failures so the syncer can emit
// friendlier diagnostics. Returns a short tag like "refused" or "tls".
func categorizeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return "refused"
	case strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "context deadline exceeded"),
		strings.Contains(msg, "deadline exceeded"):
		return "timeout"
	case strings.Contains(msg, "no such host"),
		strings.Contains(msg, "server misbehaving"),
		strings.Contains(msg, "no route to host"):
		return "dns"
	case strings.Contains(msg, "x509:"),
		strings.Contains(msg, "tls:"),
		strings.Contains(msg, "certificate"):
		return "tls"
	case strings.Contains(msg, "connection reset"):
		return "reset"
	case strings.Contains(msg, "EOF"):
		return "eof"
	default:
		return "other"
	}
}

// friendlyPeerError renders a multi-line, human-readable diagnostic with
// troubleshooting hints. Used the first time a given category appears for
// a peer (or whenever the category changes).
func friendlyPeerError(peer PeerConfig, category string, err error, backoff time.Duration) string {
	header := fmt.Sprintf("[federation] cannot reach peer %q at %s", peer.Name, peer.Endpoint)
	var reason, hints string
	switch category {
	case "refused":
		reason = "the host is reachable but nothing is listening on that port"
		hints = "    - is `noema serve --transport http` running on the peer?\n" +
			"    - does the peer's --host/--port match the endpoint above?\n" +
			"    - is a firewall on the peer blocking the port?"
	case "timeout":
		reason = "the connection attempt timed out"
		hints = "    - is the peer host online and reachable on the network?\n" +
			"    - check for firewalls, VPNs, or routing issues between hosts"
	case "dns":
		reason = "the peer's hostname could not be resolved"
		hints = "    - is the hostname spelled correctly in cortex.md?\n" +
			"    - is your DNS resolver working? try `dig` or `nslookup` against it\n" +
			"    - if it's a LAN-only name, is mDNS / your hosts file set up?"
	case "tls":
		reason = "the TLS handshake failed"
		hints = "    - if the peer uses a self-signed cert, add `ca: /path/to/ca.pem` under that peer in cortex.md\n" +
			"    - does the cert's SAN match the endpoint hostname?\n" +
			"    - is the cert expired or signed by an unknown CA?"
	case "reset":
		reason = "the connection was reset by the peer"
		hints = "    - the peer may be restarting; will retry automatically\n" +
			"    - check the peer's logs for crashes or panics"
	case "eof":
		reason = "the peer closed the connection unexpectedly"
		hints = "    - the peer may not be a Noema MCP server, or may be on an incompatible version\n" +
			"    - check the peer's logs for startup errors"
	default:
		reason = err.Error()
	}
	msg := header + ":\n    " + reason + "\n"
	if hints != "" {
		msg += hints + "\n"
	}
	msg += fmt.Sprintf("    retrying in %s", backoff)
	return msg
}

// briefPeerError renders a one-line status update for a recurring failure
// in the same category, so logs don't fill up with the same wall of text.
func briefPeerError(peer PeerConfig, category string, backoff time.Duration) string {
	return fmt.Sprintf("[federation] peer %q still unreachable (%s); next retry in %s", peer.Name, category, backoff)
}

func (s *Syncer) syncPeer(peer PeerConfig) (string, error) {
	// Load cursor for this peer.
	cursor, err := s.state.Get(PeerCursorKey(peer.Name))
	if err != nil {
		return "", fmt.Errorf("loading cursor: %w", err)
	}

	// Connect to remote MCP server. Noema speaks Streamable HTTP (the
	// MCP 2025-03-26 transport): a single endpoint at /mcp that handles
	// JSON-RPC POSTs and optional SSE streaming on the same path.
	mcpURL := peer.Endpoint + "/mcp"
	var clientOpts []transport.StreamableHTTPCOption
	if peer.CA != "" {
		httpClient, err := tlsClientWithCA(peer.CA)
		if err != nil {
			return "", fmt.Errorf("loading CA for peer %s: %w", peer.Name, err)
		}
		clientOpts = append(clientOpts, transport.WithHTTPBasicClient(httpClient))
	}
	if s.config.SharedKey != "" {
		// Ring-model auth: the same key the middleware enforces on
		// incoming requests is attached to every outbound sync. Peers
		// in mismatched modes (one keyed, one open) will 401 each
		// other — by design; see docs/design/mcp-auth-plan.md.
		clientOpts = append(clientOpts, transport.WithHTTPHeaders(map[string]string{
			"Authorization": "Bearer " + s.config.SharedKey,
		}))
	}
	mcpClient, err := client.NewStreamableHttpClient(mcpURL, clientOpts...)
	if err != nil {
		return "", fmt.Errorf("creating Streamable HTTP client: %w", err)
	}
	defer mcpClient.Close()

	if err := mcpClient.Start(s.ctx); err != nil {
		return "", fmt.Errorf("starting Streamable HTTP connection: %w", err)
	}

	initResult, err := mcpClient.Initialize(s.ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "noema-syncer",
				Version: "0.1.0",
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("MCP initialize: %w", err)
	}
	peerVersion := initResult.ServerInfo.Version

	// Verify the remote peer's identity before exchanging any events. On the
	// first successful handshake, the peer's ULID is pinned in federation_state.
	// Every subsequent sync re-checks the advertised ID against the pinned one
	// and refuses to proceed on a mismatch — see docs/design/cortex-uuid-plan.md.
	if err := s.verifyPeerIdentity(mcpClient, peer); err != nil {
		return peerVersion, &PollError{Reason: classifyIdentityError(err), Err: err}
	}

	// Publish-mode cortexes serve events outward but never pull. The
	// identity handshake above still captures the peer's advertised
	// consolidation rank (plan §14) so election decisions on this peer
	// see the full ring. We just skip the sync_events pull and exit
	// after stamping last_seen.
	if s.config.Mode == "publish" {
		now := time.Now().UTC().Format(time.RFC3339)
		s.state.SetPeerSeen(peer.Name, now)
		return peerVersion, nil
	}

	// Call sync_events on the remote peer.
	args := map[string]any{"limit": 100}
	if cursor != "" {
		args["since"] = cursor
	}

	result, err := mcpClient.CallTool(s.ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "sync_events",
			Arguments: args,
		},
	})
	if err != nil {
		return peerVersion, fmt.Errorf("calling sync_events: %w", err)
	}

	// Parse the JSON response.
	var text string
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			text = tc.Text
			break
		}
	}

	// If the peer returned an error result (e.g. it's in subscribe mode
	// and refuses to serve sync_events), surface it cleanly rather than
	// trying to JSON-parse the human-readable error message as an event
	// array — that produces a confusing "invalid character 'h' in
	// literal true" rather than the actual reason.
	if result.IsError {
		return peerVersion, fmt.Errorf("peer sync_events refused: %s", text)
	}

	if text == "" || text == "[]" {
		// No new events, but still attempt usage sync below.
	} else {
		// Guard against a hostile peer returning an oversized payload.
		// 100 events * 1 MB body each = 100 MB is a generous upper bound.
		const maxSyncResponseBytes = 100 * 1024 * 1024 // 100 MiB
		if len(text) > maxSyncResponseBytes {
			return peerVersion, fmt.Errorf("sync_events response too large (%d bytes, max %d)", len(text), maxSyncResponseBytes)
		}

		var events []event.Event
		if err := json.Unmarshal([]byte(text), &events); err != nil {
			return peerVersion, fmt.Errorf("parsing sync_events response: %w", err)
		}

		if err := s.replayBatch(peer.Name, events); err != nil {
			return peerVersion, err
		}
	}

	// Phase 2: pull usage deltas. A failure here is logged but does
	// not fail the poll — events already succeeded, the usage cursor
	// stays at its last-applied position, and the next cycle retries.
	// Pre-PR-B peers (no sync_read_signal tool) return -32601; we
	// treat that as a known non-error and keep going.
	if err := s.syncReadSignalPhase(peer, mcpClient); err != nil {
		log.Printf("[federation] peer %q: read-signal sync failed (events already applied): %v", peer.Name, err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	s.state.SetPeerSeen(peer.Name, now)
	return peerVersion, nil
}

// syncReadSignalPhase is the PR B addition to each per-peer poll:
// after sync_events completes, pull the peer's trace_usage deltas
// (rows where peer_cortex_id = theirs and updated_at > our cursor)
// and merge with CRDT MAX semantics so the aggregate heuristic view
// reflects every peer's attention, not just the local slice.
//
// Returns nil on success including the "peer doesn't have the tool"
// case — that's the pre-PR-B fallback and shouldn't poison the rest
// of the sync cycle.
func (s *Syncer) syncReadSignalPhase(peer PeerConfig, mcpClient *client.Client) error {
	cursor, err := s.state.Get(PeerUsageCursorKey(peer.Name))
	if err != nil {
		return fmt.Errorf("loading usage cursor: %w", err)
	}

	args := map[string]any{"limit": 500}
	if cursor != "" {
		args["since"] = cursor
	}

	result, err := mcpClient.CallTool(s.ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "sync_read_signal",
			Arguments: args,
		},
	})
	if err != nil {
		// JSON-RPC "method not found" surfaces as -32601 in the
		// error message. Peers on pre-PR-B binaries return this;
		// treat as a clean no-op (logged once by the caller if it
		// keeps happening, not here).
		if strings.Contains(err.Error(), "-32601") ||
			strings.Contains(err.Error(), "method not found") ||
			strings.Contains(err.Error(), "Method not found") {
			return nil
		}
		return fmt.Errorf("calling sync_read_signal: %w", err)
	}

	var text string
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			text = tc.Text
			break
		}
	}

	if result.IsError {
		// Mode-gate refusals (e.g. subscribe peer refuses to serve):
		// treat as a clean skip for this cycle. No data lost — we
		// just don't get their usage this round.
		return fmt.Errorf("peer sync_read_signal refused: %s", text)
	}

	if text == "" || text == "[]" {
		return nil
	}

	const maxUsageResponseBytes = 16 * 1024 * 1024 // 16 MiB is plenty for 500 rows
	if len(text) > maxUsageResponseBytes {
		return fmt.Errorf("sync_read_signal response too large (%d bytes, max %d)", len(text), maxUsageResponseBytes)
	}

	var rows []TraceUsage
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		return fmt.Errorf("parsing sync_read_signal response: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	if err := s.replayer.MergeRemoteUsage(rows); err != nil {
		return fmt.Errorf("merging remote usage: %w", err)
	}

	// Advance the cursor to the last row's UpdatedAt. Rows are
	// ordered ASC by UpdatedAt on the server side (see
	// cortex.LocalUsageSince), so the tail element holds the
	// max timestamp we've now seen from this peer.
	newCursor := rows[len(rows)-1].UpdatedAt
	if err := s.state.Set(PeerUsageCursorKey(peer.Name), newCursor); err != nil {
		return fmt.Errorf("saving usage cursor: %w", err)
	}
	return nil
}

// classifyIdentityError maps errors returned from verifyPeerIdentity
// into the structured reason set. Pattern-matches on the substrings
// the verifyPeerIdentity callsites use when constructing the wrapped
// error (kept here rather than on the error type itself to avoid
// adding a new package-scoped sentinel for every condition).
func classifyIdentityError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "identity mismatch"):
		return ReasonIdentityMismatch
	case strings.Contains(msg, "no cortex id"),
		strings.Contains(msg, "pre-dates the cortex-id"):
		return ReasonIdentityMissing
	case strings.Contains(msg, "schema version"):
		return ReasonInvalidFrontmatter
	}
	return ReasonOther
}

// replayBatch replays a batch of events received from a peer, advancing
// the cursor after each successful replay. On the first failure it logs,
// updates the peer's last_seen timestamp (we did successfully connect),
// and returns nil with the cursor pinned at the previous event. The
// caller's next poll re-fetches starting from the failed event.
//
// The previous behavior was `continue` on failure, which let a later
// event in the same batch silently advance the cursor past a failed one.
// That dropped the failure on the floor and broke causal ordering for
// trace history. ReplayEvent already absorbs the benign cases (event
// already in the local log, full-mesh UNIQUE races, idempotent state
// transitions for archive/unarchive/trash/recover on missing traces),
// so anything that surfaces here is a real failure worth pausing on.
func (s *Syncer) replayBatch(peerName string, events []event.Event) error {
	for _, e := range events {
		if err := s.replayer.ReplayEvent(e); err != nil {
			log.Printf(
				"[federation] peer %q: replay of event %s (%s on trace %s) failed: %v — cursor pinned, will retry next poll",
				peerName, e.ID, e.Action, e.TraceID, err,
			)
			now := time.Now().UTC().Format(time.RFC3339)
			s.state.SetPeerSeen(peerName, now)
			// Return a structured error so the outer loop can
			// record event_id / trace_id / reason in peer health
			// without re-parsing the error text. The log line
			// above already carries the full error for operators
			// tailing journalctl; health storage is the stripped-
			// down, safe-for-persistence version.
			return &PollError{
				Reason:  ClassifyReplayError(err),
				EventID: e.ID,
				TraceID: e.TraceID,
				Err:     fmt.Errorf("replay event %s: %w", e.ID, err),
			}
		}

		// Merge the remote vector clock.
		if len(e.VClock) > 0 {
			if err := s.replayer.MergeClock(VClock(e.VClock)); err != nil {
				log.Printf("[federation] merge clock: %v", err)
			}
		}

		// Advance cursor.
		if err := s.state.SetPeerCursor(peerName, e.ID); err != nil {
			return fmt.Errorf("updating cursor: %w", err)
		}
	}
	return nil
}

// minPeerManifestVersion is the lowest cortex.md schema version a peer can
// run and still federate. v1 cortexes have no stable ULID and were rejected
// at the same time the local schema was bumped to v2 — see the hard-cut
// migration discussion in docs/design/cortex-uuid-plan.md.
const minPeerManifestVersion = 2

// verifyPeerIdentity calls the cortex_identity tool on a remote peer and
// pins its ULID on first contact. On subsequent calls it refuses to proceed
// if the advertised ULID has changed (peer was reset, replaced, or restored
// from a different cortex's backup) or if the peer is on a manifest version
// older than this binary supports.
func (s *Syncer) verifyPeerIdentity(mcpClient *client.Client, peer PeerConfig) error {
	result, err := mcpClient.CallTool(s.ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "cortex_identity",
			Arguments: map[string]any{},
		},
	})
	if err != nil {
		return fmt.Errorf("calling cortex_identity: %w", err)
	}

	var text string
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			text = tc.Text
			break
		}
	}
	if result.IsError {
		return fmt.Errorf("peer %q cortex_identity refused: %s", peer.Name, text)
	}
	if text == "" {
		return fmt.Errorf("peer %q returned empty cortex_identity response — likely an older version that pre-dates the cortex-id federation handshake. Upgrade the peer to a binary that exposes cortex_identity", peer.Name)
	}

	var identity struct {
		ID      string     `json:"id"`
		Name    string     `json:"name"`
		Version int        `json:"version"`
		Rank    *RankEntry `json:"rank,omitempty"`
	}
	if err := json.Unmarshal([]byte(text), &identity); err != nil {
		return fmt.Errorf("parsing cortex_identity response from peer %q: %w", peer.Name, err)
	}

	// Persist the peer's advertised consolidation rank if they reported
	// one. Rank is advisory — a failure here logs but doesn't abort the
	// sync. The CortexID we store is the authoritative identity.ID from
	// this same response, not the nested rank.CortexID, so a peer that
	// reports an inconsistent rank.CortexID can't confuse us about who
	// we're talking to. Peers on older binaries simply omit the field
	// and are left with whatever rank (if any) we last saw.
	if identity.Rank != nil {
		entry := RankEntry{
			CortexID:   identity.ID,
			Rank:       identity.Rank.Rank,
			ObservedAt: identity.Rank.ObservedAt,
		}
		if err := s.state.SetPeerRank(peer.Name, entry); err != nil {
			log.Printf("[federation] peer %q rank persist failed: %v", peer.Name, err)
		}
	}

	if identity.Version < minPeerManifestVersion {
		return fmt.Errorf(
			"peer %q is on cortex.md schema version %d but federation requires version %d. "+
				"Ask the peer's operator to run `noema migrate cortex-id` on that cortex before federation can resume",
			peer.Name, identity.Version, minPeerManifestVersion,
		)
	}
	if identity.ID == "" {
		return fmt.Errorf("peer %q reported manifest version %d but no cortex id — refusing to federate without a stable identity", peer.Name, identity.Version)
	}

	pinned, err := s.state.Get(PeerCortexIDKey(peer.Name))
	if err != nil {
		return fmt.Errorf("loading pinned cortex id for peer %q: %w", peer.Name, err)
	}
	if pinned == "" {
		// First successful handshake — pin the ID.
		if err := s.state.SetPeerCortexID(peer.Name, identity.ID); err != nil {
			return fmt.Errorf("pinning cortex id for peer %q: %w", peer.Name, err)
		}
		log.Printf("[federation] peer %q identity pinned: %s (%s)", peer.Name, identity.Name, identity.ID)
		return nil
	}
	if pinned != identity.ID {
		return fmt.Errorf(
			"peer %q identity mismatch: pinned id is %s but the endpoint now reports %s. "+
				"This can happen if the peer was reset, replaced, or restored from a different cortex's backup. "+
				"If this change is intentional, run `noema federation reset-peer %s` to clear the pin and cursor — the next sync will re-pin the peer's new identity",
			peer.Name, pinned, identity.ID, peer.Name,
		)
	}
	return nil
}

// tlsClientWithCA returns an HTTP client that trusts the given CA certificate
// file in addition to the system roots. Use this for self-signed or private CA certs.
func tlsClientWithCA(caPath string) (*http.Client, error) {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("reading CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA file %s contains no valid certificates", caPath)
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: pool,
				// Explicit floor per gosec G402 — stdlib default has
				// drifted upward over Go releases but pinning keeps
				// the ring from silently negotiating TLS 1.0/1.1 on
				// older peers with misconfigured OpenSSL-style stacks.
				MinVersion: tls.VersionTLS12,
			},
		},
	}, nil
}
