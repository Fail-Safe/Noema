package cli

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

// TestValidateHTTPServe pins the flag invariants for `noema serve --transport
// http`. The most important case is the explicit-cortex requirement: silently
// inheriting the cortex from cfg.Default on a network transport is the failure
// mode that motivated this guard, and the test must keep firing if anyone
// weakens the check (e.g. re-adding a "but only if peers are configured"
// condition).
func TestValidateHTTPServe(t *testing.T) {
	const cortexName = "ai-1"

	cases := []struct {
		name           string
		host           string
		tlsCert        string
		tlsKey         string
		cortexExplicit bool
		wantErrSubstr  string // empty = expect nil
	}{
		{
			name:           "happy path no TLS",
			host:           "127.0.0.1",
			cortexExplicit: true,
		},
		{
			name:           "happy path with TLS",
			host:           "ai-1.example.com",
			tlsCert:        "/etc/ssl/cert.pem",
			tlsKey:         "/etc/ssl/key.pem",
			cortexExplicit: true,
		},
		{
			name:           "missing host",
			host:           "",
			cortexExplicit: true,
			wantErrSubstr:  "--host is required for HTTP transport",
		},
		{
			name:           "0.0.0.0 unspecified IPv4",
			host:           "0.0.0.0",
			cortexExplicit: true,
			wantErrSubstr:  "binding to 0.0.0.0 is not allowed",
		},
		{
			name:           "IPv6 unspecified",
			host:           "::",
			cortexExplicit: true,
			wantErrSubstr:  "is not allowed",
		},
		{
			name:           "TLS cert without key",
			host:           "127.0.0.1",
			tlsCert:        "/etc/ssl/cert.pem",
			cortexExplicit: true,
			wantErrSubstr:  "--tls-cert and --tls-key must be provided together",
		},
		{
			name:           "TLS key without cert",
			host:           "127.0.0.1",
			tlsKey:         "/etc/ssl/key.pem",
			cortexExplicit: true,
			wantErrSubstr:  "--tls-cert and --tls-key must be provided together",
		},
		{
			// The regression case: a cortex is bound (here "ai-1") but
			// --cortex was not on the command line. Even though host and
			// TLS look fine, the implicit binding is a federation
			// footgun and must be rejected.
			name:           "implicit cortex on HTTP",
			host:           "ai-1.example.com",
			tlsCert:        "/etc/ssl/cert.pem",
			tlsKey:         "/etc/ssl/key.pem",
			cortexExplicit: false,
			wantErrSubstr:  "without an explicit --cortex flag",
		},
		{
			// Specifically the agentbrain-on-ai-1 case from the field:
			// the bound cortex has no peers of its own, but is being
			// served on a host where another cortex's peers expect to
			// find a different identity. The guard must fire regardless
			// of the bound cortex's federation config.
			name:           "implicit cortex on HTTP includes cortex name in error",
			host:           "ai-1-tb.home-dns.com",
			cortexExplicit: false,
			wantErrSubstr:  "\"ai-1\"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHTTPServe(tc.host, tc.tlsCert, tc.tlsKey, cortexName, tc.cortexExplicit)
			if tc.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("expected nil, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

// --------- buildServeArgs ---------

// TestBuildServeArgs_OmitsEmptyOptionals pins the shape: mandatory args
// (serve, --cortex, --transport) are always present, optional args
// (host, port, tls) only appear when set. The reproducibility of the
// emitted command line depends on this — a unit file with a stale
// --tls-cert= argument would fail to start.
func TestBuildServeArgs_OmitsEmptyOptionals(t *testing.T) {
	// Minimal: just cortex + transport. Port 0 means "not set" so it
	// should NOT appear; callers using the real serve command always
	// have port=3000 by default, but the print functions can be called
	// with arbitrary values and must not emit --port 0.
	got := buildServeArgs("agentbrain", "http", "", 0, "", "")
	want := []string{"serve", "--cortex", "agentbrain", "--transport", "http"}
	if !equalSlices(got, want) {
		t.Errorf("minimal args: got %v, want %v", got, want)
	}

	// Full: every optional flag set.
	got = buildServeArgs("agentbrain", "http", "10.0.0.5", 3000, "/etc/ssl/cert.pem", "/etc/ssl/key.pem")
	want = []string{
		"serve", "--cortex", "agentbrain", "--transport", "http",
		"--host", "10.0.0.5",
		"--port", "3000",
		"--tls-cert", "/etc/ssl/cert.pem",
		"--tls-key", "/etc/ssl/key.pem",
	}
	if !equalSlices(got, want) {
		t.Errorf("full args: got %v, want %v", got, want)
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --------- buildSystemdUnit ---------

// TestBuildSystemdUnit_RendersRequiredSections pins the unit structure:
// the three systemd sections must all be present, the Description must
// name the cortex, and ExecStart must reproduce exactly the binary path
// + serveArgs. If any of these regress the unit file would either be
// rejected by systemd or would silently start the wrong process.
func TestBuildSystemdUnit_RendersRequiredSections(t *testing.T) {
	out := buildSystemdUnit(systemdUnitParams{
		Cortex: "agentbrain",
		User:   "mark",
		Exe:    "/home/mark/bin/noema",
		ServeArgs: []string{
			"serve", "--cortex", "agentbrain",
			"--transport", "http",
			"--host", "192.168.1.10",
			"--port", "3000",
		},
	})

	// Structural assertions.
	for _, want := range []string{
		"[Unit]",
		"[Service]",
		"[Install]",
		"Description=Noema memory server (agentbrain)",
		"User=mark",
		"ExecStart=/home/mark/bin/noema serve --cortex agentbrain --transport http --host 192.168.1.10 --port 3000",
		"Restart=on-failure",
		"WantedBy=multi-user.target",
		"After=network-online.target",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("unit missing fragment %q\nfull:\n%s", want, out)
		}
	}
}

// TestBuildSystemdUnit_CommentSuggestsCortexSpecificFilename pins the
// filename convention noema-<cortex>.service. Running multiple cortexes
// on one host is a supported configuration (a dev and a prod cortex on
// the same build host, say), so the install instructions must not
// collide on a hardcoded noema.service.
func TestBuildSystemdUnit_CommentSuggestsCortexSpecificFilename(t *testing.T) {
	out := buildSystemdUnit(systemdUnitParams{
		Cortex:    "primary",
		User:      "root",
		Exe:       "/usr/local/bin/noema",
		ServeArgs: []string{"serve", "--cortex", "primary", "--transport", "http", "--host", "127.0.0.1"},
	})
	if !strings.Contains(out, "noema-primary.service") {
		t.Errorf("install comment does not suggest noema-primary.service:\n%s", out)
	}
	if !strings.Contains(out, "systemctl enable --now noema-primary") {
		t.Errorf("install comment missing enable command for cortex-specific unit:\n%s", out)
	}
}

// TestBuildSystemdUnit_ReflectsTLSFlags pins that TLS flags pass through
// into ExecStart verbatim. Operators who set up HTTPS federation must
// not end up with a unit file that silently drops back to plain HTTP.
func TestBuildSystemdUnit_ReflectsTLSFlags(t *testing.T) {
	out := buildSystemdUnit(systemdUnitParams{
		Cortex: "secure",
		User:   "mark",
		Exe:    "/usr/bin/noema",
		ServeArgs: []string{
			"serve", "--cortex", "secure",
			"--transport", "http",
			"--host", "10.0.0.5",
			"--port", "3443",
			"--tls-cert", "/etc/noema/server.crt",
			"--tls-key", "/etc/noema/server.key",
		},
	})
	for _, want := range []string{"--tls-cert /etc/noema/server.crt", "--tls-key /etc/noema/server.key"} {
		if !strings.Contains(out, want) {
			t.Errorf("unit missing TLS fragment %q\nfull:\n%s", want, out)
		}
	}
}

// --------- buildLaunchdPlist ---------

// TestBuildLaunchdPlist_IsValidXML pins the single most important
// invariant: the emitted plist must parse as XML. launchd rejects
// malformed plists at load time, and we'd rather catch that in CI than
// on a developer's Mac.
func TestBuildLaunchdPlist_IsValidXML(t *testing.T) {
	out := buildLaunchdPlist(launchdPlistParams{
		Cortex:  "agentbrain",
		Exe:     "/usr/local/bin/noema",
		HomeDir: "/Users/mark",
		ServeArgs: []string{
			"serve", "--cortex", "agentbrain",
			"--transport", "http", "--host", "127.0.0.1",
		},
	})

	// Full pass over the document to prove it parses cleanly.
	dec := xml.NewDecoder(bytes.NewBufferString(out))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("plist is not valid XML: %v\nfull:\n%s", err, out)
		}
	}
}

// TestBuildLaunchdPlist_RendersRequiredKeys pins the plist keys launchd
// actually reads. Label/ProgramArguments/RunAtLoad/KeepAlive drive the
// supervision lifecycle; StandardOutPath/StandardErrorPath drive log
// routing. Missing any of these means a launchd agent that either
// won't start, won't restart on crash, or silently swallows its output.
func TestBuildLaunchdPlist_RendersRequiredKeys(t *testing.T) {
	out := buildLaunchdPlist(launchdPlistParams{
		Cortex:  "agentbrain",
		Exe:     "/usr/local/bin/noema",
		HomeDir: "/Users/mark",
		ServeArgs: []string{
			"serve", "--cortex", "agentbrain",
			"--transport", "http", "--host", "127.0.0.1",
			"--port", "3000",
		},
	})
	for _, want := range []string{
		"<key>Label</key>",
		"<string>com.fail-safe.noema.agentbrain</string>",
		"<key>ProgramArguments</key>",
		"<string>/usr/local/bin/noema</string>",
		"<string>serve</string>",
		"<string>--cortex</string>",
		"<string>agentbrain</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<key>StandardOutPath</key>",
		"<string>/Users/mark/Library/Logs/noema-agentbrain.log</string>",
		"<key>StandardErrorPath</key>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing fragment %q\nfull:\n%s", want, out)
		}
	}
}

// TestBuildLaunchdPlist_EscapesXMLSpecialChars pins the escaping path.
// A cortex named "a&b" or a path containing "<" would break the XML
// if passed through raw. This test uses an ampersand (the most
// common offender) and verifies it's escaped to &amp;.
func TestBuildLaunchdPlist_EscapesXMLSpecialChars(t *testing.T) {
	out := buildLaunchdPlist(launchdPlistParams{
		Cortex:  "weird&name",
		Exe:     "/bin/noema",
		HomeDir: "/home/a&b",
		ServeArgs: []string{
			"serve", "--cortex", "weird&name",
			"--transport", "http", "--host", "127.0.0.1",
		},
	})
	// The literal "&" must not appear in element content outside
	// entity references. We assert on the escaped form.
	if !strings.Contains(out, "weird&amp;name") {
		t.Errorf("cortex name not XML-escaped:\n%s", out)
	}
	if !strings.Contains(out, "/home/a&amp;b") {
		t.Errorf("home dir not XML-escaped:\n%s", out)
	}
	// And the whole thing must still parse.
	dec := xml.NewDecoder(bytes.NewBufferString(out))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("plist with special chars is not valid XML: %v", err)
		}
	}
}

