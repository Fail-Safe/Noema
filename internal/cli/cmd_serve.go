package cli

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/config"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/federation"
	mcpserver "github.com/Fail-Safe/Noema/internal/mcp"
	mcpgo "github.com/mark3labs/mcp-go/server"
)

func serveCmd() *cobra.Command {
	var (
		transport         string
		host              string
		port              int
		tlsCert           string
		tlsKey            string
		printConfig       bool
		printSystemdUnit  bool
		printLaunchdPlist bool
	)

	cmd := &cobra.Command{
		Use:     "serve",
		Short:   "Start the MCP server",
		Aliases: []string{"server"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if printConfig {
				return runPrintMCPConfig(cmd.OutOrStdout(), transport, host, port, tlsCert, tlsKey)
			}
			if printSystemdUnit {
				return runPrintSystemdUnit(cmd.OutOrStdout(), transport, host, port, tlsCert, tlsKey)
			}
			if printLaunchdPlist {
				return runPrintLaunchdPlist(cmd.OutOrStdout(), transport, host, port, tlsCert, tlsKey)
			}

			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			// Don't defer cx.Close() — server runs until interrupted.

			// Surface the bound cortex identity on every serve, so an
			// operator who started the wrong cortex sees it in the very
			// first log line instead of finding out via peer drift hours
			// later. The ULID is the federation key — print it explicitly,
			// not just the human-readable name.
			fmt.Printf("[serve] cortex %q (id=%s) at %s\n", cx.Name, cx.ID, cx.Dir)

			var syncer *federation.Syncer
			switch transport {
			case "stdio", "":
				// Stdio is local — no federation guards on tool access.
				s := mcpserver.NewServer(cx, version(), "")
				err = mcpgo.ServeStdio(s)
			case "http":
				// Read the manifest BEFORE validateHTTPServe so the
				// access-key resolution and the keyed-TLS check below
				// share the same view of cortex.md with everything
				// downstream (syncer, middleware).
				m, manifestErr := cortex.ReadManifest(cx.Dir)
				if manifestErr != nil {
					return fmt.Errorf("reading cortex.md: %w", manifestErr)
				}

				// Resolve the shared MCP access key: NOEMA_MCP_KEY env
				// var > sidecar file > open mode. Errors here are hard
				// stops — misconfigured auth must not silently downgrade
				// to open mode.
				accessKey, accessErr := cortex.LoadAccessKey(cx.Dir, m.Access)
				if accessErr != nil {
					return fmt.Errorf("loading MCP access key: %w", accessErr)
				}
				if accessKey.EnvOverride() {
					fmt.Printf("[serve] %s overrides access.shared_key_file at %s\n",
						cortex.AccessKeyEnvVar, accessKey.Path)
				}

				if err := validateHTTPServe(host, tlsCert, tlsKey, cx.Name, cortexFlag != ""); err != nil {
					return err
				}
				useTLS := tlsCert != "" && tlsKey != ""

				if err := requireTLSForKeyedMode(accessKey, useTLS); err != nil {
					return err
				}

				// Federation only runs over the HTTP transport (peers
				// need an HTTP endpoint to call sync_events on). Start
				// the syncer only when the bound cortex actually has
				// peers configured AND the mode is not "publish" (a
				// publish-mode cortex serves events but never pulls).
				fedMode := m.Federation.EffectiveMode()
				if m.Federation != nil && len(m.Federation.Peers) > 0 && fedMode != cortex.FederationModePublish {
					syncer = startSyncer(cx, accessKey.Value, m.Federation)
				}
				fmt.Printf("[serve] federation mode: %s\n", fedMode)

				// HTTP transport: apply federation mode guards.
				s := mcpserver.NewServer(cx, version(), fedMode)

				// Surface the access posture on startup. The
				// fingerprint lets two operators confirm they're
				// running the same key without speaking the secret
				// itself.
				if accessKey.Keyed() {
					fmt.Printf("[serve] access=keyed source=%s fingerprint=%s\n",
						accessKey.Source, accessKey.Fingerprint)
				} else {
					fmt.Printf("[serve] access=open\n")
				}

				// IPv6 addresses need brackets in listen addresses.
				hostForAddr := host
				if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
					hostForAddr = "[" + host + "]"
				}
				addr := fmt.Sprintf("%s:%d", hostForAddr, port)

				// Build the MCP handler. No WithEndpointPath — our
				// own mux routes /mcp. No WithTLSCert — we drive
				// ListenAndServeTLS ourselves so the middleware chain
				// wraps the handler cleanly. NewStreamableHTTPServer
				// still starts mcp-go's session sweeper, which is the
				// only side effect of that call we care about.
				mcpHandler := mcpgo.NewStreamableHTTPServer(s)

				// Middleware chain: CORS (outermost) → Auth → mcpHandler.
				// CORS is outermost because preflight (OPTIONS) cannot
				// carry credentials by spec and must not be 401'd.
				// AuthMiddleware("") is a pass-through in open mode.
				wrapped := mcpserver.CORSMiddleware(
					mcpserver.AuthMiddleware(accessKey.Value)(mcpHandler),
				)

				// Our own mux so non-/mcp paths return 404 cleanly
				// instead of being dispatched through the MCP handler.
				mux := http.NewServeMux()
				mux.Handle("/mcp", wrapped)

				httpSrv := &http.Server{
					Addr:    addr,
					Handler: mux,
				}

				scheme := "http"
				if useTLS {
					scheme = "https"
				}
				fmt.Printf("Starting Streamable HTTP server on %s://%s/mcp\n", scheme, addr)
				if useTLS {
					err = httpSrv.ListenAndServeTLS(tlsCert, tlsKey)
				} else {
					err = httpSrv.ListenAndServe()
				}
			case "sse":
				err = fmt.Errorf(
					"the legacy `sse` transport was removed in this release: noema now speaks Streamable HTTP (MCP 2025-03-26).\n" +
						"  Re-run with --transport http (default endpoint /mcp) and regenerate any\n" +
						"  systemd unit, launchd plist, or .mcp.json that pinned --transport sse.")
			default:
				err = fmt.Errorf("unknown transport %q: use stdio or http", transport)
			}

			if syncer != nil {
				syncer.Stop()
			}
			return err
		},
	}

	cmd.Flags().StringVar(&transport, "transport", "stdio", "transport: stdio or http (Streamable HTTP, MCP 2025-03-26)")
	cmd.Flags().StringVar(&host, "host", "", "listen address for HTTP transport (required, e.g. 127.0.0.1 or LAN IP)")
	cmd.Flags().IntVar(&port, "port", 3000, "port for HTTP transport")
	cmd.Flags().StringVar(&tlsCert, "tls-cert", "", "path to TLS certificate file (enables HTTPS)")
	cmd.Flags().StringVar(&tlsKey, "tls-key", "", "path to TLS private key file")
	cmd.Flags().BoolVar(&printConfig, "print-config", false, "print MCP client config JSON and exit")
	cmd.Flags().BoolVar(&printSystemdUnit, "print-systemd-unit", false, "print a systemd service unit for this serve command and exit")
	cmd.Flags().BoolVar(&printLaunchdPlist, "print-launchd-plist", false, "print a launchd LaunchAgent plist for this serve command and exit")
	return cmd
}

