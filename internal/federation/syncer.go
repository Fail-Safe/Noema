package federation

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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

// EventReplayer materializes a remote event on the local cortex.
type EventReplayer interface {
	ReplayEvent(event.Event) error
	MergeClock(VClock) error
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

		err := s.syncPeer(peer)
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
		hints = "    - is `noema serve --transport sse` running on the peer?\n" +
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

func (s *Syncer) syncPeer(peer PeerConfig) error {
	// Load cursor for this peer.
	cursor, err := s.state.Get(PeerCursorKey(peer.Name))
	if err != nil {
		return fmt.Errorf("loading cursor: %w", err)
	}

	// Connect to remote MCP server.
	sseURL := peer.Endpoint + "/sse"
	var clientOpts []transport.ClientOption
	if peer.CA != "" {
		httpClient, err := tlsClientWithCA(peer.CA)
		if err != nil {
			return fmt.Errorf("loading CA for peer %s: %w", peer.Name, err)
		}
		clientOpts = append(clientOpts, transport.WithHTTPClient(httpClient))
	}
	mcpClient, err := client.NewSSEMCPClient(sseURL, clientOpts...)
	if err != nil {
		return fmt.Errorf("creating SSE client: %w", err)
	}
	defer mcpClient.Close()

	if err := mcpClient.Start(s.ctx); err != nil {
		return fmt.Errorf("starting SSE connection: %w", err)
	}

	_, err = mcpClient.Initialize(s.ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "noema-syncer",
				Version: "0.1.0",
			},
		},
	})
	if err != nil {
		return fmt.Errorf("MCP initialize: %w", err)
	}

	// Verify the remote peer's identity before exchanging any events. On the
	// first successful handshake, the peer's ULID is pinned in federation_state.
	// Every subsequent sync re-checks the advertised ID against the pinned one
	// and refuses to proceed on a mismatch — see docs/design/cortex-uuid-plan.md.
	if err := s.verifyPeerIdentity(mcpClient, peer); err != nil {
		return err
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
		return fmt.Errorf("calling sync_events: %w", err)
	}

	// Parse the JSON response.
	var text string
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			text = tc.Text
			break
		}
	}
	if text == "" || text == "[]" {
		// No new events.
		now := time.Now().UTC().Format(time.RFC3339)
		s.state.SetPeerSeen(peer.Name, now)
		return nil
	}

	var events []event.Event
	if err := json.Unmarshal([]byte(text), &events); err != nil {
		return fmt.Errorf("parsing sync_events response: %w", err)
	}

	if err := s.replayBatch(peer.Name, events); err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	s.state.SetPeerSeen(peer.Name, now)
	return nil
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
			return nil
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
	if text == "" {
		return fmt.Errorf("peer %q returned empty cortex_identity response — likely an older version that pre-dates the cortex-id federation handshake. Upgrade the peer to a binary that exposes cortex_identity", peer.Name)
	}

	var identity struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal([]byte(text), &identity); err != nil {
		return fmt.Errorf("parsing cortex_identity response from peer %q: %w", peer.Name, err)
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
				"If this change is intentional, manually clear the pinned id and reset the cursor for this peer in federation_state",
			peer.Name, pinned, identity.ID,
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
			},
		},
	}, nil
}
