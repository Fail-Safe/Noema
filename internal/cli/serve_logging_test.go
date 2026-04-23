package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetupServeLogging_LogStderrWins pins the --log-stderr behavior:
// it must override both --log-file and the stdio default. Operators
// rely on it during triage to keep logs interactive regardless of how
// a wrapper invoked noema.
func TestSetupServeLogging_LogStderrWins(t *testing.T) {
	got, err := setupServeLogging("cortex", "stdio", "/tmp/explicit.log", true)
	if err != nil {
		t.Fatalf("setupServeLogging: %v", err)
	}
	if got != "" {
		t.Errorf("path = %q, want empty (stderr)", got)
	}
}

// TestSetupServeLogging_ExplicitFile: --log-file overrides the stdio
// default but not --log-stderr.
func TestSetupServeLogging_ExplicitFile(t *testing.T) {
	got, err := setupServeLogging("cortex", "stdio", "/var/log/noema.log", false)
	if err != nil {
		t.Fatalf("setupServeLogging: %v", err)
	}
	if got != "/var/log/noema.log" {
		t.Errorf("path = %q, want /var/log/noema.log", got)
	}
}

// TestSetupServeLogging_StdioDefault: no flags means stdio routes to
// the XDG state path.
func TestSetupServeLogging_StdioDefault(t *testing.T) {
	// Isolate XDG_STATE_HOME so we don't pollute the user's real
	// state dir and don't pick up a stale env var from the shell.
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	got, err := setupServeLogging("agentbrain", "stdio", "", false)
	if err != nil {
		t.Fatalf("setupServeLogging: %v", err)
	}
	want := filepath.Join(tmp, "noema", "agentbrain.log")
	if got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// TestSetupServeLogging_EmptyTransportIsStdio: cobra defaults the
// flag to "stdio" but a library caller could pass the zero value;
// the logic must treat empty the same as "stdio".
func TestSetupServeLogging_EmptyTransportIsStdio(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	got, err := setupServeLogging("cortex", "", "", false)
	if err != nil {
		t.Fatalf("setupServeLogging: %v", err)
	}
	if !strings.HasPrefix(got, tmp) {
		t.Errorf("empty transport should route like stdio; got %q", got)
	}
}

// TestSetupServeLogging_HttpKeepsStderr: http mode intentionally
// leaves stderr alone so systemd/launchd/docker journald capture is
// unchanged from pre-feature behavior.
func TestSetupServeLogging_HttpKeepsStderr(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	got, err := setupServeLogging("cortex", "http", "", false)
	if err != nil {
		t.Fatalf("setupServeLogging: %v", err)
	}
	if got != "" {
		t.Errorf("http path = %q, want empty (stderr preserved)", got)
	}
}

// TestSetupServeLogging_HttpRespectsExplicitFile: operators running
// http under a process supervisor that doesn't capture stderr (bare
// nohup, daemonize, etc.) can still opt into file logging.
func TestSetupServeLogging_HttpRespectsExplicitFile(t *testing.T) {
	got, err := setupServeLogging("cortex", "http", "/var/log/n.log", false)
	if err != nil {
		t.Fatalf("setupServeLogging: %v", err)
	}
	if got != "/var/log/n.log" {
		t.Errorf("path = %q, want /var/log/n.log", got)
	}
}

// TestDefaultStdioLogPath_XDGFallback: when XDG_STATE_HOME is unset,
// the spec says fall back to ~/.local/state. Exercise that branch.
func TestDefaultStdioLogPath_XDGFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")

	got, err := defaultStdioLogPath("cortex")
	if err != nil {
		t.Fatalf("defaultStdioLogPath: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".local", "state", "noema", "cortex.log")
	if got != want {
		t.Errorf("fallback path = %q, want %q", got, want)
	}
}

// TestRedirectStderrToFile_AppendsNotOverwrites: a restart-heavy
// MCP workflow (Claude Code reloading its MCP config a few times an
// hour) must not silently truncate the log file each invocation.
func TestRedirectStderrToFile_AppendsNotOverwrites(t *testing.T) {
	// Save and restore os.Stderr so the test doesn't corrupt the go
	// test runner's own stderr stream.
	saved := os.Stderr
	t.Cleanup(func() { os.Stderr = saved })

	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte("prior line\n"), 0o640); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	if err := redirectStderrToFile(path); err != nil {
		t.Fatalf("redirectStderrToFile: %v", err)
	}
	if _, err := os.Stderr.WriteString("new line\n"); err != nil {
		t.Fatalf("stderr write: %v", err)
	}
	// Close the file we redirected to before reading back; otherwise
	// Windows refuses the read on the open handle.
	_ = os.Stderr.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "prior line") {
		t.Errorf("append mode truncated prior content: %q", data)
	}
	if !strings.Contains(string(data), "new line") {
		t.Errorf("redirect didn't reach file: %q", data)
	}
}