// startSyncer converts the manifest's federation config into a running
// syncer. The caller has already verified that peers exist and the mode
// is not "publish". sharedKey is attached as an Authorization: Bearer
// header on every outbound sync when non-empty; pass "" for open-mode
// peers.
func startSyncer(cx *cortex.Cortex, sharedKey string, fc *cortex.FederationConfig) *federation.Syncer {
	if fc == nil || len(fc.Peers) == 0 {
		return nil
	}

	var peers []federation.PeerConfig
	for _, p := range fc.Peers {
		peers = append(peers, federation.PeerConfig{
			Name: p.Name, Endpoint: p.Endpoint, CA: p.CA, Mode: p.Mode,
		})
	}

	var interval time.Duration
	if fc.Interval != "" {
		interval, _ = time.ParseDuration(fc.Interval)
	}

	state := federation.NewState(cx.DB.DB)
	cfg := federation.Config{
		Mode: fc.EffectiveMode(), Peers: peers, Interval: interval, SharedKey: sharedKey,
	}

	syncer := federation.NewSyncer(cx, state, cfg)
	syncer.Start()

	fmt.Printf("Federation syncer started (%d peers, interval %s)\n", len(peers), cfg.EffectiveInterval())
	return syncer
}

// requireTLSForKeyedMode returns a startup error when shared-key auth is
// active but TLS is not configured. A bearer token sent over plaintext HTTP
// is stolen by the first adversary on the network path, so the combination
// is worse than open mode: it creates the appearance of security while
// leaking the key on every request. Making this a hard error — not a
// warning — is consistent with the --host guardrail's fail-fast posture.
// See docs/design/mcp-auth-plan.md §4.
func requireTLSForKeyedMode(access cortex.AccessKey, useTLS bool) error {
	if !access.Keyed() || useTLS {
		return nil
	}
	return fmt.Errorf(
		"refusing to serve MCP auth over plaintext HTTP: access.shared_key_file is set (source=%s) but --tls-cert/--tls-key are not configured.\n"+
			"  A bearer token sent without TLS is stolen by the first adversary on the network path.\n"+
			"  Provide --tls-cert and --tls-key, or remove access.shared_key_file from cortex.md to run in open mode.",
		access.Source,
	)
}

