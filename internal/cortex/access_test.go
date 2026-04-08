package cortex_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

// writeKeyFile writes contents to a file in dir and chmods it to mode.
// The file is always opened with 0o600 first (so target modes like
// 0o000 or 0o400 that would prevent the O_WRONLY|O_CREAT still work),
// then re-chmoded to the requested value. Returns the absolute path.
func writeKeyFile(t *testing.T, dir, name, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	// If the file already exists from a prior subtest with restrictive
	// perms, make it writable again before WriteFile tries to truncate.
	if _, err := os.Stat(path); err == nil {
		_ = os.Chmod(path, 0o600)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod %s: %v", path, err)
	}
	return path
}

// ---- LoadAccessKey ----

func TestLoadAccessKey_OpenMode_NoConfigNoEnv(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "")
	key, err := cortex.LoadAccessKey(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("LoadAccessKey: %v", err)
	}
	if key.Keyed() {
		t.Errorf("expected open mode, got keyed (Value=%q Source=%q)", key.Value, key.Source)
	}
	if key.Source != "" {
		t.Errorf("Source=%q, want empty", key.Source)
	}
}

func TestLoadAccessKey_OpenMode_EmptyConfigNoEnv(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "")
	cfg := &cortex.AccessConfig{} // SharedKeyFile == ""
	key, err := cortex.LoadAccessKey(t.TempDir(), cfg)
	if err != nil {
		t.Fatalf("LoadAccessKey: %v", err)
	}
	if key.Keyed() {
		t.Errorf("expected open mode with empty SharedKeyFile, got keyed")
	}
}

func TestLoadAccessKey_EnvOnly(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "my-env-secret")
	key, err := cortex.LoadAccessKey(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("LoadAccessKey: %v", err)
	}
	if key.Value != "my-env-secret" {
		t.Errorf("Value=%q, want my-env-secret", key.Value)
	}
	if key.Source != "env" {
		t.Errorf("Source=%q, want env", key.Source)
	}
	if key.Path != "" {
		t.Errorf("Path=%q, want empty (no configured file)", key.Path)
	}
	if key.EnvOverride() {
		t.Errorf("EnvOverride should be false when no file was configured")
	}
	if !strings.HasPrefix(key.Fingerprint, "SHA256:") {
		t.Errorf("Fingerprint=%q, want SHA256: prefix", key.Fingerprint)
	}
}

func TestLoadAccessKey_EnvTrimsSurroundingWhitespace(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "  padded-key  \n")
	key, err := cortex.LoadAccessKey(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("LoadAccessKey: %v", err)
	}
	if key.Value != "padded-key" {
		t.Errorf("Value=%q, want padded-key", key.Value)
	}
}

func TestLoadAccessKey_EnvWhitespaceOnly(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "   \t\n ")
	_, err := cortex.LoadAccessKey(t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected error for whitespace-only env var")
	}
	if !strings.Contains(err.Error(), cortex.AccessKeyEnvVar) {
		t.Errorf("error should mention env var name, got %q", err.Error())
	}
}

func TestLoadAccessKey_FileOnly(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "")
	dir := t.TempDir()
	path := writeKeyFile(t, dir, ".access.secret", "file-secret\n", 0o600)
	cfg := &cortex.AccessConfig{SharedKeyFile: ".access.secret"}

	key, err := cortex.LoadAccessKey(dir, cfg)
	if err != nil {
		t.Fatalf("LoadAccessKey: %v", err)
	}
	if key.Value != "file-secret" {
		t.Errorf("Value=%q, want file-secret", key.Value)
	}
	if key.Source != "file" {
		t.Errorf("Source=%q, want file", key.Source)
	}
	if key.Path != path {
		t.Errorf("Path=%q, want %q", key.Path, path)
	}
}

func TestLoadAccessKey_EnvOverridesFile(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "env-wins")
	dir := t.TempDir()
	path := writeKeyFile(t, dir, ".access.secret", "file-value", 0o600)
	cfg := &cortex.AccessConfig{SharedKeyFile: ".access.secret"}

	key, err := cortex.LoadAccessKey(dir, cfg)
	if err != nil {
		t.Fatalf("LoadAccessKey: %v", err)
	}
	if key.Value != "env-wins" {
		t.Errorf("Value=%q, want env-wins (env must override file)", key.Value)
	}
	if key.Source != "env" {
		t.Errorf("Source=%q, want env", key.Source)
	}
	if key.Path != path {
		t.Errorf("Path=%q, want %q (configured path must be recorded even on override)", key.Path, path)
	}
	if !key.EnvOverride() {
		t.Errorf("EnvOverride should be true when env overrides a configured file")
	}
}

