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
				return runPrintMCPConfig()
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

			s := mcpserver.NewServer(cx, version())

			var syncer *federation.Syncer
			switch transport {
			case "stdio", "":
				err = mcpgo.ServeStdio(s)
			case "sse":
				if err := validateSSEServe(host, tlsCert, tlsKey, cx.Name, cortexFlag != ""); err != nil {
					return err
				}
				useTLS := tlsCert != "" && tlsKey != ""

				// Federation only runs in SSE mode (peers need an HTTP
				// endpoint to call sync_events on). Start the syncer only
				// when the bound cortex actually has peers configured.
				m, manifestErr := cortex.ReadManifest(cx.Dir)
				if manifestErr == nil && m.Federation != nil && len(m.Federation.Peers) > 0 {
					syncer = startSyncer(cx)
				}

				// IPv6 addresses need brackets in URLs and listen addresses.
				hostForURL := host
				if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
					hostForURL = "[" + host + "]"
				}

				scheme := "http"
				if useTLS {
					scheme = "https"
				}
				addr := fmt.Sprintf("%s:%d", hostForURL, port)
				baseURL := fmt.Sprintf("%s://%s:%d", scheme, hostForURL, port)
				sseServer := mcpgo.NewSSEServer(s,
					mcpgo.WithBaseURL(baseURL),
					mcpgo.WithUseFullURLForMessageEndpoint(false),
				)

				if useTLS {
					fmt.Printf("Starting SSE server on %s (TLS)\n", addr)
					httpSrv := &http.Server{Addr: addr, Handler: sseServer}
					err = httpSrv.ListenAndServeTLS(tlsCert, tlsKey)
				} else {
					fmt.Printf("Starting SSE server on %s\n", addr)
					err = sseServer.Start(addr)
				}
			default:
				err = fmt.Errorf("unknown transport %q: use stdio or sse", transport)
			}

			if syncer != nil {
				syncer.Stop()
			}
			return err
		},
	}

	cmd.Flags().StringVar(&transport, "transport", "stdio", "transport: stdio or sse")
	cmd.Flags().StringVar(&host, "host", "", "listen address for SSE transport (required, e.g. 127.0.0.1 or LAN IP)")
	cmd.Flags().IntVar(&port, "port", 3000, "port for SSE transport")
	cmd.Flags().StringVar(&tlsCert, "tls-cert", "", "path to TLS certificate file (enables HTTPS)")
	cmd.Flags().StringVar(&tlsKey, "tls-key", "", "path to TLS private key file")
	cmd.Flags().BoolVar(&printConfig, "print-config", false, "print MCP client config JSON and exit")
	cmd.Flags().BoolVar(&printSystemdUnit, "print-systemd-unit", false, "print a systemd service unit for this serve command and exit")
	cmd.Flags().BoolVar(&printLaunchdPlist, "print-launchd-plist", false, "print a launchd LaunchAgent plist for this serve command and exit")
	return cmd
}

// startSyncer reads the cortex manifest and, if federation peers are configured,
// starts a background syncer that polls remote peers for new events.
// Returns nil if federation is not configured.
func startSyncer(cx *cortex.Cortex) *federation.Syncer {
	m, err := cortex.ReadManifest(cx.Dir)
	if err != nil || m.Federation == nil || len(m.Federation.Peers) == 0 {
		return nil
	}

	var peers []federation.PeerConfig
	for _, p := range m.Federation.Peers {
		peers = append(peers, federation.PeerConfig{Name: p.Name, Endpoint: p.Endpoint, CA: p.CA})
	}

	var interval time.Duration
	if m.Federation.Interval != "" {
		interval, _ = time.ParseDuration(m.Federation.Interval)
	}

	state := federation.NewState(cx.DB.DB)
	cfg := federation.Config{Peers: peers, Interval: interval}

	syncer := federation.NewSyncer(cx, state, cfg)
	syncer.Start()

	fmt.Printf("Federation syncer started (%d peers, interval %s)\n", len(peers), cfg.EffectiveInterval())
	return syncer
}