// validateHTTPServe enforces the flag invariants for `noema serve --transport
// http`. It is split out from RunE so the explicit-cortex requirement (the
// most common federation footgun) can be unit-tested without standing up an
// HTTP server.
//
// cortexExplicit captures whether `--cortex` was passed on the command line.
// NOEMA_CORTEX and cfg.Default are intentionally treated as implicit
// resolution paths that do NOT satisfy the requirement on a network
// transport: the HTTP transport exposes the bound cortex to peers under its
// display name, and silent default-resolution is exactly how a host ends up
// serving the wrong cortex on an endpoint that peers have pinned to a
// specific identity. The peer-side handshake catches the resulting mismatch
// but only logs the failure on the OTHER host, which is the diagnostic
// asymmetry this guard exists to prevent.
func validateHTTPServe(host, tlsCert, tlsKey, cortexName string, cortexExplicit bool) error {
	if host == "" {
		return fmt.Errorf("--host is required for HTTP transport (e.g. --host 10.0.0.1 or --host 127.0.0.1)")
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return fmt.Errorf("binding to %s is not allowed — it may expose your cortex to the network.\n  Use an explicit address: --host 127.0.0.1 (local only) or --host <your-lan-ip> (federation)", host)
	}
	if (tlsCert == "") != (tlsKey == "") {
		return fmt.Errorf("--tls-cert and --tls-key must be provided together")
	}
	if !cortexExplicit {
		return fmt.Errorf(
			"refusing to start HTTP server on cortex %q without an explicit --cortex flag.\n"+
				"  The HTTP transport exposes this cortex's event stream to the network.\n"+
				"  Inheriting the cortex from NOEMA_CORTEX or the config default makes it\n"+
				"  easy to bind the wrong one — and on a host where peers are pinned to a\n"+
				"  specific identity, that produces silent failures on the peer side.\n"+
				"  Re-run with: noema serve --cortex %s --transport http --host %s",
			cortexName, cortexName, host,
		)
	}
	return nil
}

// buildServeArgs returns the argv (starting with "serve") that reproduces the
// current invocation's transport configuration. It's the shared source of
// truth for --print-systemd-unit and --print-launchd-plist so both emit
// identical arguments, and so the unit/plist reflects every flag the
// operator actually passed on the command line that generated it.
func buildServeArgs(cortexName, transport, host string, port int, tlsCert, tlsKey string) []string {
	args := []string{"serve", "--cortex", cortexName, "--transport", transport}
	if host != "" {
		args = append(args, "--host", host)
	}
	if port != 0 {
		args = append(args, "--port", fmt.Sprintf("%d", port))
	}
	if tlsCert != "" {
		args = append(args, "--tls-cert", tlsCert)
	}
	if tlsKey != "" {
		args = append(args, "--tls-key", tlsKey)
	}
	return args
}

