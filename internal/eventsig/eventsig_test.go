package eventsig

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/event"
)

func sampleEvent() event.Event {
	return event.Event{
		ID:        "01HV000000000000000000000A",
		Action:    event.ActionUpdate,
		TraceID:   "20260609-why-we-chose-go",
		CortexID:  "01HV0000000000000000000CTX",
		Origin:    "research-cortex",
		Timestamp: "2026-06-09T14:23:00Z",
		Data:      json.RawMessage(`{"title":"Why Go","content_hash":"abc","source_locked":true}`),
		VClock:    map[string]uint64{"01HV0000000000000000000CTX": 7, "01HV00000000000000000PEER": 3},
	}
}

func mustKeypair(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	priv, _, _, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return priv
}

func pub(priv ed25519.PrivateKey) string {
	return EncodePublic(priv.Public().(ed25519.PublicKey))
}

func TestSignVerifyRoundTrip(t *testing.T) {
	priv := mustKeypair(t)
	e := sampleEvent()

	sig, err := Sign(priv, e)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !strings.HasPrefix(sig, keyPrefix) {
		t.Fatalf("signature missing scheme prefix: %q", sig)
	}
	if err := Verify(pub(priv), e, sig); err != nil {
		t.Fatalf("Verify of own signature failed: %v", err)
	}
}

func TestSignatureIsDeterministic(t *testing.T) {
	priv := mustKeypair(t)
	e := sampleEvent()
	a, _ := Sign(priv, e)
	b, _ := Sign(priv, e)
	if a != b {
		t.Fatalf("Ed25519 signatures should be deterministic: %q != %q", a, b)
	}
}

// TestTamperEveryField is the core security property: mutating any signed
// field must break verification. A gap here is a forgeable field.
func TestTamperEveryField(t *testing.T) {
	priv := mustKeypair(t)
	e := sampleEvent()
	sig, _ := Sign(priv, e)
	pk := pub(priv)

	tampers := map[string]func(*event.Event){
		"ID":        func(x *event.Event) { x.ID = "01HV000000000000000000000B" },
		"Action":    func(x *event.Event) { x.Action = event.ActionTrash },
		"TraceID":   func(x *event.Event) { x.TraceID = "20260609-something-else" },
		"CortexID":  func(x *event.Event) { x.CortexID = "01HV0000000000000000000EVL" },
		"Origin":    func(x *event.Event) { x.Origin = "attacker" },
		"Timestamp": func(x *event.Event) { x.Timestamp = "2026-06-09T14:23:01Z" },
		"Data": func(x *event.Event) {
			x.Data = json.RawMessage(`{"title":"Why Go","content_hash":"abc","source_locked":false}`)
		},
		"VClock value": func(x *event.Event) {
			x.VClock = map[string]uint64{"01HV0000000000000000000CTX": 8, "01HV00000000000000000PEER": 3}
		},
		"VClock add key": func(x *event.Event) {
			x.VClock = map[string]uint64{"01HV0000000000000000000CTX": 7, "01HV00000000000000000PEER": 3, "01HV0000000000000000000NEW": 1}
		},
		"VClock drop key": func(x *event.Event) { x.VClock = map[string]uint64{"01HV0000000000000000000CTX": 7} },
	}

	for name, mutate := range tampers {
		t.Run(name, func(t *testing.T) {
			tampered := sampleEvent()
			mutate(&tampered)
			err := Verify(pk, tampered, sig)
			if !errors.Is(err, ErrVerify) {
				t.Fatalf("tampering %s did not invalidate the signature (err=%v)", name, err)
			}
		})
	}
}

// TestSignatureFieldExcludedFromPreimage: the Signature field must not feed its
// own preimage, or signing would be impossible to reproduce on the verifier.
func TestSignatureFieldExcludedFromPreimage(t *testing.T) {
	priv := mustKeypair(t)
	e := sampleEvent()
	sig, _ := Sign(priv, e)

	withSig := sampleEvent()
	withSig.Signature = sig // verifier receives the event with the signature populated
	if err := Verify(pub(priv), withSig, sig); err != nil {
		t.Fatalf("populating Signature changed the preimage: %v", err)
	}
}

