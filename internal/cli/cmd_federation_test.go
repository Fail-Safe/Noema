package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/config"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/federation"
)

// newCortexWithPeers builds a fresh on-disk cortex and writes a federation
// block to its manifest with the given peer names. We use this rather than
// reaching for setupV1Cortex because reset-peer is a v2-era operation —
// the cortex is already past identity migration when an operator runs it.
func newCortexWithPeers(t *testing.T, name string, peerNames ...string) *cortex.Cortex {
	t.Helper()
	parent := t.TempDir()
	if _, err := cortex.Create(name, parent); err != nil {
		t.Fatalf("cortex.Create: %v", err)
	}
	dir := filepath.Join(parent, name)

	m, err := cortex.ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	peers := make([]cortex.PeerEntry, 0, len(peerNames))
	for _, p := range peerNames {
		peers = append(peers, cortex.PeerEntry{
			Name:     p,
			Endpoint: "https://" + p + ".example:3000",
		})
	}
	m.Federation = &cortex.FederationConfig{Peers: peers}
	if err := cortex.WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	cx, err := cortex.Open(name, dir)
	if err != nil {
		t.Fatalf("cortex.Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return cx
}

// seedPeerState is the inverse of what reset-peer does: it writes a pinned
// cortex_id, a cursor, and a last_seen for the given peer so the test has
// something concrete to clear. The pinned id is what determines which
// vector-clock bucket reset-peer will drop, so callers can pass it in to
// also pre-seed the matching bucket.
func seedPeerState(t *testing.T, cx *cortex.Cortex, peer, pinnedID, cursor, lastSeen string) {
	t.Helper()
	st := federation.NewState(cx.DB.DB)
	if pinnedID != "" {
		if err := st.SetPeerCortexID(peer, pinnedID); err != nil {
			t.Fatalf("SetPeerCortexID: %v", err)
		}
	}
	if cursor != "" {
		if err := st.SetPeerCursor(peer, cursor); err != nil {
			t.Fatalf("SetPeerCursor: %v", err)
		}
	}
	if lastSeen != "" {
		if err := st.SetPeerSeen(peer, lastSeen); err != nil {
			t.Fatalf("SetPeerSeen: %v", err)
		}
	}
}

// endpointMap mirrors the map runFederationResetPeer expects from its
// caller. Tests construct it from the manifest the same way the cobra
// command does, so the test exercises the same data flow.
func endpointMap(cx *cortex.Cortex, t *testing.T) map[string]string {
	t.Helper()
	m, err := cortex.ReadManifest(cx.Dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	out := map[string]string{}
	if m.Federation != nil {
		for _, p := range m.Federation.Peers {
			out[p.Name] = p.Endpoint
		}
	}
	return out
}

// TestFederationResetPeer_ClearsAllState pins the headline behavior: a peer
// with full state present (pin + cursor + last_seen + a vector-clock bucket
// keyed on the pinned id) loses all four after a successful reset. This is
// the exact scenario in the recent peer-a/peer-b/peer-c incident — without this
// test the next refactor could quietly skip clearing the vclock bucket and
// nobody would notice until federation started reporting ghost dominance.
func TestFederationResetPeer_ClearsAllState(t *testing.T) {
	cx := newCortexWithPeers(t, "alpha", "beta")

	const pinned = "01PEER000000000000000BETA00"
	seedPeerState(t, cx, "beta", pinned, "01EVENT00000000000000000001", "2026-04-07T12:00:00Z")

	// Seed a vclock bucket under the pinned id and one under an unrelated
	// id so we can prove reset-peer drops only the matching bucket.
	st := federation.NewState(cx.DB.DB)
	if err := st.SetClock(federation.VClock{
		pinned:                        7,
		"01OTHER0000000000000000001": 3,
	}); err != nil {
		t.Fatalf("SetClock: %v", err)
	}

	var out bytes.Buffer
	if err := runFederationResetPeer(&out, strings.NewReader("y\n"), cx, []string{"beta"}, endpointMap(cx, t), false); err != nil {
		t.Fatalf("runFederationResetPeer: %v\noutput:\n%s", err, out.String())
	}

	// Pin, cursor, and last_seen must be gone.
	for _, key := range []string{
		federation.PeerCortexIDKey("beta"),
		federation.PeerCursorKey("beta"),
		federation.PeerSeenKey("beta"),
	} {
		val, err := st.Get(key)
		if err != nil {
			t.Fatalf("Get %s: %v", key, err)
		}
		if val != "" {
			t.Errorf("federation_state[%s] = %q, want empty", key, val)
		}
	}

	// The vclock should have lost the bucket under the (now-gone) pinned
	// id, but the unrelated bucket must remain — clearing one peer must
	// never touch another peer's causal history.
	vc, err := st.GetClock()
	if err != nil {
		t.Fatalf("GetClock: %v", err)
	}
	if _, lingering := vc[pinned]; lingering {
		t.Errorf("vclock still has bucket under pinned id %q: %v", pinned, vc)
	}
	if vc["01OTHER0000000000000000001"] != 3 {
		t.Errorf("unrelated vclock bucket was disturbed: %v", vc)
	}

	// Output should mention what was cleared so the operator has visible
	// confirmation rather than a silent success.
	output := out.String()
	if !strings.Contains(output, "state cleared") {
		t.Errorf("output missing 'state cleared' summary:\n%s", output)
	}
	if !strings.Contains(output, "vector-clock buckets dropped: 1") {
		t.Errorf("output missing vclock bucket count:\n%s", output)
	}
}

// TestFederationResetPeer_RejectsUnknownPeer pins the typo guard. A reset
// against an unknown peer name has to fail loudly with a list of known
// peers — silently no-op'ing or matching by prefix would let an operator
// think they cleared state they did not, and they'd find out next time
// the syncer ran (or worse, after divergence had already accumulated).
func TestFederationResetPeer_RejectsUnknownPeer(t *testing.T) {
	cx := newCortexWithPeers(t, "alpha", "beta", "gamma")

	// Seed beta so the test can also prove a failed call doesn't touch
	// the database — atomicity matters when the operator might be
	// running this at 3am to unblock federation.
	const pinned = "01PEER000000000000000BETA00"
	seedPeerState(t, cx, "beta", pinned, "01EVENT00000000000000000001", "2026-04-07T12:00:00Z")

	var out bytes.Buffer
	cmd := federationResetPeerCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	// Drive through cobra so the validation that lives in RunE — not
	// runFederationResetPeer — is what we're actually testing.
	cmd.SetArgs([]string{"delta"})

	// resolveCortex() reads the global config, so we need to wire it up.
	withConfigForCortex(t, cx)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown peer, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, `peer "delta" is not configured`) {
		t.Errorf("error does not name the unknown peer: %v", err)
	}
	// Known peers must be listed so the operator can spot a typo.
	if !strings.Contains(msg, "beta") || !strings.Contains(msg, "gamma") {
		t.Errorf("error does not list known peers: %v", err)
	}

	// Beta's state must still be intact — a failed reset must never
	// have touched the DB.
	st := federation.NewState(cx.DB.DB)
	got, _ := st.Get(federation.PeerCortexIDKey("beta"))
	if got != pinned {
		t.Errorf("beta pin was disturbed by failed reset: %q", got)
	}
}

// TestFederationResetPeer_NoStateNoOp pins the "peer was configured but
// never contacted" branch. The reset is still successful (so a script
// running it across every peer doesn't fail on the brand-new ones), but
// the output has to make clear nothing was actually cleared so the
// operator doesn't think they fixed a problem they didn't.
func TestFederationResetPeer_NoStateNoOp(t *testing.T) {
	cx := newCortexWithPeers(t, "alpha", "beta")

	var out bytes.Buffer
	if err := runFederationResetPeer(&out, strings.NewReader("y\n"), cx, []string{"beta"}, endpointMap(cx, t), false); err != nil {
		t.Fatalf("runFederationResetPeer: %v\noutput:\n%s", err, out.String())
	}

	output := out.String()
	if !strings.Contains(output, "nothing to clear") {
		t.Errorf("expected 'nothing to clear' message, got:\n%s", output)
	}
	// Without a pinned id we can't know which vclock bucket belonged to
	// this peer, so the bucket-drop count should not appear.
	if strings.Contains(output, "vector-clock buckets dropped") {
		t.Errorf("unexpected bucket-drop line for never-contacted peer:\n%s", output)
	}
}

// TestFederationResetPeer_AbortLeavesStateIntact pins the prompt path.
// Answering "n" must abort the operation AND leave every byte of state
// where it was — this is the operator's safety net against typing the
// wrong peer name and noticing only at the prompt.
func TestFederationResetPeer_AbortLeavesStateIntact(t *testing.T) {
	cx := newCortexWithPeers(t, "alpha", "beta")
	const pinned = "01PEER000000000000000BETA00"
	seedPeerState(t, cx, "beta", pinned, "01EVENT00000000000000000001", "2026-04-07T12:00:00Z")

	var out bytes.Buffer
	err := runFederationResetPeer(&out, strings.NewReader("n\n"), cx, []string{"beta"}, endpointMap(cx, t), false)
	if err == nil {
		t.Fatal("expected error on user abort, got nil")
	}

	st := federation.NewState(cx.DB.DB)
	if got, _ := st.Get(federation.PeerCortexIDKey("beta")); got != pinned {
		t.Errorf("pin was cleared despite abort: %q", got)
	}
	if got, _ := st.Get(federation.PeerCursorKey("beta")); got == "" {
		t.Errorf("cursor was cleared despite abort")
	}
}

// TestFederationResetPeer_MultiplePeers pins that the command can clear
// multiple peers in one invocation, and that each peer's vclock bucket
// is dropped independently. This is the common case after a multi-host
// migration: an operator wants to reset every stale peer with one
// command rather than running it N times.
func TestFederationResetPeer_MultiplePeers(t *testing.T) {
	cx := newCortexWithPeers(t, "alpha", "beta", "gamma")

	const betaPin = "01PEER000000000000000BETA00"
	const gammaPin = "01PEER0000000000000GAMMA00"
	seedPeerState(t, cx, "beta", betaPin, "01EVENT0000000000000000B01", "2026-04-07T12:00:00Z")
	seedPeerState(t, cx, "gamma", gammaPin, "01EVENT0000000000000000G01", "2026-04-07T12:00:00Z")

	st := federation.NewState(cx.DB.DB)
	if err := st.SetClock(federation.VClock{
		betaPin:  4,
		gammaPin: 9,
	}); err != nil {
		t.Fatalf("SetClock: %v", err)
	}

	var out bytes.Buffer
	if err := runFederationResetPeer(&out, strings.NewReader("y\n"), cx, []string{"beta", "gamma"}, endpointMap(cx, t), false); err != nil {
		t.Fatalf("runFederationResetPeer: %v\noutput:\n%s", err, out.String())
	}

	vc, err := st.GetClock()
	if err != nil {
		t.Fatalf("GetClock: %v", err)
	}
	if len(vc) != 0 {
		t.Errorf("vclock should be empty after dropping both buckets, got: %v", vc)
	}
	if !strings.Contains(out.String(), "vector-clock buckets dropped: 2") {
		t.Errorf("expected count of 2 bucket drops, got:\n%s", out.String())
	}
}

// withConfigForCortex sandboxes HOME so config.Load/Save touches the test
// directory and registers cx as the default. Used by tests that drive the
// reset command through cobra (which calls resolveCortex internally).
func withConfigForCortex(t *testing.T, cx *cortex.Cortex) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("NOEMA_CORTEX", "")

	prevFlag := cortexFlag
	cortexFlag = ""
	t.Cleanup(func() { cortexFlag = prevFlag })

	// Build config pointing at the existing on-disk cortex. We don't go
	// through `noema use` because that's a separate code path; we just
	// need resolveCortex() to find the cortex by name.
	m, _ := cortex.ReadManifest(cx.Dir)
	cfg := &config.Config{
		Default: cx.Name,
		Cortexes: map[string]config.CortexEntry{
			cx.Name: {Path: cx.Dir, ID: m.ID},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("cfg.Save: %v", err)
	}
}

// writeSidecarKeyFile drops a sidecar key file at <cortexDir>/<rel> with
// mode 0o600 and rewrites cortex.md so access.shared_key_file points at
// it. Tests use this to exercise the file-source branch of the
// fingerprint command without plumbing a whole file-loader fake.
func writeSidecarKeyFile(t *testing.T, cx *cortex.Cortex, rel, key string) {
	t.Helper()
	path := filepath.Join(cx.Dir, rel)
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		t.Fatalf("write sidecar %s: %v", path, err)
	}
	m, err := cortex.ReadManifest(cx.Dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	m.Access = &cortex.AccessConfig{SharedKeyFile: rel}
	if err := cortex.WriteManifest(cx.Dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
}

// ---- federation status (access line) ----

// TestFederationStatus_ShowsAccessLineOpenMode pins that `noema
// federation status` surfaces the active MCP access posture on every
// run, even on a cortex with no peers. The access line has to come
// before the "Federation: not configured" message so an operator
// inspecting a fresh cortex can tell keyed vs open at a glance.
func TestFederationStatus_ShowsAccessLineOpenMode(t *testing.T) {
	cx := newCortexWithPeers(t, "solo") // no peers variadic arg
	withConfigForCortex(t, cx)
	t.Setenv(cortex.AccessKeyEnvVar, "")

	// Capture os.Stdout. federationStatusCmd prints through fmt.Printf
	// directly rather than cmd.OutOrStdout, so we have to redirect
	// os.Stdout. The alternative (plumbing an io.Writer through the
	// RunE) is out of scope for PR (c).
	out := captureStdout(t, func() {
		cmd := federationStatusCmd()
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(out, "Access: open") {
		t.Errorf("output missing 'Access: open' line:\n%s", out)
	}
	// Ordering matters: Access line must come before the federation
	// block so the top-of-output summary is always consistent.
	accessIdx := strings.Index(out, "Access:")
	fedIdx := strings.Index(out, "Federation:")
	if accessIdx < 0 || fedIdx < 0 || accessIdx > fedIdx {
		t.Errorf("Access line must appear before Federation line:\n%s", out)
	}
}

// TestFederationStatus_ShowsAccessLineKeyedMode pins that env-sourced
// keyed mode is surfaced with source + fingerprint and, critically,
// that the raw key does not leak into status output. `noema
// federation status` is the thing operators are likely to paste into
// a bug report or support channel, so the "no raw key" invariant
// matters more here than anywhere else in the codebase.
func TestFederationStatus_ShowsAccessLineKeyedMode(t *testing.T) {
	cx := newCortexWithPeers(t, "solo")
	withConfigForCortex(t, cx)
	const key = "not-a-real-key-for-tests"
	t.Setenv(cortex.AccessKeyEnvVar, key)

	out := captureStdout(t, func() {
		cmd := federationStatusCmd()
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(out, "Access: keyed") {
		t.Errorf("output missing 'Access: keyed' line:\n%s", out)
	}
	if !strings.Contains(out, "source=env") {
		t.Errorf("output missing source=env:\n%s", out)
	}
	if !strings.Contains(out, cortex.KeyFingerprint(key)) {
		t.Errorf("output missing fingerprint:\n%s", out)
	}
	if strings.Contains(out, key) {
		t.Errorf("SECURITY: raw key leaked in status output:\n%s", out)
	}
}

// captureStdout redirects os.Stdout for the duration of f and returns
// everything written to it. federationStatusCmd bypasses cmd.OutOrStdout
// in favor of fmt.Printf, so this indirection is the only way to
// assert on its output without rewriting the command.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()
	f()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// ---- federation key fingerprint ----

// TestFederationKeyFingerprint_OpenMode pins that a cortex with no access
// config and no NOEMA_MCP_KEY reports open mode rather than erroring.
// This is the "query whether I'm keyed" use case — the command must be
// callable on any cortex and must answer truthfully without tripping on
// the absence of a key.
func TestFederationKeyFingerprint_OpenMode(t *testing.T) {
	cx := newCortexWithPeers(t, "alpha")
	withConfigForCortex(t, cx)
	t.Setenv(cortex.AccessKeyEnvVar, "")

	var out bytes.Buffer
	cmd := federationKeyFingerprintCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "open mode") {
		t.Errorf("output does not report open mode:\n%s", s)
	}
	if strings.Contains(s, "Fingerprint:") {
		t.Errorf("open mode output must not print a Fingerprint line:\n%s", s)
	}
}

// TestFederationKeyFingerprint_EnvKey pins the env-sourced path: when
// NOEMA_MCP_KEY is set, the command reports Source: env and the
// fingerprint. The exact fingerprint is derived from cortex.KeyFingerprint
// so we can assert on it byte-for-byte without duplicating the SHA-256
// logic in the test.
func TestFederationKeyFingerprint_EnvKey(t *testing.T) {
	cx := newCortexWithPeers(t, "alpha")
	withConfigForCortex(t, cx)
	const key = "test-shared-key-123"
	t.Setenv(cortex.AccessKeyEnvVar, key)

	var out bytes.Buffer
	cmd := federationKeyFingerprintCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, out.String())
	}
	s := out.String()

	for _, want := range []string{
		"Source:      env",
		"Fingerprint: " + cortex.KeyFingerprint(key),
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	// Critical: the raw key must never appear in output. This is the
	// whole point of the fingerprint — it's the thing you can say over
	// Signal without compromising the secret.
	if strings.Contains(s, key) {
		t.Errorf("SECURITY: raw key leaked in fingerprint output:\n%s", s)
	}
}

// TestFederationKeyFingerprint_FileKey pins the sidecar-file path: a
// cortex with access.shared_key_file pointing at a valid sidecar resolves
// to Source: file and the correct fingerprint, and also surfaces the
// absolute Path so an operator can see which file is in play.
func TestFederationKeyFingerprint_FileKey(t *testing.T) {
	cx := newCortexWithPeers(t, "alpha")
	withConfigForCortex(t, cx)
	t.Setenv(cortex.AccessKeyEnvVar, "")

	const key = "file-sourced-key-abc"
	writeSidecarKeyFile(t, cx, ".access.secret", key)

	var out bytes.Buffer
	cmd := federationKeyFingerprintCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, out.String())
	}
	s := out.String()

	for _, want := range []string{
		"Source:      file",
		"Fingerprint: " + cortex.KeyFingerprint(key),
		".access.secret", // Path line includes the filename
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, key) {
		t.Errorf("SECURITY: raw key leaked in fingerprint output:\n%s", s)
	}
}

// TestFederationKeyFingerprint_EnvOverridesFile pins the override-warning
// branch: when both a sidecar file AND NOEMA_MCP_KEY are present, the
// env wins (that's the design) but the command must surface the
// override so the operator knows why the sidecar file they thought
// was active isn't. Without this the override is silent and the next
// operator inherits a confusing "why is my fingerprint wrong" puzzle.
func TestFederationKeyFingerprint_EnvOverridesFile(t *testing.T) {
	cx := newCortexWithPeers(t, "alpha")
	withConfigForCortex(t, cx)

	const fileKey = "from-file"
	const envKey = "from-env-wins"
	writeSidecarKeyFile(t, cx, ".access.secret", fileKey)
	t.Setenv(cortex.AccessKeyEnvVar, envKey)

	var out bytes.Buffer
	cmd := federationKeyFingerprintCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput:\n%s", err, out.String())
	}
	s := out.String()

	if !strings.Contains(s, "Source:      env") {
		t.Errorf("expected env source:\n%s", s)
	}
	// The reported fingerprint must match the env key, not the file
	// key — the whole point of this test is that env wins.
	if !strings.Contains(s, cortex.KeyFingerprint(envKey)) {
		t.Errorf("fingerprint does not match env key:\n%s", s)
	}
	if strings.Contains(s, cortex.KeyFingerprint(fileKey)) {
		t.Errorf("fingerprint unexpectedly matches overridden file key:\n%s", s)
	}
	// The override note is the "loud failure" — without it the
	// operator can't tell why their sidecar file isn't being used.
	if !strings.Contains(s, "NOEMA_MCP_KEY is overriding") {
		t.Errorf("output missing env-override warning:\n%s", s)
	}
}
