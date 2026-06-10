package cli

import (
	"fmt"
	"io"
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
		federationResetPeerCmd(),
		federationSetModeCmd(),
		federationPausePeerCmd(),
		federationResumePeerCmd(),
		federationKeyCmd(),
	)
	return cmd
}

// federationKeyCmd groups subcommands that inspect or manage the MCP shared
// access key. `fingerprint` is the only subcommand for now; rotate / path /
// show-source are natural follow-ons that live under the same umbrella.
func federationKeyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Inspect or manage the MCP shared access key",
	}
	cmd.AddCommand(federationKeyFingerprintCmd())
	return cmd
}

// federationKeyFingerprintCmd prints the SHA-256 fingerprint of the
// currently-active shared key, resolved via the same env > file > open
// priority chain `noema serve` uses. It exists so two operators can
// confirm they're holding the same key over an out-of-band channel
// (Signal, phone) without speaking the secret itself — the fingerprint
// is safe to say aloud.
func federationKeyFingerprintCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fingerprint",
		Short: "Print the SHA-256 fingerprint of the active MCP shared key",
		Long: `Resolves the active MCP shared key using the same priority chain as ` + "`noema serve`" + `:
NOEMA_MCP_KEY > access.shared_key_file > open mode. Prints the SHA-256
fingerprint of the key (SSH-style format, safe to say aloud) so two
operators can confirm over an out-of-band channel that they're holding
the same secret.

In open mode (no key configured), prints a message and exits successfully.`,
		Example: "  noema federation key fingerprint\n  NOEMA_MCP_KEY=... noema federation key fingerprint",
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			m, err := cortex.ReadManifest(cx.Dir)
			if err != nil {
				return fmt.Errorf("reading cortex.md: %w", err)
			}

			key, err := cortex.LoadAccessKey(cx.Dir, m.Access)
			if err != nil {
				return fmt.Errorf("loading access key: %w", err)
			}

			out := cmd.OutOrStdout()
			if !key.Keyed() {
				fmt.Fprintf(out, "Cortex:      %s\n", cx.Name)
				fmt.Fprintln(out, "Access:      open mode (no key configured)")
				return nil
			}

			fmt.Fprintf(out, "Cortex:      %s\n", cx.Name)
			fmt.Fprintf(out, "Source:      %s\n", key.Source)
			if key.Path != "" {
				fmt.Fprintf(out, "Path:        %s\n", key.Path)
			}
			fmt.Fprintf(out, "Fingerprint: %s\n", key.Fingerprint)
			if key.EnvOverride() {
				fmt.Fprintf(out, "Note:        %s is overriding access.shared_key_file\n", cortex.AccessKeyEnvVar)
			}
			return nil
		},
	}
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

			fmt.Printf("Cortex: %s\n", m.Name)

			// Surface the active MCP access posture regardless of whether
			// federation is configured. An operator inspecting status on a
			// stdio-only cortex still wants to know whether the same cortex
			// would come up keyed or open if they flipped it to http.
			// Resolution errors here are non-fatal: print a warning line
			// and keep going so the rest of the status output still lands.
			if key, err := cortex.LoadAccessKey(cx.Dir, m.Access); err != nil {
				fmt.Printf("Access: error loading key: %v\n", err)
			} else if key.Keyed() {
				fmt.Printf("Access: keyed (source=%s, fingerprint=%s)\n", key.Source, key.Fingerprint)
			} else {
				fmt.Println("Access: open")
			}
			fmt.Println()

			if m.Federation == nil || len(m.Federation.Peers) == 0 {
				fmt.Println("Federation: not configured (no peers in cortex.md)")
				return nil
			}

			fmt.Printf("Mode: %s\n", m.Federation.EffectiveMode())
			fmt.Printf("Peers: %d\n", len(m.Federation.Peers))
			if m.Federation.Interval != "" {
				fmt.Printf("Interval: %s\n", m.Federation.Interval)
			}

			state := federation.NewState(cx.DB.DB)

			// Surface the local consolidation rank (plan §14). Empty or
			// zero entries render as "(ineligible)" so an operator can
			// see at a glance whether coordination is armed.
			if localRank, rerr := state.GetLocalRank(); rerr == nil {
				fmt.Printf("Consolidation Rank: %s\n", formatFederationRank(localRank))
			}
			fmt.Println()

			localVersion := version()
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
				peerMode := p.EffectiveMode()
				peerRank := "(none)"
				if pr, perr := state.GetPeerRank(p.Name); perr == nil && pr.ObservedAt != "" {
					peerRank = formatFederationRank(pr)
				}
				fmt.Printf("  %s\n    endpoint:   %s\n    mode:       %s\n",
					p.Name, p.Endpoint, peerMode)
				renderPeerVersion(cmd.OutOrStdout(), ps.Health, localVersion)
				fmt.Fprintf(cmd.OutOrStdout(), "    rank:       %s\n    last_seen:  %s\n    last_event: %s\n",
					peerRank, lastSeen, lastEvent)
				renderPeerHealth(cmd.OutOrStdout(), ps.Health)
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