// TestDataNormalizationConsistency: nil, empty, and explicit "{}" payloads must
// all hash to the same preimage, matching what the event store persists.
func TestDataNormalizationConsistency(t *testing.T) {
	base := sampleEvent()
	base.VClock = nil

	variants := []json.RawMessage{nil, {}, json.RawMessage("{}")}
	var want []byte
	for i, d := range variants {
		e := base
		e.Data = d
		got := Preimage(e)
		if i == 0 {
			want = got
			continue
		}
		if string(got) != string(want) {
			t.Fatalf("variant %d (%q) preimage differs from nil payload", i, string(d))
		}
	}
}

// TestVClockOrderIndependence: the canonical vclock must not depend on Go map
// iteration order. Building the same logical clock twice must yield one preimage.
func TestVClockOrderIndependence(t *testing.T) {
	a := sampleEvent()
	a.VClock = map[string]uint64{"aaa": 1, "bbb": 2, "ccc": 3}
	b := sampleEvent()
	b.VClock = map[string]uint64{"ccc": 3, "bbb": 2, "aaa": 1}
	if string(Preimage(a)) != string(Preimage(b)) {
		t.Fatal("preimage depends on vclock map order")
	}
}

func TestVerifyWrongKey(t *testing.T) {
	signer := mustKeypair(t)
	other := mustKeypair(t)
	e := sampleEvent()
	sig, _ := Sign(signer, e)
	if err := Verify(pub(other), e, sig); !errors.Is(err, ErrVerify) {
		t.Fatalf("verification under a different key should fail with ErrVerify, got %v", err)
	}
}

func TestMalformedInputs(t *testing.T) {
	priv := mustKeypair(t)
	e := sampleEvent()
	goodSig, _ := Sign(priv, e)
	goodPub := pub(priv)

	t.Run("missing sig prefix", func(t *testing.T) {
		bare := strings.TrimPrefix(goodSig, keyPrefix)
		if err := Verify(goodPub, e, bare); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("want ErrBadSignature, got %v", err)
		}
	})
	t.Run("bad sig base64", func(t *testing.T) {
		if err := Verify(goodPub, e, keyPrefix+"!!!not base64!!!"); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("want ErrBadSignature, got %v", err)
		}
	})
	t.Run("wrong sig length", func(t *testing.T) {
		if err := Verify(goodPub, e, keyPrefix+"YWJj"); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("want ErrBadSignature, got %v", err)
		}
	})
	t.Run("missing pub prefix", func(t *testing.T) {
		bare := strings.TrimPrefix(goodPub, keyPrefix)
		if err := Verify(bare, e, goodSig); !errors.Is(err, ErrBadPublicKey) {
			t.Fatalf("want ErrBadPublicKey, got %v", err)
		}
	})
	t.Run("unknown scheme", func(t *testing.T) {
		if _, err := ParsePublic("rsa:YWJj"); !errors.Is(err, ErrBadPublicKey) {
			t.Fatalf("want ErrBadPublicKey, got %v", err)
		}
	})
}

func TestPrivateFromSeedRoundTrip(t *testing.T) {
	priv, pubEnc, seed, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	restored, err := PrivateFromSeed(seed)
	if err != nil {
		t.Fatalf("PrivateFromSeed: %v", err)
	}
	e := sampleEvent()
	sig, _ := Sign(restored, e)
	if err := Verify(pubEnc, e, sig); err != nil {
		t.Fatalf("signature from seed-restored key failed to verify: %v", err)
	}
	// And the restored key must match the original.
	if string(restored) != string(priv) {
		t.Fatal("seed round-trip produced a different private key")
	}
}

func TestPublicKeysEqual(t *testing.T) {
	priv := mustKeypair(t)
	a := pub(priv)
	if !PublicKeysEqual(a, "  "+a+"\n") {
		t.Fatal("PublicKeysEqual should tolerate surrounding whitespace")
	}
	if PublicKeysEqual(a, pub(mustKeypair(t))) {
		t.Fatal("distinct keys reported equal")
	}
	if PublicKeysEqual(a, "garbage") {
		t.Fatal("malformed key reported equal")
	}
}
