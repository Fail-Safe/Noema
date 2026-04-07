package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/config"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/db"
	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/federation"
)

func migrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Run an explicit, named migration on a Cortex",
		Long: `Migrate runs explicit one-shot upgrade procedures that the regular schema
migrator (which only adds tables/columns) cannot perform automatically.

Each subcommand corresponds to a single migration, named after what it does.`,
	}
	cmd.AddCommand(migrateCortexIDCmd())
	return cmd
}

func migrateCortexIDCmd() *cobra.Command {
	var reset bool
	var assumeYes bool

	cmd := &cobra.Command{
		Use:   "cortex-id",
		Short: "Upgrade a Cortex to manifest version 2 (stable ULID identity)",
		Long: `Assigns a stable ULID to a Cortex and re-keys its event log and vector
clock from cortex-name to cortex-id. Required for any cortex written by an
older binary before federation peers can talk to it.

What it does:
  1. Reads cortex.md and aborts if the cortex is already at manifest version 2.
  2. Backs up cortex.md and db/noema.db with a timestamped suffix.
  3. Generates a fresh ULID and writes it to cortex.md as 'id', bumping the
     manifest version to 2.
  4. Backfills the cortex_id column on every traces row whose origin matches
     this cortex's name (i.e. locally-authored traces). Remote-origin rows are
     left alone — replays from peers will fill them in over time.
  5. Backfills the cortex_id column on every events row written under this
     cortex's name.
  6. Re-keys the local vector clock so the bucket previously stored under the
     cortex name moves to the new ULID.
  7. Updates the global config (~/.config/noema/config.yaml) so the cortex
     entry records the new ID.

Use --reset (with caution) when this directory is a copy of another cortex —
it assigns a fresh ULID and re-keys local rows under the new ID, accepting
that the original peer's federation history is no longer ours to claim. As
part of that clean break, --reset also clears every peer sync cursor and
last_seen timestamp so the post-migration syncer re-pulls each peer's log
from the beginning; without that, a cursor carried over from the old
identity could silently skip events that were written during the period
of broken federation that motivated the reset.

This migration is intentionally interactive — pass --yes to skip the prompt.
See docs/design/cortex-uuid-plan.md for the full design rationale.`,
		Example: "  noema migrate cortex-id\n  noema migrate cortex-id --cortex research --reset",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			name := cortexFlag
			if name == "" {
				name = os.Getenv("NOEMA_CORTEX")
			}
			if name == "" {
				name = cfg.Default
			}
			if name == "" {
				return fmt.Errorf("no cortex specified: use --cortex, set NOEMA_CORTEX, or run `noema use <name>`")
			}
			entry, ok := cfg.Cortexes[name]
			if !ok {
				return fmt.Errorf("unknown cortex %q", name)
			}

			return runCortexIDMigration(cmd.OutOrStdout(), cmd.InOrStdin(), cfg, name, entry, reset, assumeYes)
		},
	}
	cmd.Flags().BoolVar(&reset, "reset", false, "treat this directory as a copy of another cortex: assign a fresh ID and re-key local rows under it")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

