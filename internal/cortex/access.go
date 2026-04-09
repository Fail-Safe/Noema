package cortex

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AccessKeyEnvVar is the environment variable that overrides
// cortex.md's access.shared_key_file. When set to a non-empty, non-
// whitespace value it is used as the MCP shared key, and any configured
// file path is recorded so the caller can warn about the override.
const AccessKeyEnvVar = "NOEMA_MCP_KEY"

// accessKeyMaxBytes caps the size of the sidecar key file. Anything
// larger is a startup error — a cheap defense against "I pasted an
// entire PEM into the file by accident."
const accessKeyMaxBytes = 4 * 1024

// AccessKey is the resolved shared key for the HTTP MCP endpoint, along
// with metadata about where it came from and a non-secret fingerprint.
// The zero value represents open mode (no authentication).
type AccessKey struct {
	// Value is the raw bearer token. It must never be logged, written to
	// the event log, serialised to MCP responses, or echoed in error
	// messages. Only the Fingerprint is safe to surface.
	Value string

	// Source is "env" (NOEMA_MCP_KEY), "file" (read from disk), or ""
	// (open mode).
	Source string

	// Path is the absolute file path the key was read from when
	// Source == "file", or the configured-but-overridden file path when
	// Source == "env" and the manifest also declared a shared_key_file.
	// Empty when neither applies.
	Path string

	// Fingerprint is a non-secret SHA-256 digest of Value, formatted
	// SSH-style (e.g. SHA256:a3:f1:...:c2). Safe to log and display.
	Fingerprint string
}

// Keyed reports whether shared-key mode is active.
func (k AccessKey) Keyed() bool { return k.Value != "" }

// EnvOverride reports whether NOEMA_MCP_KEY took precedence over a
// configured access.shared_key_file. Callers use this to emit the
// one-line warning on startup.
func (k AccessKey) EnvOverride() bool {
	return k.Source == "env" && k.Path != ""
}

// LoadAccessKey resolves the active MCP shared key for a cortex.
//
// Resolution order (highest priority first):
//  1. NOEMA_MCP_KEY — if set to a non-empty value, wins. The configured
//     file path is still recorded in AccessKey.Path so the caller can
//     log that the env var overrode it.
//  2. cfg.SharedKeyFile — read relative to cortexDir unless already
//     absolute. The file is validated for permissions, size, and
//     format (see loadKeyFile).
//  3. Open mode — returns the zero AccessKey with a nil error.
//
// Errors are returned when the env var is set to only whitespace, when
// a configured file cannot be read, when permissions are looser than
// 0600, when the file exceeds 4 KiB, when it is empty or whitespace
// only, or when it contains two or more non-empty lines.
func LoadAccessKey(cortexDir string, cfg *AccessConfig) (AccessKey, error) {
	configuredPath := ""
	if cfg != nil && cfg.SharedKeyFile != "" {
		configuredPath = cfg.SharedKeyFile
		if !filepath.IsAbs(configuredPath) {
			configuredPath = filepath.Join(cortexDir, configuredPath)
		}
	}

	if raw, ok := os.LookupEnv(AccessKeyEnvVar); ok && raw != "" {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return AccessKey{}, fmt.Errorf("%s is set but contains only whitespace", AccessKeyEnvVar)
		}
		return AccessKey{
			Value:       trimmed,
			Source:      "env",
			Path:        configuredPath,
			Fingerprint: KeyFingerprint(trimmed),
		}, nil
	}

	if configuredPath == "" {
		return AccessKey{}, nil
	}

	key, err := loadKeyFile(configuredPath)
	if err != nil {
		return AccessKey{}, err
	}
	return AccessKey{
		Value:       key,
		Source:      "file",
		Path:        configuredPath,
		Fingerprint: KeyFingerprint(key),
	}, nil
}

// loadKeyFile reads a sidecar key file, enforcing permissions and
// format rules. Returns the parsed key with surrounding whitespace
// stripped.
func loadKeyFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("reading access key file %s: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("access key file %s is a directory", path)
	}
	if info.Size() > accessKeyMaxBytes {
		return "", fmt.Errorf("access key file %s is %d bytes; maximum is %d", path, info.Size(), accessKeyMaxBytes)
	}
	// SSH-style permissions check: group/other bits must be zero. This
	// is a Unix-only guardrail — on Windows info.Mode().Perm() does not
	// carry POSIX bits, so the check is a no-op there (v1 is Unix-first).
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return "", fmt.Errorf(
			"refusing to read access key file %s: mode %#o is too permissive; chmod 600 %s",
			path, mode, path,
		)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading access key file %s: %w", path, err)
	}

	// Parse: first non-empty trimmed line is the key; two or more
	// non-empty lines is an error (v1 does not support multi-key
	// files). CRLF line endings are handled naturally because the
	// per-line TrimSpace strips the trailing \r.
	var key string
	nonEmpty := 0
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		nonEmpty++
		if nonEmpty == 1 {
			key = t
		}
	}
	switch {
	case nonEmpty == 0:
		return "", fmt.Errorf("access key file %s is empty or whitespace-only", path)
	case nonEmpty > 1:
		return "", fmt.Errorf(
			"access key file %s contains %d non-empty lines; v1 supports a single key per file",
			path, nonEmpty,
		)
	}
	return key, nil
}

// KeyFingerprint returns a non-secret SHA-256 fingerprint of an MCP
// shared key, formatted SSH-style as SHA256:<pair>:<pair>:... Safe to
// log, display in federation_status, and read aloud over an
// out-of-band channel when confirming a pairing.
func KeyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	hexed := hex.EncodeToString(sum[:])
	var b strings.Builder
	b.Grow(len("SHA256:") + len(hexed) + len(hexed)/2)
	b.WriteString("SHA256:")
	for i := 0; i < len(hexed); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(hexed[i : i+2])
	}
	return b.String()
}