func federationResetPeerCmd() *cobra.Command {
	var assumeYes bool

	cmd := &cobra.Command{
		Use:   "reset-peer <name>...",
		Short: "Clear stored state for one or more peers (forces a fresh handshake)",
		Long: `Clears the pinned cortex_id, last-event cursor, last-seen timestamp, and
local vector-clock bucket for one or more configured peers. The peer entry
in cortex.md is left untouched — only the runtime state in federation_state
and the dead vclock bucket are removed.

Use this when a peer's identity legitimately changed (e.g. it ran
` + "`noema migrate cortex-id --reset`" + `, was restored from a backup, or was
re-paired) and the local syncer is now refusing to talk to it with a
"peer identity mismatch" error. After the reset, the next sync re-pins
the peer's new cortex_id and the cursor restarts from the beginning of
the peer's event log so no events are silently skipped.

This is the supported way to clear stale federation state — never edit
the federation_state SQLite table by hand.`,
		Example: "  noema federation reset-peer peer-b\n  noema federation reset-peer peer-b peer-c --yes",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			m, err := cortex.ReadManifest(cx.Dir)
			if err != nil {
				return fmt.Errorf("reading cortex.md: %w", err)
			}

			// Build the set of configured peers so unknown names are rejected
			// up front. Reset-peer is destructive enough that a typo silently
			// becoming a no-op (or, worse, eating an unrelated key) would be
			// a foot-gun.
			configured := map[string]string{} // name -> endpoint
			if m.Federation != nil {
				for _, p := range m.Federation.Peers {
					configured[p.Name] = p.Endpoint
				}
			}
			for _, name := range args {
				if _, ok := configured[name]; !ok {
					known := make([]string, 0, len(configured))
					for k := range configured {
						known = append(known, k)
					}
					return fmt.Errorf(
						"peer %q is not configured in cortex.md (known: %s)",
						name, strings.Join(known, ", "),
					)
				}
			}

			return runFederationResetPeer(cmd.OutOrStdout(), cmd.InOrStdin(), cx, args, configured, assumeYes)
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

// runFederationResetPeer is split out so tests can drive it without going
// through cobra. It assumes args have already been validated against
// cortex.md so it never sees an unknown peer name.
func runFederationResetPeer(out io.Writer, in io.Reader, cx *cortex.Cortex, names []string, endpoints map[string]string, assumeYes bool) error {
	state := federation.NewState(cx.DB.DB)

	// Snapshot what we're about to delete so the prompt actually shows the
	// operator what will be lost. We also remember each peer's pinned ID so
	// we can drop the matching vector-clock bucket — once the pin is gone,
	// the bucket is unreachable and would otherwise linger forever.
	type snapshot struct {
		name      string
		endpoint  string
		pinnedID  string
		cursor    string
		lastSeen  string
		hadAnyRow bool
	}
	snaps := make([]snapshot, 0, len(names))
	for _, name := range names {
		ps, err := state.GetPeerState(name, endpoints[name])
		if err != nil {
			return fmt.Errorf("loading state for peer %q: %w", name, err)
		}
		snaps = append(snaps, snapshot{
			name:      name,
			endpoint:  endpoints[name],
			pinnedID:  ps.CortexID,
			cursor:    ps.LastEvent,
			lastSeen:  ps.LastSeen,
			hadAnyRow: ps.CortexID != "" || ps.LastEvent != "" || ps.LastSeen != "",
		})
	}

	fmt.Fprintf(out, "About to reset federation state for %d peer(s) in cortex %q:\n", len(snaps), cx.Name)
	for _, s := range snaps {
		fmt.Fprintf(out, "  %s (%s)\n", s.name, s.endpoint)
		fmt.Fprintf(out, "    pinned cortex_id: %s\n", emptyDash(s.pinnedID))
		fmt.Fprintf(out, "    last_event:       %s\n", emptyDash(s.cursor))
		fmt.Fprintf(out, "    last_seen:        %s\n", emptyDash(s.lastSeen))
	}
	fmt.Fprintln(out, "After reset, the next sync will re-pin the peer's current cortex_id")
	fmt.Fprintln(out, "and replay events from the beginning of its log.")

	if !assumeYes {
		fmt.Fprint(out, "Proceed? [y/N]: ")
		var resp string
		_, _ = fmt.Fscanln(in, &resp)
		if resp != "y" && resp != "Y" && resp != "yes" {
			return fmt.Errorf("aborted by user")
		}
	}

	// Load the vclock once, mutate it for every peer whose pinned id we're
	// dropping, and write it back at the end. We do not drop buckets for
	// peers that have never pinned — there's nothing to drop, and we have
	// no way to know which bucket would be theirs without the pin.
	vc, err := state.GetClock()
	if err != nil {
		return fmt.Errorf("loading vector clock: %w", err)
	}
	bucketsDropped := 0

	for _, s := range snaps {
		if err := state.Delete(federation.PeerCortexIDKey(s.name)); err != nil {
			return fmt.Errorf("clearing pin for peer %q: %w", s.name, err)
		}
		if err := state.Delete(federation.PeerCursorKey(s.name)); err != nil {
			return fmt.Errorf("clearing cursor for peer %q: %w", s.name, err)
		}
		if err := state.Delete(federation.PeerSeenKey(s.name)); err != nil {
			return fmt.Errorf("clearing last_seen for peer %q: %w", s.name, err)
		}
		if s.pinnedID != "" {
			if _, ok := vc[s.pinnedID]; ok {
				delete(vc, s.pinnedID)
				bucketsDropped++
			}
			// Drop the pinned federation signing key for this cortex_id too,
			// so a peer that rotated its key (`noema keygen --force`) gets
			// re-pinned cleanly on the next handshake instead of failing the
			// signing-key mismatch check forever.
			if err := state.Delete(federation.CortexPubKeyKey(s.pinnedID)); err != nil {
				return fmt.Errorf("clearing signing-key pin for peer %q: %w", s.name, err)
			}
		}
	}

	if bucketsDropped > 0 {
		if err := state.SetClock(vc); err != nil {
			return fmt.Errorf("saving vector clock: %w", err)
		}
	}

	fmt.Fprintf(out, "\nReset complete.\n")
	for _, s := range snaps {
		if s.hadAnyRow {
			fmt.Fprintf(out, "  %s: state cleared\n", s.name)
		} else {
			fmt.Fprintf(out, "  %s: nothing to clear (peer had no stored state)\n", s.name)
		}
	}
	if bucketsDropped > 0 {
		fmt.Fprintf(out, "  vector-clock buckets dropped: %d\n", bucketsDropped)
	}
	fmt.Fprintln(out, "Restart `noema serve` (or wait for the next syncer poll) to re-pin the peers.")
	return nil
}

func federationSetModeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-mode <sync|publish|subscribe>",
		Short: "Set the federation mode for this cortex",
		Long: `Sets the cortex-level federation mode in cortex.md:

  sync        Bidirectional: pull events from peers and serve events to them (default)
  publish     Outbound only: serve events but never pull; write tools are blocked on HTTP
  subscribe   Inbound only: pull events from peers but refuse to serve them

Changes take effect on the next ` + "`noema serve`" + ` restart.`,
		Example: "  noema federation set-mode publish\n  noema federation set-mode sync",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := args[0]
			switch mode {
			case cortex.FederationModeSync, cortex.FederationModePublish, cortex.FederationModeSubscribe:
			default:
				return fmt.Errorf("invalid mode %q; use sync, publish, or subscribe", mode)
			}

			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			m, err := cortex.ReadManifest(cx.Dir)
			if err != nil {
				return err
			}

			if m.Federation == nil {
				m.Federation = &cortex.FederationConfig{}
			}

			prev := m.Federation.EffectiveMode()
			if mode == cortex.FederationModeSync {
				m.Federation.Mode = "" // omitempty: sync is the default
			} else {
				m.Federation.Mode = mode
			}

			if err := cortex.WriteManifest(cx.Dir, m); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if prev == mode {
				fmt.Fprintf(out, "Federation mode is already %q — no change.\n", mode)
				return nil
			}

			fmt.Fprintf(out, "Federation mode changed: %s -> %s\n\n", prev, mode)
			switch mode {
			case cortex.FederationModePublish:
				fmt.Fprintln(out, "Behavior:")
				fmt.Fprintln(out, "  - The syncer will NOT start (no pulling from peers)")
				fmt.Fprintln(out, "  - sync_events will continue serving events to peers")
				fmt.Fprintln(out, "  - Write tools (create/update/delete) are blocked on HTTP")
				fmt.Fprintln(out, "  - Local stdio transport retains full write access")
			case cortex.FederationModeSubscribe:
				fmt.Fprintln(out, "Behavior:")
				fmt.Fprintln(out, "  - The syncer will pull events from peers normally")
				fmt.Fprintln(out, "  - sync_events will refuse to serve events")
				fmt.Fprintln(out, "  - Write tools remain available on all transports")
			case cortex.FederationModeSync:
				fmt.Fprintln(out, "Behavior:")
				fmt.Fprintln(out, "  - The syncer will pull events from peers")
				fmt.Fprintln(out, "  - sync_events will serve events to peers")
				fmt.Fprintln(out, "  - Write tools available on all transports")
			}
			fmt.Fprintln(out, "\nRestart `noema serve` for the change to take effect.")
			return nil
		},
	}
}

func federationPausePeerCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "pause-peer <name>",
		Short:   "Pause syncing with a federation peer",
		Long:    "Sets a peer's mode to \"paused\" in cortex.md. The syncer will skip this peer\nuntil it is resumed. No state is lost — the cursor and pinned identity are preserved.",
		Example: "  noema federation pause-peer peer-b",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setPeerMode(cmd, args[0], cortex.PeerModePaused)
		},
	}
}

func federationResumePeerCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "resume-peer <name>",
		Short:   "Resume syncing with a paused federation peer",
		Long:    "Clears a peer's mode back to \"sync\" in cortex.md. The syncer will resume\npulling from this peer on the next poll.",
		Example: "  noema federation resume-peer peer-b",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setPeerMode(cmd, args[0], cortex.PeerModeSync)
		},
	}
}

// setPeerMode updates a single peer's mode in cortex.md.
func setPeerMode(cmd *cobra.Command, name string, mode string) error {
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
		return fmt.Errorf("no federation peers configured in cortex.md")
	}

	found := false
	for i := range m.Federation.Peers {
		if m.Federation.Peers[i].Name == name {
			found = true
			prev := m.Federation.Peers[i].EffectiveMode()
			if prev == mode {
				fmt.Fprintf(cmd.OutOrStdout(), "Peer %q is already %s — no change.\n", name, mode)
				return nil
			}
			if mode == cortex.PeerModeSync {
				m.Federation.Peers[i].Mode = "" // omitempty: sync is the default
			} else {
				m.Federation.Peers[i].Mode = mode
			}
			if err := cortex.WriteManifest(cx.Dir, m); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Peer %q: %s -> %s\n", name, prev, mode)
			fmt.Fprintln(cmd.OutOrStdout(), "Restart `noema serve` for the change to take effect.")
			return nil
		}
	}

	if !found {
		known := make([]string, 0, len(m.Federation.Peers))
		for _, p := range m.Federation.Peers {
			known = append(known, p.Name)
		}
		return fmt.Errorf("peer %q is not configured in cortex.md (known: %s)", name, strings.Join(known, ", "))
	}
	return nil
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

