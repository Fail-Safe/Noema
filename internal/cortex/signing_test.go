package cortex

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/eventsig"
	"github.com/Fail-Safe/Noema/internal/federation"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// makeSignedTraceEvent builds a create/update event with a valid content hash
// and signs it, so it survives both signature verification and replayCreate/
// replayUpdate's own content-hash check.
func makeSignedTraceEvent(t *testing.T, priv ed25519.PrivateKey, pub, cortexID, origin, id, traceID string, action event.Action, body string, sourceLocked bool) event.Event {
	t.Helper()
	payload := map[string]any{
		"title":         "Locked",
		"type":          "fact",
		"author":        "author-1",
		"origin":        origin,
		"body":          body,
		"content_hash":  trace.ContentHash(body),
		"source_locked": sourceLocked,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	e := event.Event{
		ID:        id,
		Action:    action,
		TraceID:   traceID,
		CortexID:  cortexID,
		Origin:    origin,
		Timestamp: "2026-01-01T00:00:00Z",
		Data:      data,
	}
	sig, err := eventsig.Sign(priv, e)
	if err != nil {
		t.Fatal(err)
	}
	e.Signature = sig
	e.PubKey = pub
	return e
}

const selfCortexID = "01HVSELF000000000000000000"

// openVerifyCortex opens a signed cortex with the given federation verify mode
// and returns it along with its own keypair (for self-origin event tests).
func openVerifyCortex(t *testing.T, mode string) (*Cortex, ed25519.PrivateKey, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "v")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	selfPriv, selfPub, selfSeed, err := eventsig.Generate()
	if err != nil {
		t.Fatal(err)
	}
	writeSeedFile(t, dir, "noema-signing.key", selfSeed, 0o600)
	m := Manifest{
		Name:       "v",
		Created:    "2026-01-01T00:00:00Z",
		Version:    ManifestVersion,
		ID:         selfCortexID,
		Signing:    &SigningConfig{PublicKey: selfPub, PrivateKeyFile: "noema-signing.key"},
		Federation: &FederationConfig{Verify: mode},
	}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	cx, err := Open("v", dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return cx, selfPriv, selfPub
}

// makeEvent builds a remote event and signs it with priv, attaching pub.
func makeEvent(t *testing.T, priv ed25519.PrivateKey, pub, cortexID, id string) event.Event {
	t.Helper()
	e := event.Event{
		ID:        id,
		Action:    event.ActionCreate,
		TraceID:   "20260101-remote-trace",
		CortexID:  cortexID,
		Origin:    "peer",
		Timestamp: "2026-01-01T00:00:00Z",
		Data:      event.NormalizeData(nil),
	}
	sig, err := eventsig.Sign(priv, e)
	if err != nil {
		t.Fatal(err)
	}
	e.Signature = sig
	e.PubKey = pub
	return e
}

func TestVerifyReplay_OffIsNoop(t *testing.T) {
	cx, _, _ := openVerifyCortex(t, VerifyOff)
	// A totally bogus event (no signature, unknown cortex) is accepted in off mode.
	e := event.Event{ID: "01HVEVENT00000000000000001", Action: event.ActionCreate, TraceID: "20260101-x", CortexID: "01HVPEER000000000000000000"}
	if err := cx.verifyReplayEvent(e); err != nil {
		t.Fatalf("off mode should accept anything, got: %v", err)
	}
}

func TestVerifyReplay_EnforceAcceptsValidPinned(t *testing.T) {
	cx, _, _ := openVerifyCortex(t, VerifyEnforce)
	peerPriv, peerPub, _, _ := eventsig.Generate()
	const peerID = "01HVPEER000000000000000000"
	if err := federation.NewState(cx.DB.DB).SetCortexPubKey(peerID, peerPub); err != nil {
		t.Fatal(err)
	}
	e := makeEvent(t, peerPriv, peerPub, peerID, "01HVEVENT00000000000000002")
	if err := cx.verifyReplayEvent(e); err != nil {
		t.Fatalf("valid signed event from pinned cortex should verify: %v", err)
	}
}

func TestVerifyReplay_EnforceRejectsUnsigned(t *testing.T) {
	cx, _, _ := openVerifyCortex(t, VerifyEnforce)
	e := event.Event{ID: "01HVEVENT00000000000000003", Action: event.ActionCreate, TraceID: "20260101-x", CortexID: "01HVPEER000000000000000000"}
	if err := cx.verifyReplayEvent(e); err == nil {
		t.Fatal("enforce mode should reject an unsigned event from an unpinned cortex")
	}
}

func TestVerifyReplay_EnforceRejectsBadSignature(t *testing.T) {
	cx, _, _ := openVerifyCortex(t, VerifyEnforce)
	peerPriv, peerPub, _, _ := eventsig.Generate()
	const peerID = "01HVPEER000000000000000000"
	e := makeEvent(t, peerPriv, peerPub, peerID, "01HVEVENT00000000000000004")
	e.Origin = "tampered" // invalidates the signature; pubkey still present
	if err := cx.verifyReplayEvent(e); err == nil {
		t.Fatal("enforce mode should reject an event whose signature no longer matches")
	}
}

func TestVerifyReplay_EnforceTOFUThenConflict(t *testing.T) {
	cx, _, _ := openVerifyCortex(t, VerifyEnforce)
	const peerID = "01HVPEER000000000000000000"
	privA, pubA, _, _ := eventsig.Generate()
	privB, pubB, _, _ := eventsig.Generate()

	// First sighting: no pin yet → TOFU-pin pubA after verifying.
	first := makeEvent(t, privA, pubA, peerID, "01HVEVENT00000000000000005")
	if err := cx.verifyReplayEvent(first); err != nil {
		t.Fatalf("first event should TOFU-pin and verify: %v", err)
	}
	pinned, _ := federation.NewState(cx.DB.DB).GetCortexPubKey(peerID)
	if !eventsig.PublicKeysEqual(pinned, pubA) {
		t.Fatalf("expected pubA pinned, got %q", pinned)
	}

	// A later event for the same cortex_id carrying a different key is a
	// conflict — even though it is validly signed under its own key.
	second := makeEvent(t, privB, pubB, peerID, "01HVEVENT00000000000000006")
	if err := cx.verifyReplayEvent(second); err == nil {
		t.Fatal("a key swap for a pinned cortex_id must be rejected")
	}
}

func TestVerifyReplay_EnforceRejectsSelfImpersonation(t *testing.T) {
	cx, _, _ := openVerifyCortex(t, VerifyEnforce)
	// An attacker forges an event under OUR cortex_id with their own key.
	attackerPriv, attackerPub, _, _ := eventsig.Generate()
	e := makeEvent(t, attackerPriv, attackerPub, selfCortexID, "01HVEVENT00000000000000007")
	if err := cx.verifyReplayEvent(e); err == nil {
		t.Fatal("an event claiming our identity signed by a foreign key must be rejected")
	}
}

func TestVerifyReplay_EnforceAcceptsValidSelf(t *testing.T) {
	cx, selfPriv, selfPub := openVerifyCortex(t, VerifyEnforce)
	// A genuine loopback of our own event verifies under our own key.
	e := makeEvent(t, selfPriv, selfPub, selfCortexID, "01HVEVENT00000000000000008")
	if err := cx.verifyReplayEvent(e); err != nil {
		t.Fatalf("our own signed event should verify on loopback: %v", err)
	}
}

func TestVerifyReplay_WarnAcceptsButLogs(t *testing.T) {
	cx, _, _ := openVerifyCortex(t, VerifyWarn)
	// Unsigned event from an unpinned cortex: warn logs but accepts (returns nil).
	e := event.Event{ID: "01HVEVENT00000000000000009", Action: event.ActionCreate, TraceID: "20260101-x", CortexID: "01HVPEER000000000000000000"}
	if err := cx.verifyReplayEvent(e); err != nil {
		t.Fatalf("warn mode should accept (log only), got: %v", err)
	}
}

// TestReplaySourceLock_RejectsForeignMutation is the Phase 6 payoff: with
// cortex_id authenticated, a source-locked trace can only be mutated by its
// owning cortex — the original Finding #1 hole, now closed.
func TestReplaySourceLock_RejectsForeignMutation(t *testing.T) {
	cx, _, _ := openVerifyCortex(t, VerifyEnforce)
	const tid = "20260101-locked-trace"

	// Publisher P creates a source-locked trace; replay accepts and records P
	// as the owning cortex.
	privP, pubP, _, _ := eventsig.Generate()
	const peerP = "01HVPEERP00000000000000000"
	create := makeSignedTraceEvent(t, privP, pubP, peerP, "peer-p", "01HVEV0000000000000000CRE", tid, event.ActionCreate, "original body", true)
	if err := cx.ReplayEvent(create); err != nil {
		t.Fatalf("replay of P's signed create should succeed: %v", err)
	}

	// Attacker X — validly signed as itself — tries to overwrite P's locked
	// trace. Identity forgery is impossible (it would fail signature checks),
	// so X emits honestly under its own id; the source-lock owner check is
	// what stops it.
	privX, pubX, _, _ := eventsig.Generate()
	const peerX = "01HVPEERX00000000000000000"
	attack := makeSignedTraceEvent(t, privX, pubX, peerX, "peer-x", "01HVEV0000000000000000ATK", tid, event.ActionUpdate, "attacker body", true)
	if err := cx.ReplayEvent(attack); err == nil {
		t.Fatal("a foreign cortex mutating a source-locked trace must be rejected")
	}

	// The owner P may update its own locked trace.
	legit := makeSignedTraceEvent(t, privP, pubP, peerP, "peer-p", "01HVEV0000000000000000UPD", tid, event.ActionUpdate, "P's revised body", true)
	if err := cx.ReplayEvent(legit); err != nil {
		t.Fatalf("owner P updating its own locked trace should succeed: %v", err)
	}
}

// TestReplaySourceLock_OffModeSkips documents that without authenticated
// cortex_id (off mode) the owner check is intentionally not applied, since a
// spoofed cortex_id would defeat it anyway.
func TestReplaySourceLock_OffModeSkips(t *testing.T) {
	cx, _, _ := openVerifyCortex(t, VerifyOff)
	const tid = "20260101-locked-trace"

	privP, pubP, _, _ := eventsig.Generate()
	create := makeSignedTraceEvent(t, privP, pubP, "01HVPEERP00000000000000000", "peer-p", "01HVEV0000000000000000CRE", tid, event.ActionCreate, "original body", true)
	if err := cx.ReplayEvent(create); err != nil {
		t.Fatalf("create replay: %v", err)
	}
	privX, pubX, _, _ := eventsig.Generate()
	attack := makeSignedTraceEvent(t, privX, pubX, "01HVPEERX00000000000000000", "peer-x", "01HVEV0000000000000000ATK", tid, event.ActionUpdate, "other body", true)
	if err := cx.ReplayEvent(attack); err != nil {
		t.Fatalf("off mode does not enforce source-lock on replay, expected accept: %v", err)
	}
}

func writeSeedFile(t *testing.T, dir, name, seed string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(seed+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	// os.WriteFile honors umask, so force the exact mode the test wants.
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSigningKeyUnsigned(t *testing.T) {
	for _, cfg := range []*SigningConfig{nil, {}, {PublicKey: "ed25519:x"}} {
		k, err := LoadSigningKey(t.TempDir(), cfg)
		if err != nil {
			t.Fatalf("cfg %+v: unexpected error %v", cfg, err)
		}
		if k.Signing() {
			t.Fatalf("cfg %+v: expected unsigned mode", cfg)
		}
	}
}

func TestLoadSigningKeyValid(t *testing.T) {
	dir := t.TempDir()
	_, pub, seed, err := eventsig.Generate()
	if err != nil {
		t.Fatal(err)
	}
	writeSeedFile(t, dir, "noema-signing.key", seed, 0o600)

	k, err := LoadSigningKey(dir, &SigningConfig{PublicKey: pub, PrivateKeyFile: "noema-signing.key"})
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}
	if !k.Signing() {
		t.Fatal("expected signing mode")
	}
	if k.Public != pub {
		t.Fatalf("public key mismatch: got %s want %s", k.Public, pub)
	}
}

func TestLoadSigningKeyRejectsLoosePerms(t *testing.T) {
	dir := t.TempDir()
	_, pub, seed, _ := eventsig.Generate()
	writeSeedFile(t, dir, "noema-signing.key", seed, 0o644)

	_, err := LoadSigningKey(dir, &SigningConfig{PublicKey: pub, PrivateKeyFile: "noema-signing.key"})
	if err == nil {
		t.Fatal("expected error for world-readable signing key file")
	}
}

func TestLoadSigningKeyPublicMismatch(t *testing.T) {
	dir := t.TempDir()
	_, _, seed, _ := eventsig.Generate()
	_, otherPub, _, _ := eventsig.Generate()
	writeSeedFile(t, dir, "noema-signing.key", seed, 0o600)

	_, err := LoadSigningKey(dir, &SigningConfig{PublicKey: otherPub, PrivateKeyFile: "noema-signing.key"})
	if err == nil {
		t.Fatal("expected mismatch error when manifest public_key disagrees with the private seed")
	}
}

// TestEmitSignsEvents is the Phase 3 end-to-end check: a cortex configured
// with a signing key must produce events that verify under its public key,
// and any tampering with a stored event must break that verification.
func TestEmitSignsEvents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "signed")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	_, pub, seed, err := eventsig.Generate()
	if err != nil {
		t.Fatal(err)
	}
	writeSeedFile(t, dir, "noema-signing.key", seed, 0o600)

	m := Manifest{
		Name:    "signed",
		Created: "2026-01-01T00:00:00Z",
		Version: ManifestVersion,
		ID:      "01HV0000000000000000000CTX",
		Signing: &SigningConfig{PublicKey: pub, PrivateKeyFile: "noema-signing.key"},
	}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}

	cx, err := Open("signed", dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cx.Close()

	tr := trace.New("My fact", "fact", "agent-1", []string{"a"}, "Some body content.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	evs, err := cx.Events(tr.ID)
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
		t.Fatal("no create event recorded")
	}
	if create.Signature == "" {
		t.Fatal("emitted event was not signed")
	}
	if err := eventsig.Verify(pub, *create, create.Signature); err != nil {
		t.Fatalf("emitted signature does not verify: %v", err)
	}

	// Tampering with any authenticated field must invalidate the signature.
	tampered := *create
	tampered.Origin = "attacker"
	if err := eventsig.Verify(pub, tampered, create.Signature); err == nil {
		t.Fatal("tampered event still verified — signature does not cover Origin")
	}
}

// TestUnsignedCortexEmitsUnsigned confirms the backward-compatible path: a
// cortex with no signing config emits events with an empty signature and does
// not error.
func TestUnsignedCortexEmitsUnsigned(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plain")
	cx, err := Open("plain", dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cx.Close()

	tr := trace.New("Plain", "note", "agent-1", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	evs, err := cx.Events(tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Signature != "" {
			t.Fatalf("unsigned cortex produced a signature: %q", e.Signature)
		}
	}
}
