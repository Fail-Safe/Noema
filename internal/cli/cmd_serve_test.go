package cli

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

// TestValidateHTTPServe pins the flag invariants for `noema serve --transport
// http`. The most important case is the explicit-cortex requirement: silently
// inheriting the cortex from cfg.Default on a network transport is the failure
// mode that motivated this guard, and the test must keep firing if anyone
// weakens the check (e.g. re-adding a "but only if peers are configured"
// condition).
func TestValidateHTTPServe(t *testing.T) {
	const cortexName = "peer-a"

	cases := []struct {
		name           string
		hosts          []string
		tlsCert        string
		tlsKey         string
		cortexExplicit bool
		wantErrSubstr  string // empty = expect nil
	}{
		{
			name:           "happy path no TLS",
			hosts:          []string{"127.0.0.1"},
			cortexExplicit: true,
		},
		{
			name:           "happy path with TLS",
			hosts:          []string{"peer-a.example.com"},
			tlsCert:        "/etc/ssl/cert.pem",
			tlsKey:         "/etc/ssl/key.pem",
			cortexExplicit: true,
		},
		{
			name:           "missing host",
			hosts:          nil,
			cortexExplicit: true,
			wantErrSubstr:  "--host is required for HTTP transport",
		},
		{
			name:           "0.0.0.0 unspecified IPv4",
			hosts:          []string{"0.0.0.0"},
			cortexExplicit: true,
			wantErrSubstr:  "binding to 0.0.0.0 is not allowed",
		},
		{
			name:           "IPv6 unspecified",
			hosts:          []string{"::"},
			cortexExplicit: true,
			wantErrSubstr:  "is not allowed",
		},
		{
			name:           "TLS cert without key",
			hosts:          []string{"127.0.0.1"},
			tlsCert:        "/etc/ssl/cert.pem",
			cortexExplicit: true,
			wantErrSubstr:  "--tls-cert and --tls-key must be provided together",
		},
		{
			name:           "TLS key without cert",
			hosts:          []string{"127.0.0.1"},
			tlsKey:         "/etc/ssl/key.pem",
			cortexExplicit: true,
			wantErrSubstr:  "--tls-cert and --tls-key must be provided together",
		},
		{
			// The regression case: a cortex is bound (here "peer-a") but
			// --cortex was not on the command line. Even though host and
			// TLS look fine, the implicit binding is a federation
			// footgun and must be rejected.
			name:           "implicit cortex on HTTP",
			hosts:          []string{"peer-a.example.com"},
			tlsCert:        "/etc/ssl/cert.pem",
			tlsKey:         "/etc/ssl/key.pem",
			cortexExplicit: false,
			wantErrSubstr:  "without an explicit --cortex flag",
		},
		{
			// Specifically the mycortex-on-peer-a case from the field:
			// the bound cortex has no peers of its own, but is being
			// served on a host where another cortex's peers expect to
			// find a different identity. The guard must fire regardless
			// of the bound cortex's federation config.
			name:           "implicit cortex on HTTP includes cortex name in error",
			hosts:          []string{"peer-a-tb.example.com"},
			cortexExplicit: false,
			wantErrSubstr:  "\"peer-a\"",
		},
		{
			// Multi-host: all hosts must be valid. The second host is
			// a wildcard that should be rejected even though the first
			// host is fine.
			name:           "multi-host rejects wildcard in any position",
			hosts:          []string{"10.0.0.1", "0.0.0.0"},
			cortexExplicit: true,
			wantErrSubstr:  "binding to 0.0.0.0 is not allowed",
		},
		{
			name:           "multi-host happy path",
			hosts:          []string{"10.0.0.1", "192.168.45.3"},
			cortexExplicit: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHTTPServe(tc.hosts, tc.tlsCert, tc.tlsKey, cortexName, tc.cortexExplicit)
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

func TestValidateHTTPServeWithDynamic(t *testing.T) {
	if err := validateHTTPServeWithDynamic(
		[]string{"127.0.0.1"}, []string{"192.168.1.42"}, "", "", "local", true,
	); err != nil {
		t.Fatalf("valid dynamic IP rejected: %v", err)
	}

	if err := validateHTTPServeWithDynamic(
		[]string{"127.0.0.1"}, []string{"home.example.com"}, "", "", "local", true,
	); err != nil {
		t.Fatalf("valid dynamic hostname rejected: %v", err)
	}

	for _, host := range []string{"0.0.0.0", "::"} {
		err := validateHTTPServeWithDynamic(
			[]string{"127.0.0.1"}, []string{host}, "", "", "local", true,
		)
		if err == nil {
			t.Errorf("dynamic host %q: expected validation error", host)
		}
	}
}

type dynamicResolution struct {
	address   string
	available bool
}

type blockingTestListener struct {
	addr       net.Addr
	accepting  chan struct{}
	closed     chan struct{}
	acceptOnce sync.Once
	closeOnce  sync.Once
}

type testNetAddr string

func (a testNetAddr) Network() string { return "tcp" }
func (a testNetAddr) String() string  { return string(a) }

func newBlockingTestListener(address string) *blockingTestListener {
	return &blockingTestListener{
		addr:      testNetAddr(address),
		accepting: make(chan struct{}),
		closed:    make(chan struct{}),
	}
}

func (l *blockingTestListener) Accept() (net.Conn, error) {
	l.acceptOnce.Do(func() { close(l.accepting) })
	<-l.closed
	return nil, net.ErrClosed
}

func (l *blockingTestListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *blockingTestListener) Addr() net.Addr { return l.addr }

func waitForTestSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitForTestListener(t *testing.T, listeners <-chan *blockingTestListener, description string) *blockingTestListener {
	t.Helper()
	select {
	case listener := <-listeners:
		return listener
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
		return nil
	}
}

func TestReconcileDynamicListenersLifecycle(t *testing.T) {
	resolutions := make(chan dynamicResolution, 5)
	resolved := make(chan struct{}, 5)
	retries := make(chan time.Time, 1)
	listeners := make(chan *blockingTestListener, 3)
	done := make(chan struct{})
	fatal := make(chan error, 1)
	returned := make(chan struct{})

	runtime := dynamicListenerRuntime{
		resolve: func(string) (string, bool) {
			resolution := <-resolutions
			resolved <- struct{}{}
			return resolution.address, resolution.available
		},
		listen: func(_, address string) (net.Listener, error) {
			listener := newBlockingTestListener(address)
			listeners <- listener
			return listener, nil
		},
		retry: retries,
	}

	resolutions <- dynamicResolution{}
	go func() {
		defer close(returned)
		reconcileDynamicListenersWithRuntime(
			done, []string{"roaming.example.com"}, 3000, http.NotFoundHandler(), "", "", "http", fatal, runtime,
		)
	}()
	waitForTestSignal(t, resolved, "initial absent resolution")

	resolutions <- dynamicResolution{address: "127.0.0.1", available: true}
	retries <- time.Now()
	waitForTestSignal(t, resolved, "first available resolution")
	first := waitForTestListener(t, listeners, "first listener")
	waitForTestSignal(t, first.accepting, "first listener to serve")

	resolutions <- dynamicResolution{address: "127.0.0.2", available: true}
	retries <- time.Now()
	waitForTestSignal(t, resolved, "changed resolution")
	second := waitForTestListener(t, listeners, "replacement listener")
	waitForTestSignal(t, first.closed, "first listener cleanup after address change")
	waitForTestSignal(t, second.accepting, "replacement listener to serve")

	resolutions <- dynamicResolution{}
	retries <- time.Now()
	waitForTestSignal(t, resolved, "disappeared resolution")
	waitForTestSignal(t, second.closed, "replacement listener cleanup after disappearance")

	resolutions <- dynamicResolution{address: "127.0.0.1", available: true}
	retries <- time.Now()
	waitForTestSignal(t, resolved, "reappeared resolution")
	third := waitForTestListener(t, listeners, "recreated listener")
	waitForTestSignal(t, third.accepting, "recreated listener to serve")

	close(done)
	waitForTestSignal(t, third.closed, "listener cleanup on shutdown")
	waitForTestSignal(t, returned, "reconciler shutdown")
	select {
	case err := <-fatal:
		t.Fatalf("unexpected fatal error: %v", err)
	default:
	}
}

func TestReconcileDynamicListenersRetriesAddressNotAvailable(t *testing.T) {
	retries := make(chan time.Time, 1)
	listenResults := make(chan struct {
		listener net.Listener
		err      error
	}, 2)
	listenCalled := make(chan struct{}, 2)
	done := make(chan struct{})
	fatal := make(chan error, 1)
	returned := make(chan struct{})
	listener := newBlockingTestListener("127.0.0.1:3000")

	listenResults <- struct {
		listener net.Listener
		err      error
	}{err: syscall.EADDRNOTAVAIL}
	listenResults <- struct {
		listener net.Listener
		err      error
	}{listener: listener}

	runtime := dynamicListenerRuntime{
		resolve: func(string) (string, bool) { return "127.0.0.1", true },
		listen: func(_, _ string) (net.Listener, error) {
			result := <-listenResults
			listenCalled <- struct{}{}
			return result.listener, result.err
		},
		retry: retries,
	}

	go func() {
		defer close(returned)
		reconcileDynamicListenersWithRuntime(
			done, []string{"127.0.0.1"}, 3000, http.NotFoundHandler(), "", "", "http", fatal, runtime,
		)
	}()
	waitForTestSignal(t, listenCalled, "initial unavailable bind")

	retries <- time.Now()
	waitForTestSignal(t, listenCalled, "retried bind")
	waitForTestSignal(t, listener.accepting, "listener after retry")
	close(done)
	waitForTestSignal(t, listener.closed, "retried listener cleanup")
	waitForTestSignal(t, returned, "reconciler shutdown")
	select {
	case err := <-fatal:
		t.Fatalf("EADDRNOTAVAIL should not be fatal: %v", err)
	default:
	}
}

func TestReconcileDynamicListenersReportsFatalBindError(t *testing.T) {
	fatal := make(chan error, 1)
	reconcilerReturned := make(chan struct{})
	runtime := dynamicListenerRuntime{
		resolve: func(string) (string, bool) { return "127.0.0.1", true },
		listen:  func(_, _ string) (net.Listener, error) { return nil, syscall.EADDRINUSE },
		retry:   make(chan time.Time),
	}

	go func() {
		defer close(reconcilerReturned)
		reconcileDynamicListenersWithRuntime(
			make(chan struct{}), []string{"127.0.0.1"}, 3000, http.NotFoundHandler(), "", "", "http", fatal, runtime,
		)
	}()

	select {
	case err := <-fatal:
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("fatal error = %v, want EADDRINUSE", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fatal bind error")
	}
	waitForTestSignal(t, reconcilerReturned, "reconciler return after fatal bind")
}

// --------- guardStdioFlags ---------

// TestGuardStdioFlags pins the footgun short-circuit: an operator who
// forgets --transport http and passes HTTP-only flags should hit a loud
// error at startup rather than a silent stdio process that swallows
// stdin forever. The guard keys off cobra's Flags().Changed() — i.e.
// *explicit* flag presence — so default-valued --port=3000 is not a
// conflict on its own.
func TestGuardStdioFlags(t *testing.T) {
	cases := []struct {
		name                                    string
		hostSet, portSet, tlsCertSet, tlsKeySet bool
		wantErrSubstrs                          []string // empty = expect nil
	}{
		{
			name: "nothing set — fine",
		},
		{
			name:           "only --host set",
			hostSet:        true,
			wantErrSubstrs: []string{"--host", "is only meaningful", "--transport http"},
		},
		{
			name:           "only --port set",
			portSet:        true,
			wantErrSubstrs: []string{"--port", "is only meaningful", "--transport http"},
		},
		{
			name:           "only --tls-cert set",
			tlsCertSet:     true,
			wantErrSubstrs: []string{"--tls-cert", "is only meaningful", "--transport http"},
		},
		{
			name:           "only --tls-key set",
			tlsKeySet:      true,
			wantErrSubstrs: []string{"--tls-key", "is only meaningful", "--transport http"},
		},
		{
			// Plural flip: when two or more flags conflict, the error
			// says "are only meaningful" not "is only meaningful".
			// Trivial, but a user-facing grammar bug reads as sloppy.
			name:           "multiple flags — plural verb",
			hostSet:        true,
			portSet:        true,
			wantErrSubstrs: []string{"--host, --port", "are only meaningful"},
		},
		{
			// All four flags. The error must list them all so the
			// operator can see the full conflict in one shot.
			name:           "all four flags",
			hostSet:        true,
			portSet:        true,
			tlsCertSet:     true,
			tlsKeySet:      true,
			wantErrSubstrs: []string{"--host", "--port", "--tls-cert", "--tls-key", "are only meaningful"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := guardStdioFlags(tc.hostSet, tc.portSet, tc.tlsCertSet, tc.tlsKeySet)
			if len(tc.wantErrSubstrs) == 0 {
				if err != nil {
					t.Fatalf("expected nil, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			for _, want := range tc.wantErrSubstrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err.Error(), want)
				}
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
	got := buildServeArgs("mycortex", "http", nil, 0, "", "")
	want := []string{"serve", "--cortex", "mycortex", "--transport", "http"}
	if !equalSlices(got, want) {
		t.Errorf("minimal args: got %v, want %v", got, want)
	}

	// Full: every optional flag set, single host.
	got = buildServeArgs("mycortex", "http", []string{"10.0.0.5"}, 3000, "/etc/ssl/cert.pem", "/etc/ssl/key.pem")
	want = []string{
		"serve", "--cortex", "mycortex", "--transport", "http",
		"--host", "10.0.0.5",
		"--port", "3000",
		"--tls-cert", "/etc/ssl/cert.pem",
		"--tls-key", "/etc/ssl/key.pem",
	}
	if !equalSlices(got, want) {
		t.Errorf("full args (single host): got %v, want %v", got, want)
	}

	// Multi-host: each host gets its own --host flag.
	got = buildServeArgs("mycortex", "http", []string{"10.0.0.5", "192.168.45.3"}, 3000, "", "")
	want = []string{
		"serve", "--cortex", "mycortex", "--transport", "http",
		"--host", "10.0.0.5",
		"--host", "192.168.45.3",
		"--port", "3000",
	}
	if !equalSlices(got, want) {
		t.Errorf("multi-host args: got %v, want %v", got, want)
	}
}

func TestBuildServeArgsWithDynamic(t *testing.T) {
	got := buildServeArgsWithDynamic(
		"mycortex", "http", []string{"127.0.0.1"}, []string{"192.168.1.42"}, 3000, "", "",
	)
	want := []string{
		"serve", "--cortex", "mycortex", "--transport", "http",
		"--host", "127.0.0.1",
		"--host-dynamic", "192.168.1.42",
		"--port", "3000",
	}
	if !equalSlices(got, want) {
		t.Errorf("dynamic args: got %v, want %v", got, want)
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
		Cortex: "mycortex",
		User:   "mark",
		Exe:    "/home/user/bin/noema",
		ServeArgs: []string{
			"serve", "--cortex", "mycortex",
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
		"Description=Noema memory server (mycortex)",
		"User=mark",
		"ExecStart=/home/user/bin/noema serve --cortex mycortex --transport http --host 192.168.1.10 --port 3000",
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
		Cortex:  "mycortex",
		Exe:     "/usr/local/bin/noema",
		HomeDir: "/Users/user",
		ServeArgs: []string{
			"serve", "--cortex", "mycortex",
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
		Cortex:  "mycortex",
		Exe:     "/usr/local/bin/noema",
		HomeDir: "/Users/user",
		ServeArgs: []string{
			"serve", "--cortex", "mycortex",
			"--transport", "http", "--host", "127.0.0.1",
			"--port", "3000",
		},
	})
	for _, want := range []string{
		"<key>Label</key>",
		"<string>com.fail-safe.noema.mycortex</string>",
		"<key>ProgramArguments</key>",
		"<string>/usr/local/bin/noema</string>",
		"<string>serve</string>",
		"<string>--cortex</string>",
		"<string>mycortex</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<key>StandardOutPath</key>",
		"<string>/Users/user/Library/Logs/noema-mycortex.log</string>",
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
	err := runPrintSystemdUnit(&out, "http", []string{"127.0.0.1"}, 3000, "", "")
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
	cortexFlag = "mycortex"
	t.Cleanup(func() { cortexFlag = prev })

	var out bytes.Buffer
	err := runPrintSystemdUnit(&out, "stdio", nil, 0, "", "")
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
	cortexFlag = "mycortex"
	t.Cleanup(func() { cortexFlag = prev })

	var out bytes.Buffer
	err := runPrintSystemdUnit(&out, "http", []string{"0.0.0.0"}, 3000, "", "")
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
	cortexFlag = "mycortex"
	t.Cleanup(func() { cortexFlag = prev })

	var out bytes.Buffer
	if err := runPrintSystemdUnit(&out, "http", []string{"127.0.0.1"}, 3000, "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "--cortex mycortex") {
		t.Errorf("output does not include --cortex mycortex:\n%s", s)
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
	err := runPrintLaunchdPlist(&out, "http", []string{"127.0.0.1"}, 3000, "", "")
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
	cortexFlag = "mycortex"
	t.Cleanup(func() { cortexFlag = prev })

	var out bytes.Buffer
	err := runPrintLaunchdPlist(&out, "stdio", nil, 0, "", "")
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
	cortexFlag = "mycortex"
	t.Cleanup(func() { cortexFlag = prev })

	var out bytes.Buffer
	if err := runPrintLaunchdPlist(&out, "http", []string{"127.0.0.1"}, 3000, "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "<string>mycortex</string>") {
		t.Errorf("plist does not pin cortex name:\n%s", s)
	}
	if !strings.Contains(s, "com.fail-safe.noema.mycortex") {
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

// --------- buildSystemdUnit auth plumbing ---------

// TestBuildSystemdUnit_EmitsOptionalEnvironmentFile pins the shared-key
// plumbing in the generated unit. The line must be present, use a
// leading "-" so open-mode cortexes are not broken by a missing file,
// and live inside the [Service] section (not [Unit]) — a misplaced
// EnvironmentFile= is silently ignored by systemd and produces a unit
// that looks fine but never injects NOEMA_MCP_KEY.
func TestBuildSystemdUnit_EmitsOptionalEnvironmentFile(t *testing.T) {
	out := buildSystemdUnit(systemdUnitParams{
		Cortex:    "mycortex",
		User:      "mark",
		Exe:       "/usr/local/bin/noema",
		ServeArgs: []string{"serve", "--cortex", "mycortex", "--transport", "http", "--host", "127.0.0.1"},
	})

	// The "-" prefix is load-bearing: without it, systemd refuses to
	// start the unit when the env file does not exist, which is the
	// default state for open-mode cortexes. The test pins the exact
	// path form so a refactor that changes ~/.config to /etc (or
	// drops the "-") is caught here.
	const wantLine = "EnvironmentFile=-%h/.config/noema/mycortex.env"
	if !strings.Contains(out, wantLine) {
		t.Errorf("unit missing optional EnvironmentFile line %q\nfull:\n%s", wantLine, out)
	}

	// Structurally verify the directive lives in [Service]. We look
	// at the substring between [Service] and [Install] to make sure
	// EnvironmentFile= isn't sitting in [Unit] where systemd would
	// silently ignore it.
	svcIdx := strings.Index(out, "[Service]")
	instIdx := strings.Index(out, "[Install]")
	if svcIdx < 0 || instIdx < 0 || svcIdx >= instIdx {
		t.Fatalf("unit is missing [Service] or [Install] section:\n%s", out)
	}
	serviceBlock := out[svcIdx:instIdx]
	if !strings.Contains(serviceBlock, "EnvironmentFile=-") {
		t.Errorf("EnvironmentFile is not inside [Service] section:\n%s", out)
	}
}

// TestBuildSystemdUnit_IncludesKeyedModeInstallInstructions pins that
// the header comment explains how to populate the env file for keyed
// mode. If the instructions regress, operators will install the unit
// expecting keyed mode to "just work" and hit a 401 on first peer
// sync instead of getting the env file checklist up front.
func TestBuildSystemdUnit_IncludesKeyedModeInstallInstructions(t *testing.T) {
	out := buildSystemdUnit(systemdUnitParams{
		Cortex:    "mycortex",
		User:      "mark",
		Exe:       "/usr/local/bin/noema",
		ServeArgs: []string{"serve", "--cortex", "mycortex", "--transport", "http", "--host", "127.0.0.1"},
	})
	for _, want := range []string{
		"keyed-mode MCP auth",
		"NOEMA_MCP_KEY=<paste-key>",
		"chmod 600",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("install comment missing %q:\n%s", want, out)
		}
	}
}

// --------- buildLaunchdPlist auth plumbing ---------

// TestBuildLaunchdPlist_EmitsEnvironmentVariablesBlock pins the shape
// of the EnvironmentVariables dict: one key named NOEMA_MCP_KEY, with
// a placeholder value operators must replace before loading the plist.
// launchd has no EnvironmentFile= equivalent, so the key lives in the
// plist itself — this test exists so a refactor can't silently drop
// the dict (which would downgrade every keyed-mode Mac to open mode).
func TestBuildLaunchdPlist_EmitsEnvironmentVariablesBlock(t *testing.T) {
	out := buildLaunchdPlist(launchdPlistParams{
		Cortex:    "mycortex",
		Exe:       "/usr/local/bin/noema",
		HomeDir:   "/Users/user",
		ServeArgs: []string{"serve", "--cortex", "mycortex", "--transport", "http", "--host", "127.0.0.1"},
	})
	for _, want := range []string{
		"<key>EnvironmentVariables</key>",
		"<key>NOEMA_MCP_KEY</key>",
		"<string>" + launchdKeyPlaceholder + "</string>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing fragment %q\nfull:\n%s", want, out)
		}
	}

	// The placeholder must NOT look like a real key. If someone ever
	// changes it to a short, plausible-looking string an operator
	// could skip the install warning and load the plist with a
	// literal "s3cret" as NOEMA_MCP_KEY. The "REPLACE" marker is the
	// anti-footgun: it's ugly on purpose so `launchd load` surfaces
	// the 401 immediately.
	if !strings.Contains(launchdKeyPlaceholder, "REPLACE") {
		t.Errorf("placeholder %q does not obviously require replacement", launchdKeyPlaceholder)
	}

	// The emitted plist must still be valid XML — adding a nested
	// dict is easy to get wrong (unclosed <dict>), and if it does we
	// want to catch it in CI, not when an operator runs launchctl
	// bootstrap. Re-run the full parse to be sure.
	dec := xml.NewDecoder(bytes.NewBufferString(out))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("plist with EnvironmentVariables is not valid XML: %v\nfull:\n%s", err, out)
		}
	}
}

// --------- runPrintMCPConfig ---------

// TestRunPrintMCPConfig_StdioShape pins the stdio output: a single
// mcpServers.noema entry with command/args, no url/headers. This is
// the shape Claude Code and every other stdio-based MCP client
// expects; regressing to the http form here would silently break
// local-only workflows.
func TestRunPrintMCPConfig_StdioShape(t *testing.T) {
	prev := cortexFlag
	cortexFlag = "mycortex"
	t.Cleanup(func() { cortexFlag = prev })

	var buf bytes.Buffer
	if err := runPrintMCPConfig(&buf, "stdio", nil, 0, "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed struct {
		McpServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			URL     string   `json:"url"`
			Headers map[string]string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput:\n%s", err, buf.String())
	}
	entry, ok := parsed.McpServers["noema"]
	if !ok {
		t.Fatalf("output missing mcpServers.noema:\n%s", buf.String())
	}
	if entry.Command == "" {
		t.Errorf("stdio entry missing command:\n%s", buf.String())
	}
	if entry.URL != "" {
		t.Errorf("stdio entry must not include url:\n%s", buf.String())
	}
	if entry.Headers != nil {
		t.Errorf("stdio entry must not include headers:\n%s", buf.String())
	}
	// The --cortex flag must be part of args so the generated config
	// pins exactly the cortex the operator was viewing at print time.
	found := false
	for _, a := range entry.Args {
		if a == "mycortex" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("stdio entry args do not pin --cortex mycortex: %v", entry.Args)
	}
}

// TestRunPrintMCPConfig_HTTPShape pins the http output: url + headers
// with the Authorization placeholder literal. This is the whole point
// of the PR (c) work — a Claude Code client pointing at a keyed
// remote peer needs the bearer header in the config, and the
// placeholder form lets operators commit the file to source control
// without leaking the key.
func TestRunPrintMCPConfig_HTTPShape(t *testing.T) {
	var buf bytes.Buffer
	if err := runPrintMCPConfig(&buf, "http", []string{"10.0.0.1"}, 3443, "/tls.crt", "/tls.key"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed struct {
		McpServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Command string            `json:"command"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput:\n%s", err, buf.String())
	}
	entry := parsed.McpServers["noema"]

	if entry.Command != "" {
		t.Errorf("http entry must not include command:\n%s", buf.String())
	}
	// TLS flags present → https scheme; port included; /mcp path.
	wantURL := "https://10.0.0.1:3443/mcp"
	if entry.URL != wantURL {
		t.Errorf("url = %q, want %q", entry.URL, wantURL)
	}
	// The Authorization header must be the literal placeholder. We
	// DO NOT expand env vars here — the whole point is that the
	// client (Claude Code) performs the expansion at runtime, and
	// the file is safe to commit.
	wantAuth := "Bearer ${NOEMA_MCP_KEY}"
	if got := entry.Headers["Authorization"]; got != wantAuth {
		t.Errorf("Authorization header = %q, want %q", got, wantAuth)
	}
}

func TestRunPrintMCPConfig_HTTPPrefersDynamicHost(t *testing.T) {
	var buf bytes.Buffer
	if err := runPrintMCPConfigWithDynamic(
		&buf,
		"http",
		[]string{"127.0.0.1"},
		[]string{"roaming.example.com", "192.168.1.42"},
		3443,
		"/tls.crt",
		"/tls.key",
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "https://roaming.example.com:3443/mcp") {
		t.Errorf("dynamic host should be the generated client URL:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "https://127.0.0.1:3443/mcp") {
		t.Errorf("required loopback host must not replace the dynamic client URL:\n%s", buf.String())
	}
}

func TestRunPrintMCPConfig_HTTPRejectsWildcardDynamicHost(t *testing.T) {
	var buf bytes.Buffer
	err := runPrintMCPConfigWithDynamic(
		&buf,
		"http",
		[]string{"127.0.0.1"},
		[]string{"0.0.0.0"},
		3000,
		"",
		"",
	)
	if err == nil {
		t.Fatal("expected error for wildcard dynamic host, got nil")
	}
	if !strings.Contains(err.Error(), "--host-dynamic 0.0.0.0") {
		t.Errorf("error does not identify the invalid dynamic host: %v", err)
	}
}

// TestRunPrintMCPConfig_HTTPNoTLS pins that http (without --tls-cert)
// produces an http:// URL, not https://. Getting this wrong would
// point the generated client config at the wrong scheme and every
// request would fail on TLS handshake before the 401 even runs.
func TestRunPrintMCPConfig_HTTPNoTLS(t *testing.T) {
	var buf bytes.Buffer
	if err := runPrintMCPConfig(&buf, "http", []string{"127.0.0.1"}, 3000, "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "http://127.0.0.1:3000/mcp") {
		t.Errorf("no-TLS http mode should emit http:// URL:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "https://") {
		t.Errorf("no-TLS http mode must not emit https:// URL:\n%s", buf.String())
	}
}

// TestRunPrintMCPConfig_HTTPRejectsMissingHost pins the guard that
// refuses to print an http config without --host. A missing host
// would silently produce a ":3000/mcp" URL that's meaningless.
func TestRunPrintMCPConfig_HTTPRejectsMissingHost(t *testing.T) {
	var buf bytes.Buffer
	err := runPrintMCPConfig(&buf, "http", nil, 3000, "", "")
	if err == nil {
		t.Fatal("expected error for http without --host, got nil")
	}
	if !strings.Contains(err.Error(), "--host") {
		t.Errorf("error does not mention --host: %v", err)
	}
}

// TestRunPrintMCPConfig_HTTPRejectsWildcardHost pins that 0.0.0.0
// is rejected here for the same reason serve rejects it on the
// listen side: a wildcard is not a dialable address for a client.
// This guard exists so an operator who copy-pastes their serve
// args into --print-config doesn't get a useless config file.
func TestRunPrintMCPConfig_HTTPRejectsWildcardHost(t *testing.T) {
	var buf bytes.Buffer
	err := runPrintMCPConfig(&buf, "http", []string{"0.0.0.0"}, 3000, "", "")
	if err == nil {
		t.Fatal("expected error for 0.0.0.0 host, got nil")
	}
}

// TestRunPrintMCPConfig_HTTPIPv6Brackets pins the URL formatting for
// IPv6 literals. An IPv6 host without brackets produces a URL that
// url.Parse rejects, so MCP clients would fail at config load.
func TestRunPrintMCPConfig_HTTPIPv6Brackets(t *testing.T) {
	var buf bytes.Buffer
	if err := runPrintMCPConfig(&buf, "http", []string{"fe80::1"}, 3000, "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "http://[fe80::1]:3000/mcp") {
		t.Errorf("IPv6 host not bracketed:\n%s", buf.String())
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
	err := validateHTTPServe([]string{"peer-a.example.com"}, "", "", "peer-a", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		`"peer-a"`,                           // names the bound cortex
		"--cortex peer-a",                    // suggests the exact fix
		"--transport http",                   // includes the transport flag
		"--host peer-a.example.com",          // includes the host
		"silent failures on the peer side",   // explains *why*
		"NOEMA_CORTEX or the config default", // names the implicit paths
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing required fragment %q\nfull message:\n%s", want, msg)
		}
	}
}

// TestInICloudDrive pins the path-substring contract used by the
// startup warning. The check is a string match on the canonical
// macOS iCloud Drive sync root — adding flexibility (case-insensitive,
// regex, etc.) is a footgun because legitimate paths like
// "/library/mobile/documents" don't match the real container.
func TestInICloudDrive(t *testing.T) {
	cases := []struct {
		name string
		dir  string
		want bool
	}{
		{
			name: "default iCloud Drive container",
			dir:  "/Users/alice/Library/Mobile Documents/com~apple~CloudDocs/cortex",
			want: true,
		},
		{
			name: "Obsidian iCloud-managed vault",
			dir:  "/Users/alice/Library/Mobile Documents/iCloud~md~obsidian/Documents/cortex",
			want: true,
		},
		{
			name: "regular home directory",
			dir:  "/Users/alice/cortex",
			want: false,
		},
		{
			name: "lowercase library is not iCloud",
			dir:  "/Users/alice/library/mobile documents/cortex",
			want: false,
		},
		{
			name: "Linux XDG path",
			dir:  "/home/alice/.local/share/noema/cortex",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inICloudDrive(tc.dir); got != tc.want {
				t.Errorf("inICloudDrive(%q) = %v, want %v", tc.dir, got, tc.want)
			}
		})
	}
}
