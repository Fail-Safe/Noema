package federation

import (
	"testing"

	"github.com/Fail-Safe/Noema/internal/eventsig"
)

func genPub(t *testing.T) string {
	t.Helper()
	_, pub, _, err := eventsig.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

// TestPinSigningKey exercises the trust-on-first-use policy for federation
// signing keys: first contact pins, a matching re-advertisement is accepted, a
// changed key is refused, and an empty advertisement neither pins nor clears.
func TestPinSigningKey(t *testing.T) {
	s := newSyncerForTest(t, &fakeReplayer{})
	const ctxID = "01HV0000000000000000000CTX"
	pubA := genPub(t)
	pubB := genPub(t)

	// First contact: pin.
	if err := s.pinSigningKey("peer-a", ctxID, pubA, ""); err != nil {
		t.Fatalf("first pin should succeed: %v", err)
	}
	got, _ := s.state.GetCortexPubKey(ctxID)
	if !eventsig.PublicKeysEqual(got, pubA) {
		t.Fatalf("key not pinned: got %q", got)
	}

	// Re-advertising the same key is fine.
	if err := s.pinSigningKey("peer-a", ctxID, pubA, ""); err != nil {
		t.Fatalf("re-advertising same key should succeed: %v", err)
	}

	// A different key for the same cortex_id is refused.
	if err := s.pinSigningKey("peer-a", ctxID, pubB, ""); err == nil {
		t.Fatal("changed signing key should be refused")
	}
	// ...and the original pin is untouched.
	got, _ = s.state.GetCortexPubKey(ctxID)
	if !eventsig.PublicKeysEqual(got, pubA) {
		t.Fatal("refused mismatch must not overwrite the pinned key")
	}

	// An empty advertisement is a no-op that preserves the existing pin
	// (downgrade protection): a peer can't drop its key to dodge verification.
	if err := s.pinSigningKey("peer-a", ctxID, "", ""); err != nil {
		t.Fatalf("empty advertisement should be a no-op: %v", err)
	}
	got, _ = s.state.GetCortexPubKey(ctxID)
	if !eventsig.PublicKeysEqual(got, pubA) {
		t.Fatal("empty advertisement must not clear the pin")
	}

	// A different cortex_id pins independently.
	const otherCtx = "01HV00000000000000000OTHER"
	if err := s.pinSigningKey("peer-b", otherCtx, pubB, ""); err != nil {
		t.Fatalf("independent cortex pin should succeed: %v", err)
	}
	got, _ = s.state.GetCortexPubKey(otherCtx)
	if !eventsig.PublicKeysEqual(got, pubB) {
		t.Fatal("second cortex_id should pin its own key")
	}
}

// TestHardPinSigningKey covers the operator-configured hard-pin path, which
// overrides trust-on-first-use: a matching advertisement is accepted and
// pinned, a differing or absent advertisement fails the handshake, and a
// hard-pin overwrites a stale TOFU pin (explicit config beats first-use).
func TestHardPinSigningKey(t *testing.T) {
	const ctxID = "01HV0000000000000000000CTX"
	hardPin := genPub(t)
	other := genPub(t)

	// Match on first contact: accept and pin the hard-pinned key.
	t.Run("match pins", func(t *testing.T) {
		s := newSyncerForTest(t, &fakeReplayer{})
		if err := s.pinSigningKey("peer-a", ctxID, hardPin, hardPin); err != nil {
			t.Fatalf("advertised key matching the hard-pin should be accepted: %v", err)
		}
		got, _ := s.state.GetCortexPubKey(ctxID)
		if !eventsig.PublicKeysEqual(got, hardPin) {
			t.Fatalf("hard-pin not stored: got %q", got)
		}
	})

	// A differing advertisement fails the handshake regardless of TOFU state.
	t.Run("mismatch rejected", func(t *testing.T) {
		s := newSyncerForTest(t, &fakeReplayer{})
		if err := s.pinSigningKey("peer-a", ctxID, other, hardPin); err == nil {
			t.Fatal("an advertised key that differs from the hard-pin must be refused")
		}
		if got, _ := s.state.GetCortexPubKey(ctxID); got != "" {
			t.Fatalf("a rejected hard-pin handshake must not store anything, got %q", got)
		}
	})

	// A peer that advertises no key cannot satisfy a hard-pin (no silent
	// downgrade to unsigned for a high-assurance peer).
	t.Run("empty advertisement rejected", func(t *testing.T) {
		s := newSyncerForTest(t, &fakeReplayer{})
		if err := s.pinSigningKey("peer-a", ctxID, "", hardPin); err == nil {
			t.Fatal("a hard-pinned peer that advertises no key must be refused")
		}
	})

	// A hard-pin overwrites a pre-existing, differing TOFU pin.
	t.Run("overrides stale TOFU pin", func(t *testing.T) {
		s := newSyncerForTest(t, &fakeReplayer{})
		if err := s.state.SetCortexPubKey(ctxID, other); err != nil {
			t.Fatal(err)
		}
		if err := s.pinSigningKey("peer-a", ctxID, hardPin, hardPin); err != nil {
			t.Fatalf("hard-pin should override a stale TOFU pin: %v", err)
		}
		got, _ := s.state.GetCortexPubKey(ctxID)
		if !eventsig.PublicKeysEqual(got, hardPin) {
			t.Fatalf("hard-pin did not override the stale pin: got %q", got)
		}
	})
}
