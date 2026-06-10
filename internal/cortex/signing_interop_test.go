package cortex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/eventsig"
	"github.com/Fail-Safe/Noema/internal/federation"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// These tests exercise the cross-cortex wire path that the in-process unit
// tests in signing_test.go deliberately skip: a real cortex A emits a signed
// event, the event is serialized to JSON exactly as sync_events transmits it,
// deserialized on the far side, and replayed into a real cortex B. This is the
// only place a JSON-canonicalization drift in the signed preimage (Data,
// vclock, or timestamp bytes) would surface — struct-to-struct replay can't
// catch it because it never round-trips through the wire encoding.

// openSignedCortexWithID opens a signed cortex with a caller-chosen id so two
// distinct peers can coexist in one test (openVerifyCortex hardcodes one id).
func openSignedCortexWithID(t *testing.T, id, mode string) *Cortex {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "c")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	_, pub, seed, err := eventsig.Generate()
	if err != nil {
		t.Fatal(err)
	}
	writeSeedFile(t, dir, "noema-signing.key", seed, 0o600)
	m := Manifest{
		Name:       "c",
		Created:    "2026-01-01T00:00:00Z",
		Version:    ManifestVersion,
		ID:         id,
		Signing:    &SigningConfig{PublicKey: pub, PrivateKeyFile: "noema-signing.key"},
		Federation: &FederationConfig{Verify: mode},
	}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	cx, err := Open("c", dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return cx
}

// openUnsignedCortexWithMode opens an unsigned cortex (no keygen) that still
// runs in the given verify mode — the "pre-signing peer that just upgraded its
// binary" shape used by the mixed-version cases.
func openUnsignedCortexWithMode(t *testing.T, id, mode string) *Cortex {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "u")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	m := Manifest{
		Name:       "u",
		Created:    "2026-01-01T00:00:00Z",
		Version:    ManifestVersion,
		ID:         id,
		Federation: &FederationConfig{Verify: mode},
	}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	cx, err := Open("u", dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return cx
}