// runPrintSystemdUnit writes a systemd service unit for the current serve
// invocation to out. It requires --transport http and an explicit --cortex
// flag — the unit file pins exactly one cortex to supervise, and implicit
// resolution (NOEMA_CORTEX or cfg.Default) wouldn't carry into the systemd
// service environment anyway. All HTTP flag invariants are validated via
// validateHTTPServe so a broken config is caught at preview time rather
// than on first `systemctl start`.
func runPrintSystemdUnit(out io.Writer, transport, host string, port int, tlsCert, tlsKey string) error {
	if cortexFlag == "" {
		return fmt.Errorf("--print-systemd-unit requires an explicit --cortex flag (the unit file pins one cortex to supervise)")
	}
	if transport != "http" {
		return fmt.Errorf("--print-systemd-unit requires --transport http (stdio has no network endpoint to supervise)")
	}
	if err := validateHTTPServe(host, tlsCert, tlsKey, cortexFlag, true); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("looking up current user: %w", err)
	}

	unit := buildSystemdUnit(systemdUnitParams{
		Cortex:    cortexFlag,
		User:      u.Username,
		Exe:       exe,
		ServeArgs: buildServeArgs(cortexFlag, transport, host, port, tlsCert, tlsKey),
	})
	_, err = io.WriteString(out, unit)
	return err
}

// runPrintLaunchdPlist is the macOS counterpart to runPrintSystemdUnit: it
// emits a per-user LaunchAgent plist that reproduces the current serve
// invocation. LaunchAgents run as the loading user, so there's no User
// field — we do need the home directory for the log path, which is the
// only side the two templates diverge on.
//
// Install instructions go to stderr rather than being embedded in the
// plist as an XML comment: the XML 1.0 spec forbids "--" inside comments
// (section 2.5), and the install commands contain flags like --cortex
// that trip that rule. Writing instructions to stderr keeps the plist
// well-formed while still showing operators the install steps when they
// run the command interactively.
func runPrintLaunchdPlist(out io.Writer, transport, host string, port int, tlsCert, tlsKey string) error {
	if cortexFlag == "" {
		return fmt.Errorf("--print-launchd-plist requires an explicit --cortex flag (the plist pins one cortex to supervise)")
	}
	if transport != "http" {
		return fmt.Errorf("--print-launchd-plist requires --transport http (stdio has no network endpoint to supervise)")
	}
	if err := validateHTTPServe(host, tlsCert, tlsKey, cortexFlag, true); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving user home dir: %w", err)
	}

	serveArgs := buildServeArgs(cortexFlag, transport, host, port, tlsCert, tlsKey)
	label := "com.fail-safe.noema." + cortexFlag

	// Install hint to stderr so `--print-launchd-plist > foo.plist`
	// writes only the plist to the file while the operator still sees
	// the install commands in their terminal.
	plistPath := fmt.Sprintf("~/Library/LaunchAgents/%s.plist", label)
	fmt.Fprintf(os.Stderr, "# Generated LaunchAgent plist for Noema cortex %q.\n", cortexFlag)
	fmt.Fprintln(os.Stderr, "# Install with:")
	fmt.Fprintln(os.Stderr, "#")
	fmt.Fprintf(os.Stderr, "#   noema serve %s --print-launchd-plist \\\n", strings.Join(serveArgs[1:], " "))
	fmt.Fprintf(os.Stderr, "#     > %s\n", plistPath)
	fmt.Fprintf(os.Stderr, "#   launchctl bootstrap gui/$(id -u) %s\n", plistPath)
	fmt.Fprintln(os.Stderr, "#")
	fmt.Fprintln(os.Stderr, "# KEYED MODE (MCP shared-key auth):")
	fmt.Fprintf(os.Stderr, "#   The plist contains an EnvironmentVariables block with NOEMA_MCP_KEY=%s\n", launchdKeyPlaceholder)
	fmt.Fprintln(os.Stderr, "#   launchd has no EnvironmentFile= equivalent, so the key must live inside")
	fmt.Fprintln(os.Stderr, "#   the plist itself. Before loading, edit the plist and replace the")
	fmt.Fprintln(os.Stderr, "#   placeholder with your real bearer token, then tighten permissions:")
	fmt.Fprintln(os.Stderr, "#")
	fmt.Fprintf(os.Stderr, "#     chmod 600 %s\n", plistPath)
	fmt.Fprintln(os.Stderr, "#")
	fmt.Fprintln(os.Stderr, "# OPEN MODE (no auth):")
	fmt.Fprintln(os.Stderr, "#   Delete the entire <key>EnvironmentVariables</key>...</dict> block")
	fmt.Fprintln(os.Stderr, "#   before loading. The server will come up in open mode.")
	fmt.Fprintln(os.Stderr, "#")
	fmt.Fprintf(os.Stderr, "# Tail logs: tail -f ~/Library/Logs/noema-%s.log\n", cortexFlag)
	fmt.Fprintf(os.Stderr, "# Stop:      launchctl bootout gui/$(id -u) %s\n", plistPath)
	fmt.Fprintln(os.Stderr)

	plist := buildLaunchdPlist(launchdPlistParams{
		Cortex:    cortexFlag,
		Exe:       exe,
		HomeDir:   home,
		ServeArgs: serveArgs,
	})
	_, err = io.WriteString(out, plist)
	return err
}