// --------- runPrintSystemdUnit / runPrintLaunchdPlist validation ---------

// TestRunPrintSystemdUnit_RejectsMissingCortex pins the explicit-cortex
// guard. The unit file pins one cortex, and inheriting the name from
// env/default would either silently break at systemd load time (env
// not set in the service context) or silently track a changing
// cfg.Default across future `noema use` calls. Either is worse than
// a loud refusal at preview time.
func TestRunPrintSystemdUnit_RejectsMissingCortex(t *testing.T) {
	prev := cortexFlag
	cortexFlag = ""
	t.Cleanup(func() { cortexFlag = prev })

	var out bytes.Buffer
	err := runPrintSystemdUnit(&out, "http", "127.0.0.1", 3000, "", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "explicit --cortex flag") {
		t.Errorf("error does not mention explicit --cortex flag: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output on error, got %d bytes", out.Len())
	}
}

// TestRunPrintSystemdUnit_RejectsStdioTransport pins the http-only
// requirement. stdio has no endpoint for a supervisor to watch, so
// wrapping it in systemd is meaningless and would just mask bugs.
func TestRunPrintSystemdUnit_RejectsStdioTransport(t *testing.T) {
	prev := cortexFlag
	cortexFlag = "agentbrain"
	t.Cleanup(func() { cortexFlag = prev })

	var out bytes.Buffer
	err := runPrintSystemdUnit(&out, "stdio", "", 0, "", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "requires --transport http") {
		t.Errorf("error does not mention transport http requirement: %v", err)
	}
}