// emitAndWire adds a trace to src, then returns its create event after a full
// JSON marshal/unmarshal round trip — the exact bytes a peer receives from
// sync_events. The wire form, not the in-memory struct, is what gets replayed.
func emitAndWire(t *testing.T, src *Cortex, title, body string) event.Event {
	t.Helper()
	tr := trace.New(title, "fact", "agent-1", []string{"x"}, body)
	if err := src.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	evs, err := src.Events(tr.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var create *event.Event
	for i := range evs {
		if evs[i].Action == event.ActionCreate {
			create = &evs[i]
		}
	}
	if create == nil {
		t.Fatal("no create event emitted")
	}
	raw, err := json.Marshal(create)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	var wire event.Event
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	return wire
}

const (
	idPeerA = "01HVPEERA00000000000000000"
	idPeerB = "01HVPEERB00000000000000000"
)

// TestInterop_SignedRoundTripEnforce is the core gap-closer: A signs a create,
// it travels over the JSON wire, and an enforce-mode B accepts it (TOFU-pinning
// A's key) and materializes the trace — proving the signature survives the wire
// encoding and the pinned key persists for the next event.
func TestInterop_SignedRoundTripEnforce(t *testing.T) {
	a := openSignedCortexWithID(t, idPeerA, VerifyOff)
	b := openSignedCortexWithID(t, idPeerB, VerifyEnforce)

	wire := emitAndWire(t, a, "Shared fact", "body that crosses the wire")
	if wire.Signature == "" || wire.PubKey == "" {
		t.Fatal("emitted wire event lacks signature/pubkey")
	}

	if err := b.ReplayEvent(wire); err != nil {
		t.Fatalf("enforce-mode B should accept A's validly signed event over the wire: %v", err)
	}

	// B TOFU-pinned A's key under A's cortex_id.
	pinned, err := federation.NewState(b.DB.DB).GetCortexPubKey(idPeerA)
	if err != nil {
		t.Fatal(err)
	}
	if pinned != wire.PubKey {
		t.Fatalf("B pinned %q, want A's key %q", pinned, wire.PubKey)
	}

	// B materialized the trace and stored the event with signature intact, so
	// it can re-serve it transitively.
	if _, err := b.Get(wire.TraceID); err != nil {
		t.Fatalf("trace not materialized on B: %v", err)
	}
	stored, err := b.Events(wire.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) == 0 || stored[0].Signature != wire.Signature {
		t.Fatal("B did not persist the originating signature for transitive relay")
	}
}

// TestInterop_WireTamperRejected mutates a field after the event is on the wire
// (as a malicious relay would) and confirms enforce-mode B rejects it. The
// pinned-key path is exercised by pre-pinning A's key first.
func TestInterop_WireTamperRejected(t *testing.T) {
	a := openSignedCortexWithID(t, idPeerA, VerifyOff)
	b := openSignedCortexWithID(t, idPeerB, VerifyEnforce)

	wire := emitAndWire(t, a, "Authentic", "untampered body")
	if err := federation.NewState(b.DB.DB).SetCortexPubKey(idPeerA, wire.PubKey); err != nil {
		t.Fatal(err)
	}

	tampered := wire
	tampered.Origin = "relay-in-the-middle" // Origin is part of the signed preimage
	if err := b.ReplayEvent(tampered); err == nil {
		t.Fatal("enforce-mode B must reject an event tampered in transit")
	}

	// The untampered original still verifies under the same pinned key.
	if err := b.ReplayEvent(wire); err != nil {
		t.Fatalf("the authentic event should still verify: %v", err)
	}
}

// TestInterop_MixedVersionUnsignedUpstream is the backward/forward-compat
// matrix: an unsigned (pre-signing) peer's event is rejected by an enforce peer
// but accepted by an off peer, so upgrading the binary without enabling verify
// never breaks an existing ring.
func TestInterop_MixedVersionUnsignedUpstream(t *testing.T) {
	upstream := openUnsignedCortexWithMode(t, idPeerA, VerifyOff)
	wire := emitAndWire(t, upstream, "Legacy", "from an unsigned cortex")
	if wire.Signature != "" {
		t.Fatalf("unsigned cortex should emit no signature, got %q", wire.Signature)
	}

	enforce := openUnsignedCortexWithMode(t, idPeerB, VerifyEnforce)
	if err := enforce.ReplayEvent(wire); err == nil {
		t.Fatal("enforce mode must reject an unsigned upstream event")
	}

	off := openUnsignedCortexWithMode(t, idPeerB, VerifyOff)
	if err := off.ReplayEvent(wire); err != nil {
		t.Fatalf("off mode must accept an unsigned upstream event (backward compat): %v", err)
	}
}

// TestInterop_NoSilentDowngradeAfterPin closes the downgrade hole at the replay
// layer: once B has pinned A's key, an event from A's cortex_id that arrives
// stripped of its signature (an attacker or a buggy relay trying to dodge
// verification) is rejected under enforce rather than silently accepted.
func TestInterop_NoSilentDowngradeAfterPin(t *testing.T) {
	a := openSignedCortexWithID(t, idPeerA, VerifyOff)
	b := openSignedCortexWithID(t, idPeerB, VerifyEnforce)

	wire := emitAndWire(t, a, "Pinned", "establishes the pin")
	if err := b.ReplayEvent(wire); err != nil {
		t.Fatalf("first signed event should pin and verify: %v", err)
	}

	downgraded := wire
	downgraded.ID = "01HVEVENTDOWNGRADE00000000"
	downgraded.Signature = ""
	downgraded.PubKey = ""
	if err := b.ReplayEvent(downgraded); err == nil {
		t.Fatal("a pinned cortex cannot silently downgrade to unsigned under enforce")
	}
}