// renderPeerVersion prints the peer's last-observed binary version,
// and a short "⚠ differs from local" annotation when the major/minor
// segments of the two versions don't line up. Exact-match checks
// would flag every commit-tagged dev build as a warning, which is
// noise; comparing the leading `vX.Y` is accurate enough to catch
// skew that actually produces schema-widening bugs.
func renderPeerVersion(w io.Writer, h federation.PeerHealth, localVersion string) {
	if h.Version == "" {
		fmt.Fprintf(w, "    version:    (not yet observed)\n")
		return
	}
	out := h.Version
	if !versionSeriesMatches(h.Version, localVersion) {
		out += fmt.Sprintf("    ⚠ differs from local %s", localVersion)
	}
	fmt.Fprintf(w, "    version:    %s\n", out)
}

// renderPeerHealth prints a multi-line "health:" block whenever the
// peer is in a degraded state. Silent when everything is green so
// healthy peers stay compact in the listing.
func renderPeerHealth(w io.Writer, h federation.PeerHealth) {
	if h.LastError == nil && h.ConsecutiveFailures == 0 {
		return
	}
	fmt.Fprintf(w, "    health:     ⚠ %d consecutive failures since last success\n", h.ConsecutiveFailures)
	if h.LastError != nil {
		fmt.Fprintf(w, "                reason: %s\n", h.LastError.Reason)
		if h.LastError.EventID != "" {
			fmt.Fprintf(w, "                event:  %s\n", h.LastError.EventID)
		}
		if h.LastError.TraceID != "" {
			fmt.Fprintf(w, "                trace:  %s\n", h.LastError.TraceID)
		}
		if hint := healthHint(h.LastError.Reason); hint != "" {
			fmt.Fprintf(w, "                %s\n", hint)
		}
	}
}