// systemdUnitParams carries the inputs for buildSystemdUnit. Keeping this
// as a plain struct (vs. positional args) makes tests self-documenting
// and leaves room for future fields without breaking the call sites.
type systemdUnitParams struct {
	Cortex    string   // e.g. "agentbrain" — pinned into Description and filename suggestion
	User      string   // Linux username the service runs as (from os/user.Current)
	Exe       string   // absolute path to the noema binary (from os.Executable)
	ServeArgs []string // argv after the binary path, starting with "serve"
}

// buildSystemdUnit renders a ready-to-install systemd service unit. It is
// intentionally minimal: no sandboxing directives (NoNewPrivileges,
// ProtectHome, ReadWritePaths) because those require knowing the cortex
// directory and can break legitimate setups where the cortex lives
// outside ~/.noema. Operators who want hardening can add it manually;
// the generated unit works out of the box, which is the goal.
//
// The generator is pure-flag and emits the same unit whether the target
// cortex is keyed or open: the EnvironmentFile= line uses a leading "-"
// so systemd treats it as optional, which means open-mode cortexes
// simply ignore the missing file, and keyed-mode cortexes get their
// NOEMA_MCP_KEY injected when the operator drops the env file in place.
// That keeps the print path testable without loading a live cortex and
// lets operators flip between open and keyed mode without regenerating
// the unit.
func buildSystemdUnit(p systemdUnitParams) string {
	envFile := "%h/.config/noema/" + p.Cortex + ".env"

	var b strings.Builder
	fmt.Fprintf(&b, "# Noema memory server (%s)\n", p.Cortex)
	fmt.Fprintln(&b, "#")
	fmt.Fprintln(&b, "# Generated by `noema serve --print-systemd-unit`. Install with:")
	fmt.Fprintln(&b, "#")
	fmt.Fprintf(&b, "#   noema serve %s --print-systemd-unit \\\n", strings.Join(p.ServeArgs[1:], " "))
	fmt.Fprintf(&b, "#     | sudo tee /etc/systemd/system/noema-%s.service\n", p.Cortex)
	fmt.Fprintln(&b, "#   sudo systemctl daemon-reload")
	fmt.Fprintf(&b, "#   sudo systemctl enable --now noema-%s\n", p.Cortex)
	fmt.Fprintln(&b, "#")
	fmt.Fprintln(&b, "# For keyed-mode MCP auth, create the env file before first start:")
	fmt.Fprintln(&b, "#")
	fmt.Fprintf(&b, "#   install -d -m 700 ~/.config/noema\n")
	fmt.Fprintf(&b, "#   umask 077 && printf 'NOEMA_MCP_KEY=<paste-key>\\n' > %s\n", envFile)
	fmt.Fprintf(&b, "#   chmod 600 %s\n", envFile)
	fmt.Fprintln(&b, "#")
	fmt.Fprintln(&b, "# The EnvironmentFile= line below is prefixed with `-` so the unit")
	fmt.Fprintln(&b, "# still starts in open mode when the env file is absent.")
	fmt.Fprintln(&b, "#")
	fmt.Fprintf(&b, "# Tail logs: sudo journalctl -u noema-%s -f\n", p.Cortex)
	fmt.Fprintf(&b, "# Restart:   sudo systemctl restart noema-%s\n", p.Cortex)
	fmt.Fprintf(&b, "# Stop:      sudo systemctl stop noema-%s\n", p.Cortex)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "[Unit]")
	fmt.Fprintf(&b, "Description=Noema memory server (%s)\n", p.Cortex)
	fmt.Fprintln(&b, "Documentation=https://github.com/Fail-Safe/Noema")
	fmt.Fprintln(&b, "After=network-online.target")
	fmt.Fprintln(&b, "Wants=network-online.target")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "[Service]")
	fmt.Fprintln(&b, "Type=simple")
	fmt.Fprintf(&b, "User=%s\n", p.User)
	fmt.Fprintf(&b, "EnvironmentFile=-%s\n", envFile)
	fmt.Fprintf(&b, "ExecStart=%s %s\n", p.Exe, strings.Join(p.ServeArgs, " "))
	fmt.Fprintln(&b, "Restart=on-failure")
	fmt.Fprintln(&b, "RestartSec=5s")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "[Install]")
	fmt.Fprintln(&b, "WantedBy=multi-user.target")
	return b.String()
}