// TestRunPrintSystemdUnit_PropagatesHTTPValidationErrors pins the
// validateHTTPServe reuse. The print path must reject the same
// configurations the real serve path rejects — otherwise operators
// would install a unit file that fails only on first start.
func TestRunPrintSystemdUnit_PropagatesHTTPValidationErrors(t *testing.T) {
	prev := cortexFlag
	cortexFlag = "agentbrain"
	t.Cleanup(func() { cortexFlag = prev })

	var out bytes.Buffer
	err := runPrintSystemdUnit(&out, "http", "0.0.0.0", 3000, "", "")
	if err == nil {
		t.Fatal("expected error on 0.0.0.0 bind, got nil")
	}
	if !strings.Contains(err.Error(), "is not allowed") {
		t.Errorf("error does not reflect the 0.0.0.0 guard: %v", err)
	}
}

// TestRunPrintSystemdUnit_HappyPath pins the end-to-end success shape:
// given valid flags and an explicit --cortex, the function writes a
// unit file to the provided writer and returns nil. We don't pin the
// whole output (the builders have their own tests) — just that the
// ExecStart names the bound cortex, proving the whole chain hangs
// together.
func TestRunPrintSystemdUnit_HappyPath(t *testing.T) {
	prev := cortexFlag
	cortexFlag = "agentbrain"
	t.Cleanup(func() { cortexFlag = prev })

	var out bytes.Buffer
	if err := runPrintSystemdUnit(&out, "http", "127.0.0.1", 3000, "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "--cortex agentbrain") {
		t.Errorf("output does not include --cortex agentbrain:\n%s", s)
	}
	if !strings.Contains(s, "[Unit]") {
		t.Errorf("output is not a systemd unit:\n%s", s)
	}
}

