package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/federation"
)

func federationCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "federation",
		Short:   "Federation management",
		Aliases: []string{"fed"},
	}

	cmd.AddCommand(
		federationStatusCmd(),
		federationPeersCmd(),
		federationAddPeerCmd(),
	)
	return cmd
}

func federationStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show federation status, peer sync state, and vector clock",
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			m, err := cortex.ReadManifest(cx.Dir)
			if err != nil {
				m = cortex.Manifest{Name: cx.Name}
			}

			fmt.Printf("Cortex: %s\n\n", m.Name)

			if m.Federation == nil || len(m.Federation.Peers) == 0 {
				fmt.Println("Federation: not configured (no peers in cortex.md)")
				return nil
			}

			fmt.Printf("Peers: %d\n", len(m.Federation.Peers))
			if m.Federation.Interval != "" {
				fmt.Printf("Interval: %s\n", m.Federation.Interval)
			}
			fmt.Println()

			state := federation.NewState(cx.DB.DB)
			for _, p := range m.Federation.Peers {
				ps, err := state.GetPeerState(p.Name, p.Endpoint)
				if err != nil {
					fmt.Printf("  %s (%s): error loading state\n", p.Name, p.Endpoint)
					continue
				}
				lastSeen := "(never)"
				if ps.LastSeen != "" {
					lastSeen = ps.LastSeen
				}
				lastEvent := "(none)"
				if ps.LastEvent != "" {
					lastEvent = ps.LastEvent
				}
				fmt.Printf("  %s\n    endpoint:   %s\n    last_seen:  %s\n    last_event: %s\n",
					p.Name, p.Endpoint, lastSeen, lastEvent)
			}

			vc, err := cx.GetClock()
			if err == nil && len(vc) > 0 {
				fmt.Println("\nVector Clock:")
				for peer, tick := range vc {
					fmt.Printf("  %s: %d\n", peer, tick)
				}
			}

			return nil
		},
	}
}

func federationPeersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "peers",
		Short: "List configured federation peers",
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			m, err := cortex.ReadManifest(cx.Dir)
			if err != nil {
				return err
			}

			if m.Federation == nil || len(m.Federation.Peers) == 0 {
				fmt.Println("No peers configured. Add peers to cortex.md under the federation section.")
				return nil
			}

			for _, p := range m.Federation.Peers {
				fmt.Printf("  %s  %s\n", p.Name, p.Endpoint)
			}
			return nil
		},
	}
}

func federationAddPeerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add-peer <name> <endpoint>",
		Short: "Add a federation peer to cortex.md",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			endpoint := strings.TrimRight(args[1], "/")

			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			m, err := cortex.ReadManifest(cx.Dir)
			if err != nil {
				return err
			}

			// Reject if the proposed label collides with this cortex's own name.
			// Two participants in a federation must have distinct names or their
			// vector clocks merge into one bucket and concurrent edits stop being
			// detected. See docs/design/cortex-uuid-plan.md for the permanent fix.
			if m.PeerLabelCollidesWithSelf(name) {
				return fmt.Errorf("peer name %q matches this cortex's own name; pick a different label to avoid federation identity collisions", name)
			}

			// Check for duplicate.
			if m.Federation != nil {
				for _, p := range m.Federation.Peers {
					if p.Name == name {
						return fmt.Errorf("peer %q is already configured", name)
					}
				}
			}

			if m.Federation == nil {
				m.Federation = &cortex.FederationConfig{}
			}
			m.Federation.Peers = append(m.Federation.Peers, cortex.PeerEntry{
				Name:     name,
				Endpoint: endpoint,
			})

			if err := cortex.WriteManifest(cx.Dir, m); err != nil {
				return err
			}

			fmt.Printf("Added peer %q (%s) to cortex.md\n", name, endpoint)
			return nil
		},
	}
}
