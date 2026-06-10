package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

// writeTestManifest drops a minimal cortex.md with the given id into dir.
func writeTestManifest(t *testing.T, dir, id string) {
	t.Helper()
	md := "---\nname: test\ncreated: 2026-01-01T00:00:00Z\nversion: 2\n"
	if id != "" {
		md += "id: " + id + "\n"
	}
	md += "---\n"
	if err := os.WriteFile(filepath.Join(dir, "cortex.md"), []byte(md), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestRunKeygenGeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	writeTestManifest(t, dir, "01HV0000000000000000000CTX")

	var out bytes.Buffer
	if err := runKeygen(&out, "test", dir, false); err != nil {
		t.Fatalf("runKeygen: %v", err)
	}

	// The seed must never appear in user-facing output.
	m, err := cortex.ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Signing == nil || m.Signing.PublicKey == "" {
		t.Fatal("manifest signing block was not written")
	}
	if m.Signing.PrivateKeyFile != defaultSigningKeyFile {
		t.Fatalf("unexpected key file: %s", m.Signing.PrivateKeyFile)
	}

	// Sidecar exists with owner-only perms (Unix).
	keyPath := filepath.Join(dir, defaultSigningKeyFile)
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("sidecar missing: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("sidecar mode %#o, want 0600", perm)
		}
	}

	// The configured key must load and round-trip.
	k, err := cortex.LoadSigningKey(dir, m.Signing)
	if err != nil {
		t.Fatalf("LoadSigningKey after keygen: %v", err)
	}
	if k.Public != m.Signing.PublicKey {
		t.Fatal("loaded public key does not match manifest")
	}

	// Sanity: the secret seed is not echoed to the user.
	seed, _ := os.ReadFile(keyPath)
	seedLine := strings.TrimSpace(string(seed))
	if seedLine != "" && strings.Contains(out.String(), seedLine) {
		t.Fatal("keygen output leaked the private seed")
	}
}

func TestRunKeygenIdempotentWithoutForce(t *testing.T) {
	dir := t.TempDir()
	writeTestManifest(t, dir, "01HV0000000000000000000CTX")

	var out bytes.Buffer
	if err := runKeygen(&out, "test", dir, false); err != nil {
		t.Fatal(err)
	}
	m1, _ := cortex.ReadManifest(dir)

	out.Reset()
	if err := runKeygen(&out, "test", dir, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already has a signing key") {
		t.Fatalf("second run should be a no-op, got: %q", out.String())
	}
	m2, _ := cortex.ReadManifest(dir)
	if m1.Signing.PublicKey != m2.Signing.PublicKey {
		t.Fatal("idempotent run must not rotate the key")
	}
}

func TestRunKeygenForceRotates(t *testing.T) {
	dir := t.TempDir()
	writeTestManifest(t, dir, "01HV0000000000000000000CTX")

	var out bytes.Buffer
	if err := runKeygen(&out, "test", dir, false); err != nil {
		t.Fatal(err)
	}
	m1, _ := cortex.ReadManifest(dir)

	out.Reset()
	if err := runKeygen(&out, "test", dir, true); err != nil {
		t.Fatal(err)
	}
	m2, _ := cortex.ReadManifest(dir)
	if m1.Signing.PublicKey == m2.Signing.PublicKey {
		t.Fatal("--force must rotate to a new key")
	}
}

func TestRunKeygenRequiresCortexID(t *testing.T) {
	dir := t.TempDir()
	writeTestManifest(t, dir, "") // no id

	var out bytes.Buffer
	err := runKeygen(&out, "test", dir, false)
	if err == nil {
		t.Fatal("keygen without a cortex id should error")
	}
	if !strings.Contains(err.Error(), "migrate cortex-id") {
		t.Fatalf("error should point to the fix, got: %v", err)
	}
}