// TestRunPrintLaunchdPlist_RejectsMissingCortex mirrors the systemd
// guard test for the launchd code path.
func TestRunPrintLaunchdPlist_RejectsMissingCortex(t *testing.T) {
	prev := cortexFlag
	cortexFlag = ""
	t.Cleanup(func() { cortexFlag = prev })

	var out bytes.Buffer
	err := runPrintLaunchdPlist(&out, "http", "127.0.0.1", 3000, "", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "explicit --cortex flag") {
		t.Errorf("error does not mention explicit --cortex flag: %v", err)
	}
}

// TestRunPrintLaunchdPlist_RejectsStdioTransport mirrors the systemd
// stdio rejection for launchd.
func TestRunPrintLaunchdPlist_RejectsStdioTransport(t *testing.T) {
	prev := cortexFlag
	cortexFlag = "agentbrain"
	t.Cleanup(func() { cortexFlag = prev })

	var out bytes.Buffer
	err := runPrintLaunchdPlist(&out, "stdio", "", 0, "", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "requires --transport http") {
		t.Errorf("error does not mention transport http requirement: %v", err)
	}
}

// TestRunPrintLaunchdPlist_HappyPath mirrors the systemd happy path:
// valid flags + explicit cortex → plist with the bound cortex name in
// ProgramArguments.
func TestRunPrintLaunchdPlist_HappyPath(t *testing.T) {
	prev := cortexFlag
	cortexFlag = "agentbrain"
	t.Cleanup(func() { cortexFlag = prev })

	var out bytes.Buffer
	if err := runPrintLaunchdPlist(&out, "http", "127.0.0.1", 3000, "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "<string>agentbrain</string>") {
		t.Errorf("plist does not pin cortex name:\n%s", s)
	}
	if !strings.Contains(s, "com.fail-safe.noema.agentbrain") {
		t.Errorf("plist label does not include cortex name:\n%s", s)
	}
}