func TestLoadAccessKey_FileMissing(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "")
	dir := t.TempDir()
	cfg := &cortex.AccessConfig{SharedKeyFile: ".does-not-exist"}

	_, err := cortex.LoadAccessKey(dir, cfg)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadAccessKey_FilePermissionsTooLoose(t *testing.T) {
	if isWindows() {
		t.Skip("POSIX permission check is a no-op on Windows")
	}
	t.Setenv(cortex.AccessKeyEnvVar, "")
	dir := t.TempDir()
	writeKeyFile(t, dir, ".access.secret", "secret", 0o644)
	cfg := &cortex.AccessConfig{SharedKeyFile: ".access.secret"}

	_, err := cortex.LoadAccessKey(dir, cfg)
	if err == nil {
		t.Fatal("expected error for 0644 permissions")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error should suggest chmod 600, got %q", err.Error())
	}
}

func TestLoadAccessKey_FilePermissionsAcceptable(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "")
	dir := t.TempDir()
	// Both 0600 and 0400 (read-only) must be accepted.
	for _, mode := range []os.FileMode{0o600, 0o400, 0o000} {
		t.Run(mode.String(), func(t *testing.T) {
			writeKeyFile(t, dir, ".access.secret", "ok", mode)
			cfg := &cortex.AccessConfig{SharedKeyFile: ".access.secret"}
			// 0000 means the owner can't read either; os.ReadFile will fail.
			// That's a legitimate error, not a permissions-too-loose error.
			key, err := cortex.LoadAccessKey(dir, cfg)
			if mode == 0o000 {
				if err == nil {
					t.Fatal("expected read error for mode 0000")
				}
				if strings.Contains(err.Error(), "chmod 600") {
					t.Errorf("0000 should not trigger the too-permissive check, got %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadAccessKey mode=%v: %v", mode, err)
			}
			if key.Value != "ok" {
				t.Errorf("Value=%q, want ok", key.Value)
			}
		})
	}
}

func TestLoadAccessKey_FileTooLarge(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "")
	dir := t.TempDir()
	big := strings.Repeat("a", 4*1024+1) // 4 KiB + 1 byte
	writeKeyFile(t, dir, ".access.secret", big, 0o600)
	cfg := &cortex.AccessConfig{SharedKeyFile: ".access.secret"}

	_, err := cortex.LoadAccessKey(dir, cfg)
	if err == nil {
		t.Fatal("expected error for file > 4 KiB")
	}
	if !strings.Contains(err.Error(), "maximum") {
		t.Errorf("error should mention the maximum, got %q", err.Error())
	}
}

func TestLoadAccessKey_FileWhitespaceOnly(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "")
	dir := t.TempDir()
	writeKeyFile(t, dir, ".access.secret", "   \n\t\n  \n", 0o600)
	cfg := &cortex.AccessConfig{SharedKeyFile: ".access.secret"}

	_, err := cortex.LoadAccessKey(dir, cfg)
	if err == nil {
		t.Fatal("expected error for whitespace-only file")
	}
	if !strings.Contains(err.Error(), "empty or whitespace") {
		t.Errorf("error should mention empty/whitespace, got %q", err.Error())
	}
}

func TestLoadAccessKey_FileMultipleNonEmptyLines(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "")
	dir := t.TempDir()
	writeKeyFile(t, dir, ".access.secret", "first-key\nsecond-key\n", 0o600)
	cfg := &cortex.AccessConfig{SharedKeyFile: ".access.secret"}

	_, err := cortex.LoadAccessKey(dir, cfg)
	if err == nil {
		t.Fatal("expected error for file with two non-empty lines")
	}
	if !strings.Contains(err.Error(), "non-empty lines") {
		t.Errorf("error should mention non-empty lines, got %q", err.Error())
	}
}

func TestLoadAccessKey_FileCRLFLineEndings(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "")
	dir := t.TempDir()
	writeKeyFile(t, dir, ".access.secret", "\r\nkey-with-crlf\r\n", 0o600)
	cfg := &cortex.AccessConfig{SharedKeyFile: ".access.secret"}

	key, err := cortex.LoadAccessKey(dir, cfg)
	if err != nil {
		t.Fatalf("LoadAccessKey: %v", err)
	}
	if key.Value != "key-with-crlf" {
		t.Errorf("Value=%q, want key-with-crlf (CRLF stripping failed)", key.Value)
	}
}

