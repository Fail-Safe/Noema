// Package eventsig implements the federation event-signing wire spec:
// an Ed25519 signature over a canonical,
// length-prefixed, domain-separated preimage of an event's authenticated
// fields. It is the single source of truth for how signatures are produced
// and verified; the emit path (cortex.emitEvent) and the replay path
// (cortex.ReplayEvent) both go through here so the two can never drift.
//
// This package is pure: it depends only on the event shape and the standard
// library, never on the database or filesystem. Keeping it side-effect-free
// is what makes the canonical preimage exhaustively testable.
package eventsig

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Fail-Safe/Noema/internal/event"
)

// domainTag is the version-pinned domain-separation prefix that opens every
// preimage. It guarantees a Noema event signature can never be valid for any
// other protocol's message, or for a future signing-format version, even when
// an attacker can influence the bytes that follow. The trailing newline is
// part of the tag.
const domainTag = "noema-event-sig-v1\n"

// keyAlg is the only signature scheme this version understands. Encoded keys
// and signatures carry it as a prefix ("ed25519:<base64>") so a verifier
// rejects an unrecognised scheme rather than guessing. The base64 is standard
// (padded) encoding of the raw bytes.
const keyAlg = "ed25519"

var keyPrefix = keyAlg + ":"

var (
	// ErrBadPublicKey is returned when a public key string is missing the
	// scheme prefix, is not valid base64, or is not the right length.
	ErrBadPublicKey = errors.New("eventsig: malformed public key")
	// ErrBadSignature is returned when a signature string is malformed
	// (wrong prefix, bad base64, wrong length) — distinct from a
	// well-formed signature that simply does not verify.
	ErrBadSignature = errors.New("eventsig: malformed signature")
	// ErrBadSeed is returned when a private-key seed string is malformed.
	ErrBadSeed = errors.New("eventsig: malformed seed")
	// ErrVerify is returned when a well-formed signature does not verify
	// against the event under the given public key.
	ErrVerify = errors.New("eventsig: signature verification failed")
)

// Preimage builds the canonical byte string that gets signed. The layout is
// fixed by the federation signing wire spec; any change here is a
// wire-format break and must bump domainTag. The event's own Signature field
// is deliberately excluded.
func Preimage(e event.Event) []byte {
	var b bytes.Buffer
	b.WriteString(domainTag)
	writeField(&b, []byte(e.ID))
	writeField(&b, []byte(string(e.Action)))
	writeField(&b, []byte(e.TraceID))
	writeField(&b, []byte(e.CortexID))
	writeField(&b, []byte(e.Origin))
	writeField(&b, []byte(e.Timestamp))
	writeField(&b, vclockBytes(e.VClock))

	// The signature covers a hash of the payload rather than the payload
	// itself: this binds content_hash, source_hash, source_locked, and body
	// (all carried inside Data) without an unbounded preimage. NormalizeData
	// matches exactly what the event store persists and what sync_events
	// transmits, so the verifier re-hashes identical bytes.
	sum := sha256.Sum256(event.NormalizeData(e.Data))
	writeField(&b, sum[:])

	return b.Bytes()
}

// vclockBytes serializes a vector clock deterministically: entries sorted by
// cortex-id key in byte order, each emitted as field(key) || u64be(value).
// A nil/empty clock yields an empty slice (the caller still wraps it in an
// outer field()).
func vclockBytes(vc map[string]uint64) []byte {
	if len(vc) == 0 {
		return nil
	}
	keys := make([]string, 0, len(vc))
	for k := range vc {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b bytes.Buffer
	for _, k := range keys {
		writeField(&b, []byte(k))
		var v [8]byte
		binary.BigEndian.PutUint64(v[:], vc[k])
		b.Write(v[:])
	}
	return b.Bytes()
}

// writeField appends a 4-byte big-endian length prefix followed by the bytes.
// Length-prefixing makes the concatenation unambiguous: no choice of field
// values can be re-partitioned into a different sequence of fields.
func writeField(b *bytes.Buffer, v []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(v)))
	b.Write(n[:])
	b.Write(v)
}