// TestRequireTLSForKeyedMode pins the auth-requires-TLS guard: shared-key
// mode over plaintext HTTP is a worse security posture than open mode (it
// leaks the key on every request while creating the appearance of security),
// so the combination is a hard startup error. The guard must fire for both
// "file" and "env" sources, must be a no-op in open mode, and must be a
// no-op when TLS is configured.
func TestRequireTLSForKeyedMode(t *testing.T) {
	cases := []struct {
		name          string
		access        cortex.AccessKey
		useTLS        bool
		wantErrSubstr string // empty = expect nil
	}{
		{
			name:   "open mode no TLS — fine",
			access: cortex.AccessKey{}, // Keyed() == false
			useTLS: false,
		},
		{
			name:   "open mode with TLS — fine",
			access: cortex.AccessKey{},
			useTLS: true,
		},
		{
			name:   "keyed mode with TLS — fine",
			access: cortex.AccessKey{Value: "k", Source: "file", Fingerprint: "SHA256:aa"},
			useTLS: true,
		},
		{
			name:          "keyed mode from file, no TLS — reject",
			access:        cortex.AccessKey{Value: "k", Source: "file", Path: ".access.secret"},
			useTLS:        false,
			wantErrSubstr: "source=file",
		},
		{
			name:          "keyed mode from env, no TLS — reject",
			access:        cortex.AccessKey{Value: "k", Source: "env"},
			useTLS:        false,
			wantErrSubstr: "source=env",
		},
		{
			name:          "keyed mode error mentions plaintext",
			access:        cortex.AccessKey{Value: "k", Source: "file"},
			useTLS:        false,
			wantErrSubstr: "plaintext HTTP",
		},
		{
			name:          "keyed mode error suggests remediation",
			access:        cortex.AccessKey{Value: "k", Source: "file"},
			useTLS:        false,
			wantErrSubstr: "--tls-cert and --tls-key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireTLSForKeyedMode(tc.access, tc.useTLS)
			if tc.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("expected nil, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

// TestValidateHTTPServe_ImplicitCortexErrorMessage pins the specific shape of
// the explicit-cortex error message. The error has to (a) name the bound
// cortex so the operator can sanity-check it, (b) suggest the exact
// command they should re-run, and (c) explain the *why* (network
// exposure / silent failure mode) — not just say "no". A bare "missing
// --cortex" error would be technically correct but would put the operator
// right back in the same diagnostic loop the guard exists to short-circuit.
func TestValidateHTTPServe_ImplicitCortexErrorMessage(t *testing.T) {
	err := validateHTTPServe("ai-1.example.com", "", "", "ai-1", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		`"ai-1"`,                             // names the bound cortex
		"--cortex ai-1",                      // suggests the exact fix
		"--transport http",                   // includes the transport flag
		"--host ai-1.example.com",            // includes the host
		"silent failures on the peer side",   // explains *why*
		"NOEMA_CORTEX or the config default", // names the implicit paths
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing required fragment %q\nfull message:\n%s", want, msg)
		}
	}
}