// launchdPlistParams carries the inputs for buildLaunchdPlist. HomeDir is
// used only for the log path (~/Library/Logs/noema-<cortex>.log); the
// agent itself runs as the user who loads the plist.
type launchdPlistParams struct {
	Cortex    string   // e.g. "agentbrain" — pinned into Label and filename
	Exe       string   // absolute path to the noema binary
	HomeDir   string   // user home dir (from os.UserHomeDir) for log path
	ServeArgs []string // argv after the binary path, starting with "serve"
}

// launchdKeyPlaceholder is the literal value emitted in the plist's
// EnvironmentVariables dict for NOEMA_MCP_KEY. Operators must replace
// it with the real key before loading the plist, OR delete the whole
// EnvironmentVariables block for open mode. The stderr install hints
// printed by runPrintLaunchdPlist call this out explicitly.
//
// launchd has no equivalent of systemd's EnvironmentFile= directive, so
// unlike the systemd unit we cannot reference an external secret file:
// the plist itself becomes secret-bearing in keyed mode. The placeholder
// makes that requirement visible in the generated file rather than
// leaving it buried in documentation.
const launchdKeyPlaceholder = "REPLACE_WITH_BEARER_KEY_OR_DELETE_THIS_DICT"

// buildLaunchdPlist renders a per-user LaunchAgent plist. Every text value
// that could come from user input (cortex name, paths) is run through
// xml.EscapeText so a weirdly-named cortex or a path with an ampersand
// can't produce malformed XML that launchd would reject. The output
// contains no XML comment block — install instructions are printed to
// stderr by runPrintLaunchdPlist because the XML 1.0 spec forbids "--"
// inside comments, and our install commands contain long-flag syntax.
func buildLaunchdPlist(p launchdPlistParams) string {
	label := "com.fail-safe.noema." + p.Cortex
	logPath := p.HomeDir + "/Library/Logs/noema-" + p.Cortex + ".log"

	var b strings.Builder
	fmt.Fprintln(&b, `<?xml version="1.0" encoding="UTF-8"?>`)
	fmt.Fprintln(&b, `<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`)
	fmt.Fprintln(&b, `<plist version="1.0">`)
	fmt.Fprintln(&b, "<dict>")
	fmt.Fprintln(&b, "    <key>Label</key>")
	fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(label))
	fmt.Fprintln(&b, "    <key>ProgramArguments</key>")
	fmt.Fprintln(&b, "    <array>")
	fmt.Fprintf(&b, "        <string>%s</string>\n", xmlEscape(p.Exe))
	for _, arg := range p.ServeArgs {
		fmt.Fprintf(&b, "        <string>%s</string>\n", xmlEscape(arg))
	}
	fmt.Fprintln(&b, "    </array>")
	fmt.Fprintln(&b, "    <key>EnvironmentVariables</key>")
	fmt.Fprintln(&b, "    <dict>")
	fmt.Fprintln(&b, "        <key>NOEMA_MCP_KEY</key>")
	fmt.Fprintf(&b, "        <string>%s</string>\n", xmlEscape(launchdKeyPlaceholder))
	fmt.Fprintln(&b, "    </dict>")
	fmt.Fprintln(&b, "    <key>RunAtLoad</key>")
	fmt.Fprintln(&b, "    <true/>")
	fmt.Fprintln(&b, "    <key>KeepAlive</key>")
	fmt.Fprintln(&b, "    <dict>")
	fmt.Fprintln(&b, "        <key>SuccessfulExit</key>")
	fmt.Fprintln(&b, "        <false/>")
	fmt.Fprintln(&b, "    </dict>")
	fmt.Fprintln(&b, "    <key>StandardOutPath</key>")
	fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(logPath))
	fmt.Fprintln(&b, "    <key>StandardErrorPath</key>")
	fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(logPath))
	fmt.Fprintln(&b, "</dict>")
	fmt.Fprintln(&b, "</plist>")
	return b.String()
}

