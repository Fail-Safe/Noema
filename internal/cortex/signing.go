package cortex

import (
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"path/filepath"

	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/eventsig"
	"github.com/Fail-Safe/Noema/internal/federation"
)

// SigningKey is a cortex's resolved Ed25519 signing material. The zero value
// represents unsigned mode: a cortex that has not run `noema keygen` emits
// events without a signature and federates exactly as it did before signing
// existed.
type SigningKey struct {
	// Private is the expanded private key used to sign emitted events. Nil
	// in unsigned mode. Like the MCP shared key, it must never be logged,
	// written to the event log, or echoed in errors.
	Private ed25519.PrivateKey

	// Public is the "ed25519:<base64>" public key advertised to peers. Safe
	// to log and display. Empty in unsigned mode.
	Public string
}

// Signing reports whether usable signing material was loaded.
func (k SigningKey) Signing() bool { return k.Private != nil }

// PublicKey returns this cortex's "ed25519:<base64>" federation signing public
// key, or "" if the cortex is unsigned. It is advertised to peers through the
// cortex_identity handshake so they can pin it and verify this cortex's events.
func (c *Cortex) PublicKey() string { return c.signKey.Public }

// verifyReplayEvent enforces the federation verify mode against an incoming
// remote event. It is called at the top of ReplayEvent, before any state
// change. In "off" mode it is a no-op. In "warn" it logs problems but accepts;
// in "enforce" it returns an error, which pins the syncer cursor and stops the
// batch (the same path any replay failure takes).
func (c *Cortex) verifyReplayEvent(e event.Event) error {
	if c.verifyMode == "" || c.verifyMode == VerifyOff {
		return nil
	}

	// An event claiming OUR cortex_id must verify under OUR key, never a key
	// carried in the event — otherwise a forger could impersonate us by
	// emitting under our id with their own pubkey.
	if e.CortexID == c.ID {
		if c.signKey.Public == "" {
			return c.failVerify(e, fmt.Errorf("event claims this cortex's id %s but no local signing key is configured to check it", c.ID))
		}
		if e.Signature == "" {
			return c.failVerify(e, fmt.Errorf("event %s claims this cortex's id but carries no signature", e.ID))
		}
		if err := eventsig.Verify(c.signKey.Public, e, e.Signature); err != nil {
			return c.failVerify(e, fmt.Errorf("event %s claims our identity but does not verify under our key: %w", e.ID, err))
		}
		return nil
	}

	state := federation.NewState(c.DB.DB)
	pinned, err := state.GetCortexPubKey(e.CortexID)
	if err != nil {
		return fmt.Errorf("loading pinned signing key for cortex %s: %w", e.CortexID, err)
	}

	keyToUse := pinned
	switch {
	case pinned != "":
		// A known cortex must verify under its pinned key. An event that also
		// carries a different key is an impersonation attempt or an
		// uncoordinated rotation — refuse it rather than silently re-pinning.
		if e.PubKey != "" && !eventsig.PublicKeysEqual(e.PubKey, pinned) {
			return c.failVerify(e, fmt.Errorf("event %s pubkey conflicts with the pinned key for cortex %s", e.ID, e.CortexID))
		}
	case e.PubKey != "":
		// No key pinned yet: trust-on-first-use the key carried in the event,
		// but only after the signature actually verifies under it (below).
		keyToUse = e.PubKey
	default:
		return c.failVerify(e, fmt.Errorf("no signing key available for cortex %s (event carries none and none is pinned)", e.CortexID))
	}

	if e.Signature == "" {
		return c.failVerify(e, fmt.Errorf("unsigned event %s from cortex %s", e.ID, e.CortexID))
	}
	if err := eventsig.Verify(keyToUse, e, e.Signature); err != nil {
		return c.failVerify(e, fmt.Errorf("signature verification failed for event %s from cortex %s: %w", e.ID, e.CortexID, err))
	}

	// Verified. If we learned the key from the event, pin it now (TOFU) so a
	// later event that swaps the key for this cortex_id is caught as a conflict.
	if pinned == "" {
		if err := state.SetCortexPubKey(e.CortexID, keyToUse); err != nil {
			return fmt.Errorf("pinning signing key for cortex %s: %w", e.CortexID, err)
		}
		log.Printf("[federation] TOFU-pinned signing key for cortex %s from event %s", e.CortexID, e.ID)
	}
	return nil
}