// healthHint turns a machine-readable reason into a one-line human
// hint pointing at the likely cause. Lives here rather than on
// PeerError itself because the mapping is a UI concern — the on-disk
// enum stays stable while display text can evolve.
func healthHint(reason string) string {
	switch reason {
	case federation.ReasonInvalidTraceID,
		federation.ReasonUnknownAction,
		federation.ReasonUnknownType:
		return "likely cause: peer binary predates schema changes elsewhere on the ring; upgrade the peer"
	case federation.ReasonInvalidFrontmatter:
		return "likely cause: peer received an event whose trace shape it doesn't recognise"
	case federation.ReasonNetworkRefused:
		return "likely cause: nothing listening on the peer's endpoint; is `noema serve` running?"
	case federation.ReasonNetworkTimeout:
		return "likely cause: peer unreachable on the network"
	case federation.ReasonNetworkDNS:
		return "likely cause: hostname resolution failed"
	case federation.ReasonNetworkTLS:
		return "likely cause: TLS handshake failed — check cert validity and CA trust"
	case federation.ReasonAuth:
		return "likely cause: shared-key mismatch between local and peer"
	case federation.ReasonIdentityMismatch:
		return "likely cause: peer cortex id changed — `noema federation reset-peer` after verifying"
	case federation.ReasonIdentityMissing:
		return "likely cause: peer predates cortex-id federation handshake; upgrade the peer"
	}
	return ""
}

// versionSeriesMatches compares the released-version baseline of two
// Noema version strings. Returns true when both describe builds
// against the same released tag — i.e. they share a `vX.Y.Z` prefix,
// ignoring any `-N-gSHA[-dirty]` dev-build suffix that git describe
// appends.
//
// Returns true when either side is unparseable ("dev", a bare commit
// hash, ad-hoc build strings) so the warning doesn't fire on builds
// that don't carry a meaningful baseline. The goal is to flag peer-
// vs-local skew that operators can act on — a dev build is already
// a known unknown.
//
// Earlier implementation compared only `vX.Y`, which let patch-level
// drift slip through silently. A peer on v0.9.1 while the local
// binary is v0.9.2-based is exactly the scenario this diagnostic
// exists to surface, so the comparison now runs at the full patch
// level.
func versionSeriesMatches(a, b string) bool {
	ap, aok := semverBaseline(a)
	bp, bok := semverBaseline(b)
	if !aok || !bok {
		return true
	}
	return ap == bp
}

// semverBaseline returns the `X.Y.Z` (or `X.Y`) released-version
// prefix of a version string. Everything past the first `-` is
// stripped because `git describe --tags` formats dev builds as
// `vX.Y.Z-N-gSHA[-dirty]` and that suffix identifies the dev commit,
// not the released baseline the build descends from.
//
// Returns ok=false for strings that don't lead with at least two
// dot-separated all-numeric segments (so "dev", "v", "1", and
// arbitrary branch names don't pass as parseable versions).
func semverBaseline(v string) (string, bool) {
	s := strings.TrimPrefix(v, "v")
	if s == "" {
		return "", false
	}
	if dash := strings.IndexByte(s, '-'); dash >= 0 {
		s = s[:dash]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return "", false
	}
	for _, p := range parts {
		if p == "" {
			return "", false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return "", false
			}
		}
	}
	return s, true
}

// formatFederationRank renders a federation.RankEntry for the CLI
// `noema federation status` output. Mirrors the formatRank helper in
// internal/mcp/server.go — the two surfaces are parallel views of the
// same data, so they render it the same way.
func formatFederationRank(r federation.RankEntry) string {
	if r.Rank == 0 || r.ObservedAt == "" {
		return "(ineligible)"
	}
	return fmt.Sprintf("%d (observed %s)", r.Rank, r.ObservedAt)
}