// xmlEscape wraps xml.EscapeText for use inside element content. We never
// emit untrusted data into attribute values or comments, so element-text
// escaping is sufficient — it handles &, <, and > which are the only
// characters that would break a well-formed plist.
func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// runPrintMCPConfig writes a ready-to-use .mcp.json snippet to stdout.
// For stdio transport (the default), it emits a `command` + `args` entry
// that Claude Code and other stdio-based MCP clients expect. For http
// transport, it emits a `url` + `headers` entry pointing at the
// /mcp Streamable HTTP endpoint with an Authorization header literal
// of "Bearer ${NOEMA_MCP_KEY}". That placeholder is emitted verbatim
// rather than pulling the live key for two reasons:
//
//  1. the file is typically committed to source control alongside the
//     rest of the repo's .mcp.json, and
//  2. MCP clients that support env interpolation in headers (Claude
//     Code does) will resolve it at runtime, while clients that do not
//     will produce an obvious 401 with a bearer string they can search
//     for and edit, rather than a silent "wrong key" failure.
//
// The cortex name is resolved via the same priority chain as resolveCortex
// (flag > env > config default). Transport-specific flags are plumbed
// through from RunE so a single `noema serve --print-config --transport
// http --host 10.0.0.1 ...` invocation produces the exact config a
// remote client would need, including the URL scheme that matches the
// --tls-cert/--tls-key pair.
func runPrintMCPConfig(out io.Writer, transport, host string, port int, tlsCert, tlsKey string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	name := cortexFlag
	if name == "" {
		name = os.Getenv("NOEMA_CORTEX")
	}
	if name == "" {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}
		name = cfg.Default
	}

	var entry map[string]any
	switch transport {
	case "http":
		// For the http entry we must be able to form a URL, so --host
		// and --port are required. --tls-cert/--tls-key are optional;
		// their presence determines https vs. http. We reuse
		// validateHTTPServe's sub-checks (host required, not 0.0.0.0,
		// tls pair coherent) but don't gate on cortexExplicit here —
		// the .mcp.json snippet is emitted for a client that's talking
		// TO a server, not launching one, so there's no network-
		// exposure risk in printing an http config without --cortex.
		if host == "" {
			return fmt.Errorf("--print-config --transport http requires --host (the address a client will dial, e.g. 10.0.0.1 or my.lan)")
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
			return fmt.Errorf("--host %s is a wildcard bind address; pass the address clients should dial instead", host)
		}
		if (tlsCert == "") != (tlsKey == "") {
			return fmt.Errorf("--tls-cert and --tls-key must be provided together")
		}
		scheme := "http"
		if tlsCert != "" && tlsKey != "" {
			scheme = "https"
		}
		// IPv6 addresses need brackets in URLs.
		hostForURL := host
		if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
			hostForURL = "[" + host + "]"
		}
		entry = map[string]any{
			"url": fmt.Sprintf("%s://%s:%d/mcp", scheme, hostForURL, port),
			"headers": map[string]any{
				"Authorization": "Bearer ${NOEMA_MCP_KEY}",
			},
		}
	case "stdio", "":
		serveArgs := []string{"serve"}
		if name != "" {
			serveArgs = append(serveArgs, "--cortex", name)
		}
		entry = map[string]any{
			"command": exe,
			"args":    serveArgs,
		}
	default:
		return fmt.Errorf("--print-config does not support --transport %q (use stdio or http)", transport)
	}

	payload := map[string]any{
		"mcpServers": map[string]any{
			"noema": entry,
		},
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}
