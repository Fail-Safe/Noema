package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	mcpserver "github.com/Fail-Safe/Noema/internal/mcp"
	mcpgo "github.com/mark3labs/mcp-go/server"
)

func serveCmd() *cobra.Command {
	var (
		transport string
		port      int
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the MCP server",
		Aliases: []string{"server"},
		RunE: func(cmd *cobra.Command, args []string) error {
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
	return cmd
}
