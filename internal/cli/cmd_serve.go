package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
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
		transport   string
		host        string
		port        int
		tlsCert     string
		tlsKey      string
		printConfig bool
	)

	cmd := &cobra.Command{
		Use:     "serve",
		Short:   "Start the MCP server",
		Aliases: []string{"server"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if printConfig {
				return runPrintMCPConfig()
			}

			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			// Don't defer cx.Close() — server runs until interrupted.

			s := mcpserver.NewServer(cx)

			// Start federation syncer if peers are configured.
			syncer := startSyncer(cx)

			switch transport {
			case "stdio", "":
				err = mcpgo.ServeStdio(s)
			case "sse":
				if host == "" {
					return fmt.Errorf("--host is required for SSE transport (e.g. --host 10.0.0.1 or --host 127.0.0.1)")
				}
				if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
					return fmt.Errorf("binding to %s is not allowed — it may expose your cortex to the network.\n  Use an explicit address: --host 127.0.0.1 (local only) or --host <your-lan-ip> (federation)", host)
				}
				if (tlsCert == "") != (tlsKey == "") {
					return fmt.Errorf("--tls-cert and --tls-key must be provided together")
				}
				useTLS := tlsCert != "" && tlsKey != ""

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