// failVerify applies the verify mode to a verification failure: enforce returns
// an error (rejecting the event); warn logs and accepts.
func (c *Cortex) failVerify(e event.Event, reason error) error {
	if c.verifyMode == VerifyEnforce {
		return fmt.Errorf("rejecting event %s (verify=enforce): %w", e.ID, reason)
	}
	log.Printf("[federation] signature warning (action=%s trace=%s): %v — accepted (verify=warn)", e.Action, e.TraceID, reason)
	return nil
}

// checkReplaySourceLock enforces, on the replay path, that a source-locked
// trace may only be mutated by the cortex that owns it. This is the federation
// counterpart to CheckSourceLock (which guards local mutations): without it,
// any peer could overwrite, trash, or purge a source-locked trace by emitting
// an event for it.
//
// The rule is only meaningful once cortex_id is authenticated, so it is gated
// on the verify mode exactly like signature verification: off skips it (a
// spoofed cortex_id would defeat it anyway), enforce rejects a violation, warn
// logs it. By the time this runs in enforce mode, verifyReplayEvent has already
// proven the event's cortex_id, so an attacker cannot pass the owner check by
// claiming the owner's identity — that forgery was already rejected at the
// signature step. This catches the remaining case: an authenticated-but-
// unauthorized cortex trying to mutate another cortex's locked trace.
func (c *Cortex) checkReplaySourceLock(e event.Event) error {
	if c.verifyMode == "" || c.verifyMode == VerifyOff {
		return nil
	}
	switch e.Action {
	case event.ActionUpdate, event.ActionTagUpdate, event.ActionTrash, event.ActionPurge:
	default:
		return nil // create/archive/vote/tier/etc. don't overwrite locked content
	}

	var sourceLocked int
	var owner string
	err := c.DB.QueryRow(`SELECT source_locked, cortex_id FROM traces WHERE id = ?`, e.TraceID).Scan(&sourceLocked, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // we hold no local copy to protect
	}
	if err != nil {
		return fmt.Errorf("checking source-lock for replayed event %s: %w", e.ID, err)
	}
	if sourceLocked == 0 || e.CortexID == owner {
		return nil
	}

	reason := fmt.Errorf("event %s (%s) targets source-locked trace %s owned by cortex %s but originates from cortex %s",
		e.ID, e.Action, e.TraceID, owner, e.CortexID)
	if c.verifyMode == VerifyEnforce {
		return fmt.Errorf("rejecting source-lock violation: %w", reason)
	}
	log.Printf("[federation] source-lock warning: %v — accepted (verify=warn)", reason)
	return nil
}

// LoadSigningKey resolves a cortex's signing key from its manifest config.
// It returns the zero value (unsigned mode) when no signing config or no
// private-key file is present. The sidecar file is held to the same 0600 /
// size / single-line rules as the MCP shared key.
//
// When the manifest also records a public_key, it is checked against the key
// derived from the private seed: a mismatch is a hard error, because a
// manifest public_key that disagrees with the private key would make every
// signature this cortex emits fail verification on its peers — a silent,
// fleet-wide federation outage that is far better caught at startup.
func LoadSigningKey(cortexDir string, cfg *SigningConfig) (SigningKey, error) {
	if cfg == nil || cfg.PrivateKeyFile == "" {
		return SigningKey{}, nil
	}

	path := cfg.PrivateKeyFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(cortexDir, path)
	}

	seed, err := loadSidecarLine(path, "signing key file")
	if err != nil {
		return SigningKey{}, err
	}

	priv, err := eventsig.PrivateFromSeed(seed)
	if err != nil {
		return SigningKey{}, fmt.Errorf("parsing signing key %s: %w", path, err)
	}

	pub := eventsig.EncodePublic(priv.Public().(ed25519.PublicKey))
	if cfg.PublicKey != "" && !eventsig.PublicKeysEqual(cfg.PublicKey, pub) {
		return SigningKey{}, fmt.Errorf(
			"signing key mismatch: cortex.md public_key does not match the key derived from %s "+
				"(re-run `noema keygen --force` to regenerate, or fix public_key)",
			path,
		)
	}

	return SigningKey{Private: priv, Public: pub}, nil
}