func TestLoadAccessKey_FileLeadingBlankLines(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "")
	dir := t.TempDir()
	writeKeyFile(t, dir, ".access.secret", "\n\n  \nkey\n", 0o600)
	cfg := &cortex.AccessConfig{SharedKeyFile: ".access.secret"}

	key, err := cortex.LoadAccessKey(dir, cfg)
	if err != nil {
		t.Fatalf("LoadAccessKey: %v", err)
	}
	if key.Value != "key" {
		t.Errorf("Value=%q, want key (leading blank lines must be skipped)", key.Value)
	}
}

func TestLoadAccessKey_FileAbsolutePath(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "")
	secretDir := t.TempDir()
	abs := writeKeyFile(t, secretDir, "abs.secret", "abs-key", 0o600)

	cortexDir := t.TempDir() // different dir
	cfg := &cortex.AccessConfig{SharedKeyFile: abs}

	key, err := cortex.LoadAccessKey(cortexDir, cfg)
	if err != nil {
		t.Fatalf("LoadAccessKey: %v", err)
	}
	if key.Value != "abs-key" {
		t.Errorf("Value=%q, want abs-key", key.Value)
	}
	if key.Path != abs {
		t.Errorf("Path=%q, want %q", key.Path, abs)
	}
}

func TestLoadAccessKey_FileRelativePathResolvedAgainstCortexDir(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "")
	dir := t.TempDir()
	sub := filepath.Join(dir, "subdir")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	writeKeyFile(t, sub, "nested.secret", "nested", 0o600)
	cfg := &cortex.AccessConfig{SharedKeyFile: filepath.Join("subdir", "nested.secret")}

	key, err := cortex.LoadAccessKey(dir, cfg)
	if err != nil {
		t.Fatalf("LoadAccessKey: %v", err)
	}
	if key.Value != "nested" {
		t.Errorf("Value=%q, want nested", key.Value)
	}
}

func TestLoadAccessKey_FileIsDirectory(t *testing.T) {
	t.Setenv(cortex.AccessKeyEnvVar, "")
	dir := t.TempDir()
	sub := filepath.Join(dir, "a-dir")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &cortex.AccessConfig{SharedKeyFile: "a-dir"}

	_, err := cortex.LoadAccessKey(dir, cfg)
	if err == nil {
		t.Fatal("expected error when SharedKeyFile points at a directory")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error should mention directory, got %q", err.Error())
	}
}

// ---- KeyFingerprint ----

func TestKeyFingerprint_Deterministic(t *testing.T) {
	a := cortex.KeyFingerprint("same-key")
	b := cortex.KeyFingerprint("same-key")
	if a != b {
		t.Errorf("fingerprint not deterministic: %q vs %q", a, b)
	}
}

func TestKeyFingerprint_DifferentKeysDiffer(t *testing.T) {
	if cortex.KeyFingerprint("a") == cortex.KeyFingerprint("b") {
		t.Error("fingerprints for different keys should differ")
	}
}

func TestKeyFingerprint_Format(t *testing.T) {
	fp := cortex.KeyFingerprint("any-key")
	if !strings.HasPrefix(fp, "SHA256:") {
		t.Errorf("fingerprint %q missing SHA256: prefix", fp)
	}
	// 32 bytes → 64 hex chars → 32 colon-separated pairs.
	pairs := strings.Split(strings.TrimPrefix(fp, "SHA256:"), ":")
	if len(pairs) != 32 {
		t.Errorf("expected 32 pairs, got %d (fingerprint=%q)", len(pairs), fp)
	}
	for _, p := range pairs {
		if len(p) != 2 {
			t.Errorf("pair %q is not 2 hex chars", p)
		}
	}
}

func TestKeyFingerprint_NeverEqualToSecret(t *testing.T) {
	// Sanity check: the fingerprint must not contain the plaintext key.
	const secret = "my-super-secret-password"
	fp := cortex.KeyFingerprint(secret)
	if strings.Contains(fp, secret) {
		t.Errorf("fingerprint %q leaks the secret", fp)
	}
}

// isWindows reports whether the test is running on Windows; the POSIX
// permission check in loadKeyFile is a no-op there.
func isWindows() bool {
	return os.PathSeparator == '\\'
}