// Sign returns the "ed25519:<base64>" signature of the event under priv.
func Sign(priv ed25519.PrivateKey, e event.Event) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("%w: private key is %d bytes, want %d", ErrBadSeed, len(priv), ed25519.PrivateKeySize)
	}
	sig := ed25519.Sign(priv, Preimage(e))
	return keyPrefix + base64.StdEncoding.EncodeToString(sig), nil
}

// Verify checks that sig is a valid signature for e under the encoded public
// key. It returns nil on success, ErrBadPublicKey/ErrBadSignature for
// malformed inputs, or ErrVerify when a well-formed signature does not match.
// Callers distinguish these because the verify mode treats a malformed or
// failed signature the same way but logs them differently.
func Verify(pub string, e event.Event, sig string) error {
	pk, err := ParsePublic(pub)
	if err != nil {
		return err
	}
	raw, err := decodeScheme(sig, ed25519.SignatureSize)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	if !ed25519.Verify(pk, Preimage(e), raw) {
		return ErrVerify
	}
	return nil
}

// EncodePublic renders a public key as "ed25519:<base64>".
func EncodePublic(pub ed25519.PublicKey) string {
	return keyPrefix + base64.StdEncoding.EncodeToString(pub)
}

// ParsePublic decodes an "ed25519:<base64>" public key.
func ParsePublic(s string) (ed25519.PublicKey, error) {
	raw, err := decodeScheme(s, ed25519.PublicKeySize)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadPublicKey, err)
	}
	return ed25519.PublicKey(raw), nil
}

// EncodeSeed renders a 32-byte private-key seed as base64 for the sidecar
// file. The seed, not the expanded private key, is what gets stored.
func EncodeSeed(seed []byte) string {
	return base64.StdEncoding.EncodeToString(seed)
}

// PrivateFromSeed expands a base64-encoded 32-byte seed into a usable private
// key via ed25519.NewKeyFromSeed.
func PrivateFromSeed(s string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSeed, err)
	}
	if len(raw) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: seed is %d bytes, want %d", ErrBadSeed, len(raw), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

// Generate creates a fresh keypair and returns the private key, its encoded
// public key ("ed25519:<base64>"), and the base64 seed for the sidecar file.
func Generate() (priv ed25519.PrivateKey, pub string, seed string, err error) {
	s := make([]byte, ed25519.SeedSize)
	if _, err = rand.Read(s); err != nil {
		return nil, "", "", fmt.Errorf("eventsig: generating seed: %w", err)
	}
	priv = ed25519.NewKeyFromSeed(s)
	return priv, EncodePublic(priv.Public().(ed25519.PublicKey)), EncodeSeed(s), nil
}

// PublicKeysEqual reports whether two encoded public keys denote the same key,
// tolerating base64/whitespace differences by comparing the decoded bytes in
// constant time. Used by the TOFU pinning layer to decide whether an
// advertised key matches the one already pinned for a cortex.
func PublicKeysEqual(a, b string) bool {
	pa, err := ParsePublic(a)
	if err != nil {
		return false
	}
	pb, err := ParsePublic(b)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(pa, pb) == 1
}

// decodeScheme strips the mandatory "ed25519:" prefix, base64-decodes the
// remainder, and checks it is exactly wantLen bytes.
func decodeScheme(s string, wantLen int) ([]byte, error) {
	s = strings.TrimSpace(s)
	rest, ok := strings.CutPrefix(s, keyPrefix)
	if !ok {
		return nil, fmt.Errorf("missing %q scheme prefix", keyAlg)
	}
	raw, err := base64.StdEncoding.DecodeString(rest)
	if err != nil {
		return nil, fmt.Errorf("base64: %v", err)
	}
	if len(raw) != wantLen {
		return nil, fmt.Errorf("decoded %d bytes, want %d", len(raw), wantLen)
	}
	return raw, nil
}
