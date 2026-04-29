package cli

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// setupServeLogging resolves the log destination for a `noema serve`
// invocation given the transport and the --log-file / --log-stderr
// flags. Returns the resolved file path (empty if logs should stay on
// stderr). The caller is responsible for actually opening the file and
// redirecting via redirectStderrToFile; keeping the two steps separate
// makes the path-resolution logic trivially unit-testable without
// touching process-level state like os.Stderr.
//
// Priority order:
//
//  1. --log-stderr takes precedence over everything, including
//     --log-file. Operators who explicitly want interactive logs
//     (tailing a crashlog during triage, wiring stderr into their
//     own logging pipeline, etc.) get that.
//  2. --log-file overrides the transport default.
//  3. Transport default: stdio redirects, http stays on stderr.
//     Stdio's default is $XDG_STATE_HOME/noema/<cortex>.log with the
//     spec's ~/.local/state fallback when XDG_STATE_HOME is unset.
func setupServeLogging(cortexName, transport, logFile string, logStderr bool) (string, error) {
	if logStderr {
		return "", nil
	}
	if logFile != "" {
		return logFile, nil
	}
	// stdio is the default transport. An empty string means stdio too.
	if transport == "" || transport == "stdio" {
		return defaultStdioLogPath(cortexName)
	}
	// http (and any future transport that streams over a network):
	// keep stderr. systemd / launchd / docker / journald already
	// capture it, and there's no interactive-terminal spill risk.
	return "", nil
}

// defaultStdioLogPath returns the default log destination for
// stdio-mode noema. Honors XDG_STATE_HOME per the spec, falling back
// to ~/.local/state. The cortex name is included in the filename so
// an operator running multiple stdio cortexes (one per MCP client)
// gets per-cortex logs instead of interleaved output.
func defaultStdioLogPath(cortexName string) (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home dir for default log path: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "noema", cortexName+".log"), nil
}

// redirectStderrToFile opens (or creates, appending) the file at path
// and redirects both the standard logger and os.Stderr to it. Reasons
// this has to touch both:
//
//  - log.Printf (used throughout internal/watch, internal/federation,
//    internal/consolidation) writes to the default logger's output,
//    which log.SetOutput switches cleanly.
//  - fmt.Fprintf(os.Stderr, ...) (used for startup messages and a
//    few ad-hoc notices in cmd_serve.go) reads os.Stderr at call time.
//    Reassigning the package-level variable is legal and affects all
//    subsequent callers that read os.Stderr dynamically.
//
// The log directory is created if it doesn't exist. File permissions
// are 0o640 so other local users on a shared host don't casually
// tail a running cortex's logs.
func redirectStderrToFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("opening log file %s: %w", path, err)
	}
	log.SetOutput(f)
	os.Stderr = f
	return nil
}