// validateSSEServe enforces the flag invariants for `noema serve --transport
// sse`. It is split out from RunE so the explicit-cortex requirement (the
// most common federation footgun) can be unit-tested without standing up an
// SSE server.
//
// cortexExplicit captures whether `--cortex` was passed on the command line.
// NOEMA_CORTEX and cfg.Default are intentionally treated as implicit
// resolution paths that do NOT satisfy the requirement on a network
// transport: SSE exposes the bound cortex to peers under its display name,
// and silent default-resolution is exactly how a host ends up serving the
// wrong cortex on an endpoint that peers have pinned to a specific identity.
// The peer-side handshake catches the resulting mismatch but only logs the
// failure on the OTHER host, which is the diagnostic asymmetry this guard
// exists to prevent.
func validateSSEServe(host, tlsCert, tlsKey, cortexName string, cortexExplicit bool) error {
	if host == "" {
		return fmt.Errorf("--host is required for SSE transport (e.g. --host 10.0.0.1 or --host 127.0.0.1)")
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return fmt.Errorf("binding to %s is not allowed — it may expose your cortex to the network.\n  Use an explicit address: --host 127.0.0.1 (local only) or --host <your-lan-ip> (federation)", host)
	}
	if (tlsCert == "") != (tlsKey == "") {
		return fmt.Errorf("--tls-cert and --tls-key must be provided together")
	}
	if !cortexExplicit {
		return fmt.Errorf(
			"refusing to start SSE server on cortex %q without an explicit --cortex flag.\n"+
				"  SSE exposes this cortex's event stream to the network. Inheriting\n"+
				"  the cortex from NOEMA_CORTEX or the config default makes it easy to\n"+
				"  bind the wrong one — and on a host where peers are pinned to a\n"+
				"  specific identity, that produces silent failures on the peer side.\n"+
				"  Re-run with: noema serve --cortex %s --transport sse --host %s",
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
// invocation to out. It requires --transport sse and an explicit --cortex
// flag — the unit file pins exactly one cortex to supervise, and implicit
// resolution (NOEMA_CORTEX or cfg.Default) wouldn't carry into the systemd
// service environment anyway. All SSE flag invariants are validated via
// validateSSEServe so a broken config is caught at preview time rather than
// on first `systemctl start`.
func runPrintSystemdUnit(out io.Writer, transport, host string, port int, tlsCert, tlsKey string) error {
	if cortexFlag == "" {
		return fmt.Errorf("--print-systemd-unit requires an explicit --cortex flag (the unit file pins one cortex to supervise)")
	}
	if transport != "sse" {
		return fmt.Errorf("--print-systemd-unit requires --transport sse (stdio has no network endpoint to supervise)")
	}
	if err := validateSSEServe(host, tlsCert, tlsKey, cortexFlag, true); err != nil {
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
	if transport != "sse" {
		return fmt.Errorf("--print-launchd-plist requires --transport sse (stdio has no network endpoint to supervise)")
	}
	if err := validateSSEServe(host, tlsCert, tlsKey, cortexFlag, true); err != nil {
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
	fmt.Fprintf(os.Stderr, "# Generated LaunchAgent plist for Noema cortex %q.\n", cortexFlag)
	fmt.Fprintln(os.Stderr, "# Install with:")
	fmt.Fprintln(os.Stderr, "#")
	fmt.Fprintf(os.Stderr, "#   noema serve %s --print-launchd-plist \\\n", strings.Join(serveArgs[1:], " "))
	fmt.Fprintf(os.Stderr, "#     > ~/Library/LaunchAgents/%s.plist\n", label)
	fmt.Fprintf(os.Stderr, "#   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/%s.plist\n", label)
	fmt.Fprintln(os.Stderr, "#")
	fmt.Fprintf(os.Stderr, "# Tail logs: tail -f ~/Library/Logs/noema-%s.log\n", cortexFlag)
	fmt.Fprintf(os.Stderr, "# Stop:      launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/%s.plist\n", label)
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
func buildSystemdUnit(p systemdUnitParams) string {
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
// It resolves the binary path via os.Executable and the cortex name via the
// same priority chain used by resolveCortex (flag > env > config default).
func runPrintMCPConfig() error {
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

	serveArgs := []string{"serve"}
	if name != "" {
		serveArgs = append(serveArgs, "--cortex", name)
	}

	out := map[string]any{
		"mcpServers": map[string]any{
			"noema": map[string]any{
				"command": exe,
				"args":    serveArgs,
			},
		},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
