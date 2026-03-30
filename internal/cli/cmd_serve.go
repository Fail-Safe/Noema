package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/config"
	mcpserver "github.com/Fail-Safe/Noema/internal/mcp"
	mcpgo "github.com/mark3labs/mcp-go/server"
)

func serveCmd() *cobra.Command {
	var (
		transport   string
		port        int
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

			switch transport {
			case "stdio", "":
				return mcpgo.ServeStdio(s)
			case "sse":
				addr := fmt.Sprintf(":%d", port)
				baseURL := fmt.Sprintf("http://localhost:%d", port)
				fmt.Printf("Starting SSE server on %s\n", addr)
				return mcpgo.NewSSEServer(s, mcpgo.WithBaseURL(baseURL)).Start(addr)
			default:
				return fmt.Errorf("unknown transport %q: use stdio or sse", transport)
			}
		},
	}

	cmd.Flags().StringVar(&transport, "transport", "stdio", "transport: stdio or sse")
	cmd.Flags().IntVar(&port, "port", 3000, "port for SSE transport")
	cmd.Flags().BoolVar(&printConfig, "print-config", false, "print MCP client config JSON and exit")
	return cmd
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