func runCortexIDMigration(out io.Writer, in io.Reader, cfg *config.Config, name string, entry config.CortexEntry, reset, assumeYes bool) error {
	dir := entry.Path
	manifestPath := filepath.Join(dir, "cortex.md")

	m, err := cortex.ReadManifest(dir)
	if err != nil {
		return fmt.Errorf("reading cortex.md: %w", err)
	}

	// Idempotency: skip if already at v2 with an ID, unless --reset is set.
	if !reset && m.Version >= cortex.ManifestVersion && m.ID != "" {
		fmt.Fprintf(out, "Cortex %q is already at manifest version %d (id=%s); nothing to do.\n", name, m.Version, m.ID)
		return nil
	}

	// Compute the new ID. --reset always assigns a fresh one. Otherwise reuse
	// any partially-written ID from a previous attempt to keep retry idempotent.
	var newID string
	switch {
	case reset:
		newID = event.NewULID()
	case m.ID != "":
		newID = m.ID
	default:
		newID = event.NewULID()
	}

	// Confirmation prompt.
	fmt.Fprintf(out, "About to migrate cortex %q at %s\n", name, dir)
	fmt.Fprintf(out, "  current version: %d\n", m.Version)
	fmt.Fprintf(out, "  current id:      %s\n", emptyDash(m.ID))
	fmt.Fprintf(out, "  new version:     %d\n", cortex.ManifestVersion)
	fmt.Fprintf(out, "  new id:          %s\n", newID)
	if reset {
		fmt.Fprintln(out, "  mode:            --reset (re-key local rows under new id)")
	}
	if !assumeYes {
		fmt.Fprint(out, "Proceed? [y/N]: ")
		var resp string
		_, _ = fmt.Fscanln(in, &resp)
		if resp != "y" && resp != "Y" && resp != "yes" {
			return fmt.Errorf("aborted by user")
		}
	}

	// Backups.
	stamp := time.Now().UTC().Format("20060102T150405Z")
	if err := backupFile(manifestPath, manifestPath+"."+stamp+".bak"); err != nil {
		return fmt.Errorf("backing up cortex.md: %w", err)
	}
	dbPath := filepath.Join(dir, "db", "noema.db")
	if _, err := os.Stat(dbPath); err == nil {
		if err := backupFile(dbPath, dbPath+"."+stamp+".bak"); err != nil {
			return fmt.Errorf("backing up noema.db: %w", err)
		}
	}

	// Open the DB directly. db.Open() runs schema migrations (additive only)
	// so the cortex_id columns are guaranteed to exist before backfill.
	conn, err := db.Open(dir)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer conn.Close()

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	// Backfill traces.cortex_id. Locally-authored traces are those whose
	// origin matches this cortex's display name OR whose origin is empty
	// (older traces predate the origin field).
	tracesRes, err := tx.Exec(
		`UPDATE traces SET cortex_id = ? WHERE cortex_id = '' AND (origin = ? OR origin = '')`,
		newID, name,
	)
	if err != nil {
		return fmt.Errorf("backfilling traces.cortex_id: %w", err)
	}
	tracesUpdated, _ := tracesRes.RowsAffected()

	// Backfill events.cortex_id. Same heuristic — events keyed on the local
	// name become rows under the new ULID.
	eventsRes, err := tx.Exec(
		`UPDATE events SET cortex_id = ? WHERE cortex_id = '' AND origin = ?`,
		newID, name,
	)
	if err != nil {
		return fmt.Errorf("backfilling events.cortex_id: %w", err)
	}
	eventsUpdated, _ := eventsRes.RowsAffected()

	// In --reset mode, also re-key any rows that were inherited from the
	// original (copied) cortex's old ULID, so the new instance has a clean
	// identity for everything it now claims as its own.
	if reset {
		extraTracesRes, err := tx.Exec(
			`UPDATE traces SET cortex_id = ? WHERE origin = ? AND cortex_id != ?`,
			newID, name, newID,
		)
		if err != nil {
			return fmt.Errorf("re-keying traces under new id: %w", err)
		}
		extra, _ := extraTracesRes.RowsAffected()
		tracesUpdated += extra

		extraEventsRes, err := tx.Exec(
			`UPDATE events SET cortex_id = ? WHERE origin = ? AND cortex_id != ?`,
			newID, name, newID,
		)
		if err != nil {
			return fmt.Errorf("re-keying events under new id: %w", err)
		}
		extra, _ = extraEventsRes.RowsAffected()
		eventsUpdated += extra
	}

	// Collect peer names from cortex.md so the vclock cleanup can drop
	// any stale name-keyed peer buckets at the same time as the local
	// re-key. Without this step the local bucket migrates to a ULID but
	// the peer buckets stay name-keyed forever, and once those peers
	// also migrate they start emitting events under their new ULIDs.
	// The result is two parallel buckets per peer that never advance
	// together — federation.Compare can't relate them and starts
	// flagging legitimate updates as concurrent edits.
	var peerNames []string
	if m.Federation != nil {
		for _, p := range m.Federation.Peers {
			peerNames = append(peerNames, p.Name)
		}
	}

	// Re-key the vector clock: move whatever counter was stored under the
	// old name (or, in --reset mode, any non-ULID key) to the new ULID,
	// and drop stale name-keyed peer buckets.
	clockMoved, peersCleared, err := rekeyVectorClock(tx, name, newID, peerNames, reset)
	if err != nil {
		return fmt.Errorf("re-keying vector clock: %w", err)
	}

	// Drop pinned peer cortex_ids so peers re-pin on next handshake.
	// After this cortex's ID changes, peers' previous handshakes become
	// stale relative to whatever ID their counterpart now advertises;
	// clearing the pins lets the next sync re-establish identity cleanly
	// instead of refusing on a "peer identity mismatch" error.
	pinsCleared, err := clearPeerCortexIDPins(tx)
	if err != nil {
		return fmt.Errorf("clearing peer cortex id pins: %w", err)
	}

	// Under --reset (and only under --reset), also drop peer sync cursors
	// and last_seen rows. A reset declares the local cortex's causal
	// history disconnected from everything it had previously pulled, so
	// the cursors — which tell us "we already saw events up to X from
	// peer P" — are noise. Keeping them would cause the post-migration
	// syncer to ask each peer for "events since <stale cursor>" and
	// quietly skip everything written to the peer's log before that
	// point, which is exactly the incident that motivated this flag.
	// The normal (non-reset) migration leaves cursors alone because
	// ULIDs are stable across re-keying, so an existing cursor still
	// points at a causally meaningful event in the peer's log.
	var cursorsCleared int
	if reset {
		cursorsCleared, err = clearPeerSyncCursors(tx)
		if err != nil {
			return fmt.Errorf("clearing peer sync cursors: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing migration: %w", err)
	}

	// Write the new manifest.
	m.ID = newID
	m.Version = cortex.ManifestVersion
	if err := cortex.WriteManifest(dir, m); err != nil {
		return fmt.Errorf("writing cortex.md: %w", err)
	}

	// Update the global config so the cortex entry carries the new ID.
	entry.ID = newID
	cfg.Cortexes[name] = entry
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	fmt.Fprintf(out, "\nMigration complete.\n")
	fmt.Fprintf(out, "  cortex.md updated: id=%s version=%d\n", newID, cortex.ManifestVersion)
	fmt.Fprintf(out, "  traces backfilled: %d\n", tracesUpdated)
	fmt.Fprintf(out, "  events backfilled: %d\n", eventsUpdated)
	fmt.Fprintf(out, "  vector-clock buckets moved: %d\n", clockMoved)
	if peersCleared > 0 {
		fmt.Fprintf(out, "  stale peer buckets cleared: %d\n", peersCleared)
	}
	if pinsCleared > 0 {
		fmt.Fprintf(out, "  peer cortex_id pins cleared: %d (peers will re-pin on next handshake)\n", pinsCleared)
	}
	if cursorsCleared > 0 {
		fmt.Fprintf(out, "  peer sync cursors cleared: %d (peers will re-pull from the start of each log)\n", cursorsCleared)
	}
	fmt.Fprintf(out, "  backups: cortex.md.%s.bak, db/noema.db.%s.bak\n", stamp, stamp)
	return nil
}

// clearPeerCortexIDPins removes every federation_state row whose key is a
// peer cortex_id pin. After a cortex_id migration, the local cortex's
// identity changes, and peers will fail their next identity handshake
// against the now-stale pin unless we clear it. Returns the number of
// pins removed.
func clearPeerCortexIDPins(tx *sql.Tx) (int, error) {
	res, err := tx.Exec(
		`DELETE FROM federation_state WHERE key LIKE 'peer:%:cortex_id'`,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// clearPeerSyncCursors removes every federation_state row keyed on a peer
// cursor or last_seen timestamp. Called only under --reset, because a
// reset is the operator's declaration that the local cortex's causal
// history is disconnected from everything it had pulled before — cursors
// that were causally meaningful under the old identity become noise under
// the new one. The normal migration path leaves these rows alone since
// ULIDs are stable across re-keying, so existing cursors still point at
// causally meaningful events. Returns the number of rows removed (cursor
// + last_seen combined) so the migration summary can report it.
func clearPeerSyncCursors(tx *sql.Tx) (int, error) {
	res, err := tx.Exec(
		`DELETE FROM federation_state
		 WHERE key LIKE 'peer:%:last_event'
		    OR key LIKE 'peer:%:last_seen'`,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// rekeyVectorClock loads the vclock from federation_state, moves any counter
// stored under the old display name (and, in --reset mode, any non-ULID key)
// to the new ID, drops stale name-keyed buckets that match a known peer
// name from cortex.md, and writes it back. Returns (movedToNewID,
// peersCleared) so the caller can report both numbers.
//
// We drop peer name buckets rather than carrying them forward because
// once a peer also migrates, it will start emitting events under its new
// ULID — the old name-keyed bucket would never advance again, and
// federation.Compare would treat it as "this peer is missing causally
// later events" forever, producing false divergence detection.
func rekeyVectorClock(tx *sql.Tx, oldName, newID string, peerNames []string, reset bool) (moved int, peersCleared int, err error) {
	var raw string
	err = tx.QueryRow(`SELECT value FROM federation_state WHERE key = 'vclock'`).Scan(&raw)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}

	vc := make(federation.VClock)
	if err := json.Unmarshal([]byte(raw), &vc); err != nil {
		return 0, 0, fmt.Errorf("parsing existing vclock: %w", err)
	}

	if v, ok := vc[oldName]; ok {
		if vc[newID] < v {
			vc[newID] = v
		}
		delete(vc, oldName)
		moved++
	}

	// Drop any bucket whose key matches a configured peer name. These are
	// always pre-migration name-keys: a ULID is 26 chars and a peer name
	// in cortex.md is a human-friendly label.
	peerSet := make(map[string]struct{}, len(peerNames))
	for _, p := range peerNames {
		peerSet[p] = struct{}{}
	}
	for k := range vc {
		if k == newID {
			continue
		}
		if _, isPeer := peerSet[k]; isPeer {
			delete(vc, k)
			peersCleared++
		}
	}

	// In reset mode, also move any non-ULID-shaped key into the new ULID.
	// (ULIDs are exactly 26 chars; anything shorter or longer was a
	// pre-migration name-key.) Non-reset leaves stranger keys alone to avoid
	// surprising the operator.
	if reset {
		for k, v := range vc {
			if k == newID || len(k) == 26 {
				continue
			}
			if vc[newID] < v {
				vc[newID] = v
			}
			delete(vc, k)
			moved++
		}
	}

	data, err := json.Marshal(vc)
	if err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(
		`INSERT INTO federation_state (key, value) VALUES ('vclock', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		string(data),
	); err != nil {
		return 0, 0, err
	}
	return moved, peersCleared, nil
}

func backupFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func emptyDash(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
