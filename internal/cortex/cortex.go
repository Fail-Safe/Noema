package cortex

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Fail-Safe/Noema/internal/config"
	"github.com/Fail-Safe/Noema/internal/db"
	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/federation"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// ManifestVersion is the current cortex.md schema version. Cortexes written
// at this version carry an `id` field; cortexes at any earlier version must
// be migrated via `noema migrate cortex-id` before federation will accept them.
const ManifestVersion = 2

// Manifest is the cortex.md file at the root of each Cortex.
type Manifest struct {
	ID            string               `yaml:"id,omitempty"`
	Name          string               `yaml:"name"`
	Purpose       string               `yaml:"purpose,omitempty"`
	Owner         string               `yaml:"owner,omitempty"`
	Created       string               `yaml:"created"`
	Version       int                  `yaml:"version"`
	Access        *AccessConfig        `yaml:"access,omitempty"`
	Federation    *FederationConfig    `yaml:"federation,omitempty"`
	Watch         *WatchConfig         `yaml:"watch,omitempty"`
	Consolidation *ConsolidationConfig `yaml:"consolidation,omitempty"`
}

// AccessConfig holds MCP endpoint authentication settings for cortex.md.
// When SharedKeyFile is set, the HTTP MCP endpoint runs in shared-key mode
// and every incoming request must carry a matching Authorization bearer
// header. See docs/design/mcp-auth-plan.md for the full design.
type AccessConfig struct {
	// SharedKeyFile is a path to a sidecar file whose first non-empty
	// line is the shared bearer token. Relative paths are resolved
	// against the cortex directory. The manifest itself never holds the
	// secret — only a pointer to where it lives.
	SharedKeyFile string `yaml:"shared_key_file,omitempty"`
}

// Federation mode constants.
const (
	FederationModeSync      = "sync"      // bidirectional: pull from peers + serve events
	FederationModePublish   = "publish"   // outbound only: serve events, never pull
	FederationModeSubscribe = "subscribe" // inbound only: pull from peers, refuse to serve
)

// Peer mode constants.
const (
	PeerModeSync   = "sync"   // actively pull from this peer
	PeerModePaused = "paused" // configured but skipped by the syncer
)

// FederationConfig holds peer declarations for cortex.md.
type FederationConfig struct {
	Mode     string      `yaml:"mode,omitempty"`     // sync | publish | subscribe
	Peers    []PeerEntry `yaml:"peers,omitempty"`
	Interval string      `yaml:"interval,omitempty"` // e.g. "30s", "1m"
}

// EffectiveMode returns the configured federation mode, defaulting to "sync".
func (fc *FederationConfig) EffectiveMode() string {
	if fc == nil || fc.Mode == "" {
		return FederationModeSync
	}
	return fc.Mode
}

// PeerEntry is a peer declared in cortex.md.
type PeerEntry struct {
	Name     string `yaml:"name"`
	Endpoint string `yaml:"endpoint"`
	CA       string `yaml:"ca,omitempty"` // path to CA cert for TLS verification
	Mode     string `yaml:"mode,omitempty"` // sync | paused
}

// EffectiveMode returns the configured peer mode, defaulting to "sync".
func (pe PeerEntry) EffectiveMode() string {
	if pe.Mode == "" {
		return PeerModeSync
	}
	return pe.Mode
}

// WatchConfig controls the filesystem watcher that turns external edits to
// trace files (Obsidian, VS Code, Finder, etc.) into first-class mutation
// events. When unset, the watcher is enabled by default during
// `noema serve --transport http`.
type WatchConfig struct {
	// Enabled is a pointer so an omitted field means "default on."
	// Explicitly setting `enabled: false` disables the watcher.
	Enabled *bool `yaml:"enabled,omitempty"`
	// DebounceMs is the per-path debounce window in milliseconds.
	// Zero means use the default (300ms).
	DebounceMs int `yaml:"debounce_ms,omitempty"`
	// AutoOnboard controls what happens when a new file is dropped into
	// traces/ without conforming frontmatter (no id, empty id, invalid
	// id format, or filename mismatch). Nil / unset defaults to true:
	// the watcher rescues the file by synthesizing valid frontmatter,
	// generating an ID from the title or filename, and renaming the
	// file to match. Makes Obsidian Web Clipper, Drafts, and shortcut
	// macros work without teaching users Noema's filename-ID rules.
	// Set to false to restore the prior skip-and-log behaviour.
	AutoOnboard *bool `yaml:"auto_onboard,omitempty"`
}

// WatchEnabled reports whether the watcher should run given the parsed
// manifest value. Nil manifest or nil Enabled pointer both mean "on."
func (wc *WatchConfig) WatchEnabled() bool {
	if wc == nil || wc.Enabled == nil {
		return true
	}
	return *wc.Enabled
}

// AutoOnboardEnabled reports whether the watcher should rescue non-
// conforming files dropped into traces/. Nil manifest or nil
// AutoOnboard pointer both mean "on" so users get the graceful
// behaviour by default.
func (wc *WatchConfig) AutoOnboardEnabled() bool {
	if wc == nil || wc.AutoOnboard == nil {
		return true
	}
	return *wc.AutoOnboard
}

// EffectiveDebounce returns the configured debounce duration, defaulting
// to 300ms when unset.
func (wc *WatchConfig) EffectiveDebounce() time.Duration {
	const defaultDebounce = 300 * time.Millisecond
	if wc == nil || wc.DebounceMs <= 0 {
		return defaultDebounce
	}
	return time.Duration(wc.DebounceMs) * time.Millisecond
}

// ConsolidationConfig controls the background memory-consolidation agent.
// All three triggers (cron, idle, threshold) are composable — set any
// combination, the agent fires on whichever triggers first. Leaving all
// three empty disables the agent even when Enabled is true.
//
// See docs/plans/consolidation-plan.md §4 in the Noema-design repo for
// the cadence design. Model-tier and local-LLM fields are deferred to
// later phases; this config block only covers scheduling for now.
type ConsolidationConfig struct {
	// Enabled is the master opt-in. The feature ships off by default so
	// existing cortexes are unaffected until users explicitly turn it on.
	Enabled bool `yaml:"enabled,omitempty"`

	// Cron is the nightly trigger time in "HH:MM" local-clock format
	// (e.g. "03:00"). Empty means no cron trigger.
	Cron string `yaml:"cron,omitempty"`

	// IdleMinutes fires a pass after N minutes of no trace mutations.
	// Zero disables the idle trigger. Cooldown equal to IdleMinutes is
	// enforced so a quiet cortex doesn't consolidate on every tick.
	IdleMinutes int `yaml:"idle_minutes,omitempty"`

	// ThresholdShort fires a pass when the short-term tier count
	// exceeds this many active traces. Zero disables the trigger.
	// Hysteresis: once tripped, re-arms only when the count drops
	// back below 0.8 * ThresholdShort so a cortex hovering near the
	// threshold doesn't thrash.
	ThresholdShort int `yaml:"threshold_short,omitempty"`

	// WindowHours bounds the candidate pool for a pass to traces
	// created within the last N hours. Zero defaults to 24.
	WindowHours int `yaml:"window_hours,omitempty"`
}

// EffectiveWindowHours returns the window duration with the 24h default
// applied when unset.
func (cc *ConsolidationConfig) EffectiveWindowHours() time.Duration {
	if cc == nil || cc.WindowHours <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(cc.WindowHours) * time.Hour
}

// ErrSourceLocked is returned when a mutation is attempted on a
// source-locked trace from a foreign origin.
var ErrSourceLocked = errors.New("trace is source-locked")

// MaxSearchQueryLen caps the length of FTS5 search queries to prevent
// denial of service via expensive wildcard or deeply nested expressions.
const MaxSearchQueryLen = 1000

// SanitizeFTS5Query quotes each whitespace-delimited token so that FTS5
// treats hyphens, colons, and other operator characters as literals.
// Tokens that are already quoted or use explicit FTS5 operators (AND, OR,
// NOT, prefix*) are passed through unchanged to preserve power-user syntax.
func SanitizeFTS5Query(q string) string {
	tokens := strings.Fields(q)
	if len(tokens) == 0 {
		return q
	}
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		switch {
		case t == "AND" || t == "OR" || t == "NOT":
			out = append(out, t)
		case strings.HasPrefix(t, "\""):
			out = append(out, t) // already quoted (may be part of multi-token phrase)
		case strings.HasSuffix(t, "*"):
			out = append(out, t) // prefix search
		case strings.ContainsAny(t, "-:"):
			out = append(out, "\""+t+"\"")
		default:
			out = append(out, t)
		}
	}
	result := strings.Join(out, " ")

	// Safety net: if the result has an odd number of quote marks, the
	// string has unbalanced quotes that would cause an FTS5 syntax
	// error (and leak the raw query in the error message). Strip all
	// quotes to fall back to default FTS5 tokenization. Legitimate
	// quoted phrases always have matched pairs, so this only fires on
	// malformed input.
	if strings.Count(result, "\"")%2 != 0 {
		result = strings.ReplaceAll(result, "\"", "")
	}
	return result
}

type Cortex struct {
	ID              string // ULID, stable across renames; the federation identity key
	Name            string // human-readable display label
	Dir             string
	DB              *db.DB
	forceSourceLock bool // when true, checkSourceLock is a no-op
}

// SetForceSourceLock enables or disables the source-lock override. When
// enabled, mutations on source-locked traces succeed with a warning instead
// of being refused. Intended for CLI --force flags only.
func (c *Cortex) SetForceSourceLock(v bool) { c.forceSourceLock = v }

// CheckSourceLock returns ErrSourceLocked if the trace is source-locked
// by a foreign origin. The check is skipped when forceSourceLock is set.
func (c *Cortex) CheckSourceLock(id string) error {
	if c.forceSourceLock {
		return nil
	}
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if r.SourceLocked && r.Origin != c.Name {
		return fmt.Errorf("%w by origin %q", ErrSourceLocked, r.Origin)
	}
	return nil
}

// Create initialises a new Cortex on disk and registers it.
// dir is the parent directory; the cortex is created as dir/<name>/. Returns
// the freshly written manifest so callers can surface the new cortex's ULID
// to the user (see `noema init`).
func Create(name, dir string) (Manifest, error) {
	root := filepath.Join(dir, name)
	for _, sub := range []string{"traces", "archive/traces", "trash/traces", "db"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o750); err != nil {
			return Manifest{}, fmt.Errorf("creating %s: %w", sub, err)
		}
	}

	manifest := Manifest{
		ID:      event.NewULID(),
		Name:    name,
		Created: time.Now().UTC().Format("2006-01-02"),
		Version: ManifestVersion,
	}
	data, err := yaml.Marshal(manifest)
	if err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(root, "cortex.md"), data, 0o640); err != nil {
		return Manifest{}, fmt.Errorf("writing cortex.md: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(agentsMDContent(manifest)), 0o640); err != nil {
		return Manifest{}, fmt.Errorf("writing AGENTS.md: %w", err)
	}

	// Open (and migrate) the DB to initialise the schema.
	conn, err := db.Open(root)
	if err != nil {
		return Manifest{}, fmt.Errorf("initialising database: %w", err)
	}
	return manifest, conn.Close()
}

// ValidateFederation checks that the federation mode and per-peer modes in
// the manifest are recognized values. Returns nil when there is no
// federation block or when all values are valid.
func (m Manifest) ValidateFederation() error {
	if m.Federation == nil {
		return nil
	}
	switch m.Federation.EffectiveMode() {
	case FederationModeSync, FederationModePublish, FederationModeSubscribe:
	default:
		return fmt.Errorf("federation.mode %q is not valid; use sync, publish, or subscribe", m.Federation.Mode)
	}
	for _, p := range m.Federation.Peers {
		switch p.EffectiveMode() {
		case PeerModeSync, PeerModePaused:
		default:
			return fmt.Errorf("federation.peers[%s].mode %q is not valid; use sync or paused", p.Name, p.Mode)
		}
	}
	return nil
}

// ReadManifest parses the cortex.md manifest in the given cortex directory.
func ReadManifest(dir string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "cortex.md"))
	if err != nil {
		return Manifest{}, fmt.Errorf("reading cortex.md: %w", err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parsing cortex.md: %w", err)
	}
	if err := m.ValidateFederation(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// WriteManifest writes the manifest back to cortex.md in the given directory.
func WriteManifest(dir string, m Manifest) error {
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshaling cortex.md: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "cortex.md"), data, 0o640)
}

// PeerLabelCollidesWithSelf reports whether the proposed peer label is the
// same as this cortex's own name. This is a federation safety guardrail:
// even after the cortex-id migration (docs/design/cortex-uuid-plan.md), a
// label collision is confusing in display surfaces and should be rejected
// at config time.
func (m Manifest) PeerLabelCollidesWithSelf(peerLabel string) bool {
	return peerLabel != "" && peerLabel == m.Name
}

// lookupCortexIDForTrace returns the cortex_id of the most recent event
// for a given trace, or "" if none exists. Used by Sync to attribute
// remote-origin traces back to the cortex that created them when the
// frontmatter only carries the display name.
func (c *Cortex) lookupCortexIDForTrace(traceID string) string {
	var id string
	err := c.DB.QueryRow(
		`SELECT cortex_id FROM events WHERE trace_id = ? AND cortex_id != '' ORDER BY id DESC LIMIT 1`,
		traceID,
	).Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

// agentsMDContent returns the generated AGENTS.md content for the given
// manifest. AGENTS.md is the de facto cross-tool agent-instructions filename
// (see agents.md) and is picked up by Codex, Cursor, Aider, Zed, Copilot, and
// most other coding-agent tooling. Content is intentionally scoped to what an
// agent needs to use the Cortex — operator-only material (TLS, key rotation,
// service install) lives in the project README.
func agentsMDContent(m Manifest) string {
	purposeLine := ""
	if m.Purpose != "" {
		purposeLine = "- **Purpose:** " + m.Purpose + "\n"
	}
	ownerLine := ""
	if m.Owner != "" {
		ownerLine = "- **Owner:** " + m.Owner + "\n"
	}
	return `# Noema Cortex — Agent Guide

This file was generated by ` + "`noema init`" + `. It describes how to work with
this Cortex as an AI agent, with or without the ` + "`noema`" + ` binary or MCP server.

## This Cortex

- **Name:** ` + m.Name + `
` + purposeLine + ownerLine + `- **Created:** ` + m.Created + `

## Concepts

**Cortex** — a named collection of Traces. This directory is a Cortex.

**Trace** — a single memory unit: one markdown file with YAML frontmatter.
The markdown files are the **source of truth**. The SQLite database
(` + "`db/noema.db`" + `) is a derived index and can be rebuilt from the files at
any time with ` + "`noema sync`" + `.

## Directory Layout

` + "```" + `
` + m.Name + `/
  AGENTS.md             ← this file
  cortex.md             ← cortex manifest
  traces/               ← active traces
  archive/
    traces/             ← archived (still relevant, lower priority)
  trash/
    traces/             ← soft-deleted, auto-purged after 30 days
  db/
    noema.db            ← SQLite index (do not edit directly)
` + "```" + `

## Trace File Format

Every Trace is a markdown file with YAML frontmatter:

` + "```markdown" + `
---
id: 20260329-why-we-chose-go
title: Why we chose Go
type: decision
author: research-agent-1
tags: [go, language, architecture]
derived_from: [20260328-language-candidates]
origin: research-cortex
created: 2026-03-29T14:23:00Z
updated: 2026-03-29T14:23:00Z
---

Body content here. Plain markdown, any length.
` + "```" + `

### Frontmatter Fields

| Field | Required | Description |
|---|---|---|
| ` + "`id`" + ` | yes | Matches the filename: ` + "`YYYYMMDD-slugified-title`" + ` |
| ` + "`title`" + ` | yes | Short, descriptive title |
| ` + "`type`" + ` | yes | One of the types listed below |
| ` + "`created`" + ` | yes | RFC3339 UTC timestamp |
| ` + "`updated`" + ` | yes | RFC3339 UTC timestamp — update on every edit |
| ` + "`author`" + ` | no | Human username or agent name |
| ` + "`tags`" + ` | no | YAML list of strings |
| ` + "`derived_from`" + ` | no | YAML list of trace IDs this was derived from |
| ` + "`origin`" + ` | no | Cortex name that created this trace |

## Trace Types

Choose the type that best reflects the **intent** of the memory:

| Type | When to use |
|---|---|
| ` + "`fact`" + ` | A discrete thing that is true |
| ` + "`decision`" + ` | A choice made and why |
| ` + "`preference`" + ` | A behavioral or stylistic lean |
| ` + "`context`" + ` | Situational background |
| ` + "`skill`" + ` | A learned capability or procedure |
| ` + "`intent`" + ` | Something that needs to happen (autonomous pickup) |
| ` + "`observation`" + ` | Something witnessed but not yet verified |
| ` + "`note`" + ` | Anything else |

## Titles

- Keep titles short and descriptive — aim for under 80 characters.
- The ID slug is capped at 100 characters; longer titles are silently
  truncated before the date prefix is prepended.
- **Do not include the date in the title.** ` + "`noema`" + ` prepends today's date
  automatically, and leading ` + "`YYYYMMDD-`" + ` / ` + "`YYYY-MM-DD-`" + ` prefixes in the
  title are stripped to prevent doubled-date IDs.
- **Avoid embedded dates or timestamps mid-title** (e.g. "session 20260416
  142000"). Only leading dates are stripped, so mid-title date fragments
  survive and produce awkward IDs. If a trace is *about* a specific date,
  put it in a tag (` + "`tags: [event-2026-04-02]`" + `) or in the body.

## Creating a Trace (filesystem)

1. Pick a type and write your content.
2. Generate an ID: today's date + slugified title, e.g.
   ` + "`20260330-decision-to-use-sqlite`" + `. See **Titles** above for naming rules.
3. Write the file to ` + "`traces/<id>.md`" + ` with the frontmatter shown above.
4. Run ` + "`noema sync`" + ` (if available) to update the database index.

## Editing a Trace (filesystem)

Edit the markdown file directly. Always update the ` + "`updated`" + ` timestamp.
Run ` + "`noema sync`" + ` to re-index.

## Archiving / Restoring (filesystem)

- Archive: move the file from ` + "`traces/`" + ` to ` + "`archive/traces/`" + `.
- Restore: move it back to ` + "`traces/`" + `.
- Run ` + "`noema sync`" + ` after either operation.

## Database Sync

If you create, edit, or move trace files directly on the filesystem,
the SQLite index will be out of date until you run:

` + "```" + `
noema sync
` + "```" + `

Sync walks all three trace directories, upserts every file it finds into the
database, and reports how many entries were added, updated, or are orphaned
(in the database but missing on disk).

## External Edits Are First-Class (under any ` + "`noema serve`" + `)

Whenever a ` + "`noema serve`" + ` process is running on this cortex — under
` + "`--transport stdio`" + ` OR ` + "`--transport http`" + ` — a background watcher
observes ` + "`traces/`" + `, ` + "`archive/traces/`" + `, and ` + "`trash/traces/`" + `.
External changes (Obsidian, VS Code, Finder drags, ` + "`rm`" + `, iCloud sync
from another device, a second noema process on the same cortex) are
ingested as real mutation events — identical to what the MCP tools emit.
You get:

- edit an .md file → ` + "`update`" + ` event
- drop a new valid .md → ` + "`create`" + ` event
- move between ` + "`traces/`" + `, ` + "`archive/traces/`" + `, ` + "`trash/traces/`" + ` → the matching archive/trash/recover event
- delete a file outright → ` + "`trash`" + ` event; the body is restored to ` + "`trash/traces/`" + ` from the event log so ` + "`noema recover`" + ` still works

Source-locked foreign traces are skipped (the watcher logs a warning
rather than poisoning federation). Malformed frontmatter is skipped.
The watcher is on by default; opt out with ` + "`watch: { enabled: false }`" + `
in ` + "`cortex.md`" + `. Federation propagation still only happens under
` + "`--transport http`" + ` (peers need a network endpoint), but external-edit
events land in the local log during stdio sessions and flow outward the
next time an HTTP serve runs. When no ` + "`noema serve`" + ` is running at
all, external edits stay invisible until you run ` + "`noema sync`" + ` — but
` + "`sync`" + ` emits no events, so federation peers won't see them.

## MCP Tools (if available)

If a ` + "`noema`" + ` MCP server is connected, prefer the tools over direct file access —
they keep the database in sync automatically:

| Tool | Purpose |
|---|---|
| ` + "`list_traces`" + ` | List traces with optional type/author/tag/origin filters |
| ` + "`get_trace`" + ` | Fetch full content of a trace by ID |
| ` + "`create_trace`" + ` | Create a new trace (supports derived_from, origin) |
| ` + "`update_trace`" + ` | Update fields of an existing trace |
| ` + "`append_trace`" + ` | Append content to a trace body (fire-and-forget) |
| ` + "`search_traces`" + ` | Full-text search across titles and bodies |
| ` + "`archive_trace`" + ` | Archive a trace |
| ` + "`unarchive_trace`" + ` | Restore an archived trace |
| ` + "`delete_trace`" + ` | Move a trace to trash (soft-delete, recoverable) |
| ` + "`recover_trace`" + ` | Restore a trashed trace to active |
| ` + "`trace_history`" + ` | Show the event log (audit trail) for a trace |
| ` + "`trace_lineage`" + ` | Show what a trace was derived from and what derives from it |
| ` + "`cortex_identity`" + ` | Return this cortex's stable ULID + name + manifest version (federation handshake) |
| ` + "`sync_events`" + ` | Pull events for federation sync (used by remote peers) |
| ` + "`federation_status`" + ` | Show federation config, peer states, and vector clock |
| ` + "`announce_peer`" + ` | Accept a peer announcement for mutual discovery |
| ` + "`resolve_divergence`" + ` | Resolve a concurrent edit conflict (divergence) |

## Conflict Resolution

When two federated cortexes update the same trace concurrently, neither edit
overwrites the other. Instead, a **divergence trace** is auto-created with
` + "`type=divergence`" + ` and ` + "`tags=[divergence, needs-resolution]`" + `. Its body lists every
conflicting version under ` + "`### Version from <name> (<id-prefix>)`" + ` headers and a
top-line ` + "`**Conflicting origins:** <labels>`" + ` summary. The body is rendered
deterministically (sorted by cortex id, not name) so every replica sees identical content.

To resolve, run one of:

` + "```" + `
noema resolve <divergence-id> --accept <name|id-prefix>   # apply that peer's version
noema resolve <divergence-id> --custom "<merged body>"    # apply a custom merge
` + "```" + `

Either choice updates the original trace, federates the resolution, and trashes
the divergence trace. The MCP equivalent is the ` + "`resolve_divergence`" + ` tool.
`
}

// Open opens an existing Cortex by directory path.
// It ensures required subdirectories exist and auto-purges expired trash.
func Open(name, dir string) (*Cortex, error) {
	for _, sub := range []string{"traces", "archive/traces", "trash/traces"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, fmt.Errorf("ensuring %s: %w", sub, err)
		}
	}
	conn, err := db.Open(dir)
	if err != nil {
		return nil, err
	}
	cx := &Cortex{Name: name, Dir: dir, DB: conn}

	// Load the manifest to pick up the cortex ID and enforce versioning.
	m, mErr := ReadManifest(dir)
	if mErr == nil {
		if m.Version > 0 && m.Version < ManifestVersion {
			conn.Close()
			return nil, fmt.Errorf(
				"cortex %q is at manifest version %d but this binary requires version %d.\n"+
					"Run `noema migrate cortex-id --cortex %s` to upgrade.\n"+
					"See docs/design/cortex-uuid-plan.md for what this changes.",
				name, m.Version, ManifestVersion, name,
			)
		}
		cx.ID = m.ID

		// Copied-directory detection (Gotcha #3 in the design doc).
		// If the manifest claims an ID but the events table has rows under
		// a *different* cortex_id (and none under this one), the directory
		// was copied from another machine and would silently corrupt vector
		// clocks if allowed to run. Refuse with a clear escape hatch.
		if cx.ID != "" {
			if err := cx.detectCopiedDirectory(); err != nil {
				conn.Close()
				return nil, err
			}
		}
	}

	// Auto-purge expired trash. Best-effort: errors are silently ignored.
	cfg, _ := config.Load()
	days := 30
	if cfg != nil && cfg.TrashDays > 0 {
		days = cfg.TrashDays
	}
	_ = cx.Purge(days)

	// Refresh the per-cortex agent-instructions file on every Open so
	// existing cortexes pick up template changes (new sections, renamed
	// commands, etc.) without the operator having to delete the file
	// first. The file's header documents it as auto-generated.
	//
	// The write is drift-aware: we only overwrite when the on-disk
	// content diverges from the current template. That keeps the mtime
	// stable under iCloud/Dropbox/Syncthing so a noop open doesn't cause
	// a sync ripple.
	//
	// Any legacy AGENT.md (singular, pre-#27 naming) is removed
	// unconditionally — regardless of whether AGENTS.md also exists —
	// so mixed states from partial upgrades clean themselves up.
	_ = os.Remove(filepath.Join(dir, "AGENT.md"))
	if mErr == nil {
		agentsMDPath := filepath.Join(dir, "AGENTS.md")
		want := agentsMDContent(m)
		existing, _ := os.ReadFile(agentsMDPath)
		if string(existing) != want {
			_ = os.WriteFile(agentsMDPath, []byte(want), 0o640)
		}
	}

	// Auto-rebuild the FTS5 index when it's smaller than the traces
	// table. This is the recovery path after migration 008, which
	// dropped and recreated traces_fts with a new tags column but
	// couldn't repopulate body (body lives in the markdown files, not
	// SQL). The first serve after upgrade does the full filesystem
	// walk; subsequent opens are no-ops because counts match.
	// Best-effort: a rebuild failure logs but doesn't block Open —
	// searches will return fewer results until the next successful
	// rebuild, but the cortex still opens for normal writes.
	if err := cx.RebuildFTSIfStale(); err != nil {
		fmt.Fprintf(os.Stderr, "[cortex] FTS5 reindex warning: %v\n", err)
	}

	return cx, nil
}

// detectCopiedDirectory refuses to start if the events table contains rows
// from a different cortex ID than the one declared in cortex.md, and zero
// rows under the declared ID. That signature only happens when a Cortex
// directory has been copied wholesale from another machine — the new
// "instance" inherits the original's ULID but has none of its own writes.
// Allowing this would create two physical Cortexes claiming the same
// identity in any federation they joined.
func (c *Cortex) detectCopiedDirectory() error {
	var distinctIDs, ownRows int
	if err := c.DB.QueryRow(
		`SELECT COUNT(DISTINCT cortex_id) FROM events WHERE cortex_id != ''`,
	).Scan(&distinctIDs); err != nil {
		return nil // table missing or empty — fresh cortex, fine
	}
	if distinctIDs == 0 {
		return nil // never written; fresh or pre-migration
	}
	if err := c.DB.QueryRow(
		`SELECT COUNT(*) FROM events WHERE cortex_id = ?`, c.ID,
	).Scan(&ownRows); err != nil {
		return nil
	}
	if ownRows > 0 {
		return nil // we have our own writes — legitimate
	}
	return fmt.Errorf(
		"cortex %q at %s appears to be a copy of another Cortex.\n"+
			"Its cortex.md declares id=%s but the event log contains no rows under that id\n"+
			"(it has %d distinct other id(s)). Two Cortexes cannot share an identity in a\n"+
			"federation — vector clocks would silently merge and concurrent edits would be\n"+
			"clobbered. To make this directory a distinct Cortex, run:\n"+
			"    noema migrate cortex-id --cortex %s --reset\n"+
			"That will assign a fresh id and re-key the local event log. If this is not a\n"+
			"copy and you expected the events to be present, restore from backup instead.",
		c.Name, c.Dir, c.ID, distinctIDs, c.Name,
	)
}

func (c *Cortex) Close() error {
	return c.DB.Close()
}

func (c *Cortex) TracesDir() string {
	return filepath.Join(c.Dir, "traces")
}

func (c *Cortex) ArchiveDir() string {
	return filepath.Join(c.Dir, "archive", "traces")
}

func (c *Cortex) TrashDir() string {
	return filepath.Join(c.Dir, "trash", "traces")
}

// TraceFile returns the absolute path to a trace's markdown file.
func (c *Cortex) TraceFile(id string, archived bool) string {
	if archived {
		return filepath.Join(c.ArchiveDir(), id+".md")
	}
	return filepath.Join(c.TracesDir(), id+".md")
}

// TrashFile returns the path for a trace in the trash.
func (c *Cortex) TrashFile(id string) string {
	return filepath.Join(c.TrashDir(), id+".md")
}

// filePath resolves the correct on-disk path for any row state.
func (c *Cortex) filePath(r *Row) string {
	if r.TrashedAt != "" {
		return c.TrashFile(r.ID)
	}
	return c.TraceFile(r.ID, r.ArchivedAt != "")
}

// Add writes a new Trace to disk and inserts it into the DB.
func (c *Cortex) Add(t *trace.Trace) error {
	if t.Origin == "" {
		t.Origin = c.Name
	}
	if t.Tier == "" {
		t.Tier = trace.TierShort
	}
	t.ContentHash = trace.ContentHash(t.Body)
	path := c.TraceFile(t.ID, false)
	if err := t.Write(path); err != nil {
		return fmt.Errorf("writing trace file: %w", err)
	}
	if err := c.insertDB(t); err != nil {
		os.Remove(path)
		return fmt.Errorf("inserting into database: %w", err)
	}
	return nil
}

func (c *Cortex) insertDB(t *trace.Trace) error {
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	tier := t.Tier
	if tier == "" {
		tier = trace.TierShort
	}
	_, err = tx.Exec(
		`INSERT INTO traces (id, title, type, tier, author, origin, cortex_id, created_at, updated_at, content_hash, source_locked, source_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Title, t.Type, tier, t.Author, t.Origin, c.ID, t.Created, t.Updated, t.ContentHash, boolToInt(t.SourceLocked), nullIfEmpty(t.SourceHash),
	)
	if err != nil {
		return err
	}
	for _, tag := range t.Tags {
		if _, err := tx.Exec(`INSERT INTO trace_tags (trace_id, tag) VALUES (?, ?)`, t.ID, tag); err != nil {
			return err
		}
	}
	for _, src := range t.DerivedFrom {
		if _, err := tx.Exec(`INSERT INTO trace_lineage (trace_id, derived_from) VALUES (?, ?)`, t.ID, src); err != nil {
			return err
		}
	}
	if err := insertFTS(tx, t.ID, t.Title, t.Body, t.Tags); err != nil {
		return err
	}
	if err := c.emitEvent(tx, event.ActionCreate, t.ID, t.Created, marshalTraceData(t)); err != nil {
		return err
	}
	return tx.Commit()
}

// Row is a DB row joined with tags, returned by list/search operations.
type Row struct {
	ID           string
	Title        string
	Type         string
	Tier         string
	Author       string
	Origin       string
	Tags         []string
	DerivedFrom  []string
	ArchivedAt   string
	TrashedAt    string
	CreatedAt    string
	UpdatedAt    string
	ContentHash  string
	SourceLocked bool
	SourceHash   string
}

type ListOptions struct {
	Type     string
	Author   string
	Tag      string
	Origin   string
	Archived bool // only archived (excludes trashed)
	Trashed  bool // only trashed
	All      bool // active + archived (excludes trashed)
}

func (c *Cortex) List(opts ListOptions) ([]Row, error) {
	q := `SELECT id, title, type, tier, author, origin, archived_at, trashed_at, created_at, updated_at, content_hash, source_locked, source_hash FROM traces WHERE 1=1`
	var args []any

	switch {
	case opts.Trashed:
		q += ` AND trashed_at IS NOT NULL`
	case opts.All:
		q += ` AND trashed_at IS NULL`
	case opts.Archived:
		q += ` AND archived_at IS NOT NULL AND trashed_at IS NULL`
	default:
		q += ` AND archived_at IS NULL AND trashed_at IS NULL`
	}
	if opts.Type != "" {
		q += ` AND type = ?`
		args = append(args, opts.Type)
	}
	if opts.Author != "" {
		q += ` AND author = ?`
		args = append(args, opts.Author)
	}
	if opts.Tag != "" {
		q += ` AND id IN (SELECT trace_id FROM trace_tags WHERE tag = ?)`
		args = append(args, opts.Tag)
	}
	if opts.Origin != "" {
		q += ` AND origin = ?`
		args = append(args, opts.Origin)
	}
	q += ` ORDER BY created_at DESC, rowid DESC`

	rows, err := c.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return c.scanRows(rows)
}

func (c *Cortex) Search(query string, opts ListOptions) ([]Row, error) {
	if len(query) > MaxSearchQueryLen {
		return nil, fmt.Errorf("search query too long (%d chars, max %d)", len(query), MaxSearchQueryLen)
	}
	ftsQuery := SanitizeFTS5Query(query)
	q := `
		SELECT t.id, t.title, t.type, t.tier, t.author, t.origin, t.archived_at, t.trashed_at, t.created_at, t.updated_at, t.content_hash, t.source_locked, t.source_hash
		FROM traces t
		WHERE t.id IN (SELECT id FROM traces_fts WHERE traces_fts MATCH ?)`
	args := []any{ftsQuery}

	switch {
	case opts.Trashed:
		q += ` AND t.trashed_at IS NOT NULL`
	case opts.All:
		q += ` AND t.trashed_at IS NULL`
	case opts.Archived:
		q += ` AND t.archived_at IS NOT NULL AND t.trashed_at IS NULL`
	default:
		q += ` AND t.archived_at IS NULL AND t.trashed_at IS NULL`
	}
	if opts.Type != "" {
		q += ` AND t.type = ?`
		args = append(args, opts.Type)
	}
	if opts.Author != "" {
		q += ` AND t.author = ?`
		args = append(args, opts.Author)
	}
	if opts.Tag != "" {
		q += ` AND t.id IN (SELECT trace_id FROM trace_tags WHERE tag = ?)`
		args = append(args, opts.Tag)
	}
	q += ` ORDER BY t.created_at DESC`

	rows, err := c.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return c.scanRows(rows)
}

func (c *Cortex) Get(id string) (*Row, error) {
	var r Row
	var archivedAt, trashedAt, contentHash, sourceHash *string
	var sourceLocked int
	err := c.DB.QueryRow(
		`SELECT id, title, type, tier, author, origin, archived_at, trashed_at, created_at, updated_at, content_hash, source_locked, source_hash FROM traces WHERE id = ?`, id,
	).Scan(&r.ID, &r.Title, &r.Type, &r.Tier, &r.Author, &r.Origin, &archivedAt, &trashedAt, &r.CreatedAt, &r.UpdatedAt, &contentHash, &sourceLocked, &sourceHash)
	if err != nil {
		return nil, err
	}
	if archivedAt != nil {
		r.ArchivedAt = *archivedAt
	}
	if trashedAt != nil {
		r.TrashedAt = *trashedAt
	}
	if contentHash != nil {
		r.ContentHash = *contentHash
	}
	r.SourceLocked = sourceLocked != 0
	if sourceHash != nil {
		r.SourceHash = *sourceHash
	}
	r.Tags, err = c.tagsFor(id)
	if err != nil {
		return nil, err
	}
	r.DerivedFrom, err = c.lineageFor(id)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Remove permanently deletes a trace from disk and the database.
// Use Trash for recoverable deletion.
func (c *Cortex) Remove(id string) error {
	if err := c.CheckSourceLock(id); err != nil {
		return err
	}
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if err := os.Remove(c.filePath(r)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing file: %w", err)
	}
	_, err = c.DB.Exec(`DELETE FROM traces WHERE id = ?`, id)
	return err
}

// Trash moves a trace to the trash directory for deferred deletion.
func (c *Cortex) Trash(id string) error {
	if err := c.CheckSourceLock(id); err != nil {
		return err
	}
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if r.TrashedAt != "" {
		return fmt.Errorf("trace %s is already in trash", id)
	}
	src := c.filePath(r)
	dst := c.TrashFile(id)
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("moving file to trash: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE traces SET trashed_at = ?, archived_at = NULL WHERE id = ?`, now, id); err != nil {
		return err
	}
	if err := c.emitEvent(tx, event.ActionTrash, id, now, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// Recover moves a trace out of the trash and back to the active traces directory.
func (c *Cortex) Recover(id string) error {
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if r.TrashedAt == "" {
		return fmt.Errorf("trace %s is not in trash", id)
	}
	src := c.TrashFile(id)
	dst := c.TraceFile(id, false)
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("recovering file from trash: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE traces SET trashed_at = NULL WHERE id = ?`, id); err != nil {
		return err
	}
	if err := c.emitEvent(tx, event.ActionRecover, id, now, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// Purge permanently deletes traces that have been in the trash for more than
// days days. A days value of 0 is treated as 30.
func (c *Cortex) Purge(days int) error {
	if days <= 0 {
		days = 30
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
	rows, err := c.DB.Query(`SELECT id FROM traces WHERE trashed_at IS NOT NULL AND trashed_at < ?`, cutoff)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range ids {
		if err := os.Remove(c.TrashFile(id)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("purging %s: %w", id, err)
		}
		tx, err := c.DB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM traces WHERE id = ?`, id); err != nil {
			tx.Rollback()
			return fmt.Errorf("purging %s from db: %w", id, err)
		}
		if err := c.emitEvent(tx, event.ActionPurge, id, now, nil); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cortex) Archive(id string) error {
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if r.TrashedAt != "" {
		return fmt.Errorf("trace %s is in trash — recover it first", id)
	}
	if r.ArchivedAt != "" {
		return fmt.Errorf("trace %s is already archived", id)
	}
	src := c.TraceFile(id, false)
	dst := c.TraceFile(id, true)
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("moving file: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE traces SET archived_at = ? WHERE id = ?`, now, id); err != nil {
		return err
	}
	if err := c.emitEvent(tx, event.ActionArchive, id, now, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Cortex) Unarchive(id string) error {
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if r.ArchivedAt == "" {
		return fmt.Errorf("trace %s is not archived", id)
	}
	src := c.TraceFile(id, true)
	dst := c.TraceFile(id, false)
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("moving file: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE traces SET archived_at = NULL WHERE id = ?`, id); err != nil {
		return err
	}
	if err := c.emitEvent(tx, event.ActionUnarchive, id, now, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkArchivedNoMove updates the DB to mark a trace as archived and emits
// an ActionArchive event — without moving any files. The filesystem watcher
// uses this when an external tool (e.g. Finder drag) has already moved the
// file from traces/ to archive/traces/. Returns nil if the trace is already
// archived (idempotent). Errors if the trace is in the trash.
func (c *Cortex) MarkArchivedNoMove(id string) error {
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if r.TrashedAt != "" {
		return fmt.Errorf("trace %s is in trash — recover it first", id)
	}
	if r.ArchivedAt != "" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE traces SET archived_at = ? WHERE id = ?`, now, id); err != nil {
		return err
	}
	if err := c.emitEvent(tx, event.ActionArchive, id, now, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkUnarchivedNoMove clears archived_at and emits ActionUnarchive without
// moving the file. Used by the watcher when a file reappears in traces/
// after being in archive/traces/. Idempotent if already unarchived.
func (c *Cortex) MarkUnarchivedNoMove(id string) error {
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if r.ArchivedAt == "" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE traces SET archived_at = NULL WHERE id = ?`, id); err != nil {
		return err
	}
	if err := c.emitEvent(tx, event.ActionUnarchive, id, now, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkTrashedNoMove stamps trashed_at and emits ActionTrash without moving
// the file. Used when the watcher sees a file appear in trash/traces/ via
// an external move. Clears archived_at to match existing Trash semantics.
// Idempotent if already trashed.
func (c *Cortex) MarkTrashedNoMove(id string) error {
	if err := c.CheckSourceLock(id); err != nil {
		return err
	}
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if r.TrashedAt != "" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE traces SET trashed_at = ?, archived_at = NULL WHERE id = ?`, now, id); err != nil {
		return err
	}
	if err := c.emitEvent(tx, event.ActionTrash, id, now, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkRecoveredNoMove clears trashed_at and emits ActionRecover without
// moving the file. Used when the watcher sees a trashed trace's file
// reappear in traces/ or archive/traces/.
func (c *Cortex) MarkRecoveredNoMove(id string) error {
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if r.TrashedAt == "" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE traces SET trashed_at = NULL WHERE id = ?`, id); err != nil {
		return err
	}
	if err := c.emitEvent(tx, event.ActionRecover, id, now, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// IngestExternalDelete handles the case where a trace file vanishes from
// traces/ or archive/traces/ entirely (user deleted via Finder or rm).
// Restores the last known body from the local event log into
// trash/traces/<id>.md so the delete is recoverable, then stamps trashed_at
// and emits ActionTrash. When no event snapshot exists (pre-event-log
// traces), skips the restore and emits the event anyway with an empty body
// so federation peers still see the state transition.
//
// Source-locked foreign traces are refused with ErrSourceLocked — the
// watcher must skip rather than poison downstream peers with a tamper.
func (c *Cortex) IngestExternalDelete(id string) error {
	if err := c.CheckSourceLock(id); err != nil {
		return err
	}
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if r.TrashedAt != "" {
		return nil
	}
	// Best-effort: restore body from the event log so the delete stays
	// recoverable. If there's no snapshot, skip the file restore — the
	// state transition still gets an event so federation is consistent.
	if _, err := c.restoreBodyToTrash(id, r); err != nil {
		return fmt.Errorf("restoring deleted trace body to trash: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE traces SET trashed_at = ?, archived_at = NULL WHERE id = ?`, now, id); err != nil {
		return err
	}
	if err := c.emitEvent(tx, event.ActionTrash, id, now, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// ApplyExternalPurge handles the case where a trace file vanishes from
// trash/traces/ — the user emptied Noema's trash via the filesystem.
// Deletes the DB row and emits ActionPurge. This is the only path outside
// the age-based Purge() that permanently removes a trace through the event
// log, so federation peers see it too.
func (c *Cortex) ApplyExternalPurge(id string) error {
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if r.TrashedAt == "" {
		return fmt.Errorf("trace %s is not in trash — refusing to purge", id)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM traces WHERE id = ?`, id); err != nil {
		return err
	}
	if err := c.emitEvent(tx, event.ActionPurge, id, now, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// restoreBodyToTrash reads the last create/update event for a trace and
// writes the snapshot to trash/traces/<id>.md. Returns (true, nil) on a
// successful write, (false, nil) when there's no snapshot to restore, and
// (false, err) on a real I/O or parse failure. Used by IngestExternalDelete.
func (c *Cortex) restoreBodyToTrash(id string, r *Row) (bool, error) {
	events, err := event.ForTrace(c.DB.DB, id)
	if err != nil {
		return false, err
	}
	last := lastMutationEvent(events)
	if last == nil || len(last.Data) == 0 {
		return false, nil
	}
	var data struct {
		Title        string   `json:"title"`
		Type         string   `json:"type"`
		Author       string   `json:"author"`
		Tags         []string `json:"tags"`
		DerivedFrom  []string `json:"derived_from"`
		Origin       string   `json:"origin"`
		Body         string   `json:"body"`
		ContentHash  string   `json:"content_hash"`
		SourceHash   string   `json:"source_hash"`
		SourceLocked bool     `json:"source_locked"`
	}
	if err := json.Unmarshal(last.Data, &data); err != nil {
		return false, fmt.Errorf("parsing event snapshot: %w", err)
	}
	t := &trace.Trace{
		Frontmatter: trace.Frontmatter{
			ID:           id,
			Title:        data.Title,
			Type:         data.Type,
			Author:       data.Author,
			Tags:         data.Tags,
			DerivedFrom:  data.DerivedFrom,
			Origin:       data.Origin,
			Created:      r.CreatedAt,
			Updated:      last.Timestamp,
			ContentHash:  data.ContentHash,
			SourceHash:   data.SourceHash,
			SourceLocked: data.SourceLocked,
		},
		Body: data.Body,
	}
	if err := t.Write(c.TrashFile(id)); err != nil {
		return false, fmt.Errorf("writing recovered body: %w", err)
	}
	return true, nil
}

// Update rewrites an existing trace's DB row and FTS entry from its (potentially
// edited) markdown file on disk.
func (c *Cortex) Update(id string) error {
	if err := c.CheckSourceLock(id); err != nil {
		return err
	}
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	path := c.filePath(r)
	t, err := trace.ParseFile(path)
	if err != nil {
		return err
	}

	// Stamp the update time authoritatively rather than trusting whatever the
	// editor left in the frontmatter — and write it back to the file so disk,
	// DB, and emitted event all agree. Tier is DB-managed after creation: if
	// the on-disk frontmatter drifted (manual edit), authoritative value lives
	// in the DB row and gets stamped back to the file here.
	t.Updated = time.Now().UTC().Format(time.RFC3339)
	t.Tier = r.Tier
	t.ContentHash = trace.ContentHash(t.Body)
	if err := t.Write(path); err != nil {
		return fmt.Errorf("rewriting trace file with updated timestamp: %w", err)
	}

	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`UPDATE traces SET title=?, type=?, author=?, origin=?, updated_at=?, content_hash=?, source_locked=?, source_hash=? WHERE id=?`,
		t.Title, t.Type, t.Author, t.Origin, t.Updated, t.ContentHash, boolToInt(t.SourceLocked), nullIfEmpty(t.SourceHash), id,
	)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM trace_tags WHERE trace_id = ?`, id); err != nil {
		return err
	}
	for _, tag := range t.Tags {
		if _, err := tx.Exec(`INSERT INTO trace_tags (trace_id, tag) VALUES (?, ?)`, id, tag); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM trace_lineage WHERE trace_id = ?`, id); err != nil {
		return err
	}
	for _, src := range t.DerivedFrom {
		if _, err := tx.Exec(`INSERT INTO trace_lineage (trace_id, derived_from) VALUES (?, ?)`, id, src); err != nil {
			return err
		}
	}
	if err := upsertFTS(tx, id, t.Title, t.Body, t.Tags); err != nil {
		return err
	}
	if err := c.emitEvent(tx, event.ActionUpdate, id, t.Updated, marshalTraceData(t)); err != nil {
		return err
	}
	return tx.Commit()
}

// Append adds content to the end of an existing trace's body. It reads the
// current file, appends the new content (with a newline separator if the
// existing body doesn't already end with one), recomputes the content hash,
// and emits a standard "update" event. Designed for fire-and-forget logging
// where agents append to a running trace without consuming its full body.
func (c *Cortex) Append(id, content string) error {
	if err := c.CheckSourceLock(id); err != nil {
		return err
	}
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	path := c.filePath(r)
	t, err := trace.ParseFile(path)
	if err != nil {
		return err
	}

	// Append with a newline separator if the body doesn't already end with one.
	if t.Body != "" && !strings.HasSuffix(t.Body, "\n") {
		t.Body += "\n"
	}
	t.Body += content

	t.Updated = time.Now().UTC().Format(time.RFC3339)
	t.ContentHash = trace.ContentHash(t.Body)
	if err := t.Write(path); err != nil {
		return fmt.Errorf("rewriting trace file for append: %w", err)
	}

	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`UPDATE traces SET updated_at=?, content_hash=? WHERE id=?`,
		t.Updated, t.ContentHash, id,
	)
	if err != nil {
		return err
	}
	if err := upsertFTS(tx, id, t.Title, t.Body, t.Tags); err != nil {
		return err
	}
	if err := c.emitEvent(tx, event.ActionUpdate, id, t.Updated, marshalTraceData(t)); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Cortex) scanRows(rows *sql.Rows) ([]Row, error) { //nolint:govet
	var result []Row
	for rows.Next() {
		var r Row
		var archivedAt, trashedAt, contentHash, sourceHash *string
		var sourceLocked int
		if err := rows.Scan(&r.ID, &r.Title, &r.Type, &r.Tier, &r.Author, &r.Origin, &archivedAt, &trashedAt, &r.CreatedAt, &r.UpdatedAt, &contentHash, &sourceLocked, &sourceHash); err != nil {
			return nil, err
		}
		if archivedAt != nil {
			r.ArchivedAt = *archivedAt
		}
		if trashedAt != nil {
			r.TrashedAt = *trashedAt
		}
		if contentHash != nil {
			r.ContentHash = *contentHash
		}
		r.SourceLocked = sourceLocked != 0
		if sourceHash != nil {
			r.SourceHash = *sourceHash
		}
		tags, err := c.tagsFor(r.ID)
		if err != nil {
			return nil, err
		}
		r.Tags = tags
		result = append(result, r)
	}
	return result, rows.Err()
}

// SyncResult summarises what Sync found.
type SyncResult struct {
	Added     int // files found on disk but not in DB
	Updated   int // files found on disk and already in DB (re-synced)
	Recovered int // orphaned DB rows whose files were rebuilt from the event log
	Orphaned  int // IDs in DB with no corresponding file on disk (after recovery)
}

// SyncOptions controls optional Sync behaviors.
type SyncOptions struct {
	// Recover, when true, attempts to rebuild missing files for orphaned DB
	// rows from the local event log. Off by default so manual `rm` of a trace
	// file remains a valid way to mark it for cleanup.
	Recover bool
}

// Sync reconciles the database with the current state of the markdown files on
// disk. It walks traces/, archive/traces/, and trash/traces/, upserts every
// file it finds, and reports orphaned DB rows (not deleted — just reported).
func (c *Cortex) Sync() (SyncResult, error) {
	return c.SyncWithOptions(SyncOptions{})
}

// SyncWithOptions is Sync with explicit options. See SyncOptions.
func (c *Cortex) SyncWithOptions(opts SyncOptions) (SyncResult, error) {
	type entry struct {
		path  string
		state string // "active" | "archived" | "trashed"
	}

	var entries []entry
	walkDir := func(dir, state string) error {
		files, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, f := range files {
			if !f.IsDir() && filepath.Ext(f.Name()) == ".md" {
				entries = append(entries, entry{filepath.Join(dir, f.Name()), state})
			}
		}
		return nil
	}
	if err := walkDir(c.TracesDir(), "active"); err != nil {
		return SyncResult{}, err
	}
	if err := walkDir(c.ArchiveDir(), "archived"); err != nil {
		return SyncResult{}, err
	}
	if err := walkDir(c.TrashDir(), "trashed"); err != nil {
		return SyncResult{}, err
	}

	seenIDs := make(map[string]bool)
	now := time.Now().UTC().Format(time.RFC3339)
	var result SyncResult

	for _, e := range entries {
		t, err := trace.ParseFile(e.path)
		if err != nil {
			continue // skip malformed files
		}
		seenIDs[t.ID] = true

		existing, dbErr := c.Get(t.ID)

		// Determine what archived_at and trashed_at should be.
		var archivedAt, trashedAt *string
		switch e.state {
		case "archived":
			if dbErr == nil && existing.ArchivedAt != "" {
				archivedAt = &existing.ArchivedAt // preserve original timestamp
			} else {
				archivedAt = &now
			}
		case "trashed":
			if dbErr == nil && existing.TrashedAt != "" {
				trashedAt = &existing.TrashedAt
			} else {
				trashedAt = &now
			}
		}

		tx, err := c.DB.Begin()
		if err != nil {
			return result, err
		}

		// Resolve the cortex_id for this trace. Local-origin traces use the
		// local ID; remote-origin traces look up the most recent event for
		// the trace ID (which carries the writing cortex's ID). If neither
		// applies, leave it empty — the next replay will fill it in.
		cortexID := ""
		if t.Origin == c.Name || t.Origin == "" {
			cortexID = c.ID
		} else {
			cortexID = c.lookupCortexIDForTrace(t.ID)
		}

		contentHash := trace.ContentHash(t.Body)
		if t.ContentHash != contentHash {
			t.ContentHash = contentHash
			if err := t.Write(e.path); err != nil {
				return result, fmt.Errorf("updating content hash for %s: %w", t.ID, err)
			}
		}
		if dbErr != nil {
			// Not in DB — insert.
			_, err = tx.Exec(
				`INSERT INTO traces (id, title, type, author, origin, cortex_id, created_at, updated_at, archived_at, trashed_at, content_hash, source_locked, source_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				t.ID, t.Title, t.Type, t.Author, t.Origin, cortexID, t.Created, t.Updated, archivedAt, trashedAt, contentHash, boolToInt(t.SourceLocked), nullIfEmpty(t.SourceHash),
			)
			if err != nil {
				tx.Rollback()
				return result, fmt.Errorf("inserting %s: %w", t.ID, err)
			}
			result.Added++
		} else {
			// Already in DB — update metadata and reconcile state.
			_, err = tx.Exec(
				`UPDATE traces SET title=?, type=?, author=?, origin=?, cortex_id=?, updated_at=?, archived_at=?, trashed_at=?, content_hash=?, source_locked=?, source_hash=? WHERE id=?`,
				t.Title, t.Type, t.Author, t.Origin, cortexID, t.Updated, archivedAt, trashedAt, contentHash, boolToInt(t.SourceLocked), nullIfEmpty(t.SourceHash), t.ID,
			)
			if err != nil {
				tx.Rollback()
				return result, fmt.Errorf("updating %s: %w", t.ID, err)
			}
			result.Updated++
		}

		if _, err := tx.Exec(`DELETE FROM trace_tags WHERE trace_id = ?`, t.ID); err != nil {
			tx.Rollback()
			return result, err
		}
		for _, tag := range t.Tags {
			if _, err := tx.Exec(`INSERT INTO trace_tags (trace_id, tag) VALUES (?, ?)`, t.ID, tag); err != nil {
				tx.Rollback()
				return result, err
			}
		}
		if _, err := tx.Exec(`DELETE FROM trace_lineage WHERE trace_id = ?`, t.ID); err != nil {
			tx.Rollback()
			return result, err
		}
		for _, src := range t.DerivedFrom {
			if _, err := tx.Exec(`INSERT INTO trace_lineage (trace_id, derived_from) VALUES (?, ?)`, t.ID, src); err != nil {
				tx.Rollback()
				return result, err
			}
		}
		if err := upsertFTS(tx, t.ID, t.Title, t.Body, t.Tags); err != nil {
			tx.Rollback()
			return result, err
		}
		if err := tx.Commit(); err != nil {
			return result, err
		}
	}

	// Find orphaned DB rows and attempt to recover their files from the local
	// event log. If a create/update event exists with a body snapshot, rewrite
	// the file from that snapshot. Anything that can't be recovered is counted
	// as still orphaned.
	rows, err := c.DB.Query(`SELECT id FROM traces`)
	if err != nil {
		return result, err
	}
	var orphanIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return result, err
		}
		if !seenIDs[id] {
			orphanIDs = append(orphanIDs, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, err
	}

	for _, id := range orphanIDs {
		if !opts.Recover {
			result.Orphaned++
			continue
		}
		recovered, err := c.recoverOrphanFromEventLog(id)
		if err != nil {
			return result, fmt.Errorf("recovering %s: %w", id, err)
		}
		if recovered {
			result.Recovered++
		} else {
			result.Orphaned++
		}
	}
	return result, nil
}

// recoverOrphanFromEventLog rebuilds a missing trace file from the most recent
// create or update event in the local event log. Returns true if the file was
// rebuilt, false if no usable snapshot exists.
func (c *Cortex) recoverOrphanFromEventLog(id string) (bool, error) {
	events, err := event.ForTrace(c.DB.DB, id)
	if err != nil {
		return false, err
	}
	last := lastMutationEvent(events)
	if last == nil {
		return false, nil
	}

	var data struct {
		Title        string   `json:"title"`
		Type         string   `json:"type"`
		Author       string   `json:"author"`
		Tags         []string `json:"tags"`
		DerivedFrom  []string `json:"derived_from"`
		Origin       string   `json:"origin"`
		Body         string   `json:"body"`
		ContentHash  string   `json:"content_hash"`
		SourceHash   string   `json:"source_hash"`
		SourceLocked bool     `json:"source_locked"`
	}
	if err := json.Unmarshal(last.Data, &data); err != nil {
		return false, fmt.Errorf("parsing event data: %w", err)
	}

	// Look up created_at + state from the DB so the recovered file matches
	// what the index believes about it.
	r, err := c.Get(id)
	if err != nil {
		return false, err
	}

	t := &trace.Trace{
		Frontmatter: trace.Frontmatter{
			ID:           id,
			Title:        data.Title,
			Type:         data.Type,
			Author:       data.Author,
			Tags:         data.Tags,
			DerivedFrom:  data.DerivedFrom,
			Origin:       data.Origin,
			Created:      r.CreatedAt,
			Updated:      last.Timestamp,
			ContentHash:  data.ContentHash,
			SourceHash:   data.SourceHash,
			SourceLocked: data.SourceLocked,
		},
		Body: data.Body,
	}

	var path string
	switch {
	case r.TrashedAt != "":
		path = c.TrashFile(id)
	case r.ArchivedAt != "":
		path = c.TraceFile(id, true)
	default:
		path = c.TraceFile(id, false)
	}
	if err := t.Write(path); err != nil {
		return false, fmt.Errorf("writing recovered file: %w", err)
	}
	return true, nil
}

func (c *Cortex) tagsFor(id string) ([]string, error) {
	rows, err := c.DB.Query(`SELECT tag FROM trace_tags WHERE trace_id = ? ORDER BY tag`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tags []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

func (c *Cortex) lineageFor(id string) ([]string, error) {
	rows, err := c.DB.Query(`SELECT derived_from FROM trace_lineage WHERE trace_id = ? ORDER BY derived_from`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sources []string
	for rows.Next() {
		var src string
		if err := rows.Scan(&src); err != nil {
			return nil, err
		}
		sources = append(sources, src)
	}
	return sources, rows.Err()
}

// DerivedBy returns all trace IDs that list the given trace as a source.
func (c *Cortex) DerivedBy(id string) ([]string, error) {
	rows, err := c.DB.Query(`SELECT trace_id FROM trace_lineage WHERE derived_from = ? ORDER BY trace_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var tid string
		if err := rows.Scan(&tid); err != nil {
			return nil, err
		}
		ids = append(ids, tid)
	}
	return ids, rows.Err()
}

// Events returns the event log for a specific trace, ordered chronologically.
func (c *Cortex) Events(traceID string) ([]event.Event, error) {
	return event.ForTrace(c.DB.DB, traceID)
}

// EventsSince returns events after the given ULID cursor, up to limit.
func (c *Cortex) EventsSince(afterID string, limit int) ([]event.Event, error) {
	return event.Since(c.DB.DB, afterID, limit)
}

// DivergenceCount returns the number of unresolved divergence traces.
func (c *Cortex) DivergenceCount() (int, error) {
	var n int
	err := c.DB.QueryRow(
		`SELECT COUNT(*) FROM traces WHERE type = 'divergence' AND archived_at IS NULL AND trashed_at IS NULL`,
	).Scan(&n)
	return n, err
}

// ResolveDivergence resolves a divergence trace by either picking one of the
// versions stored in the divergence body (by origin name) or applying a
// caller-supplied custom merge. Exactly one of acceptOrigin or customBody must
// be non-empty. The divergence trace is trashed once the original is updated.
func (c *Cortex) ResolveDivergence(divergenceID, acceptOrigin, customBody string) error {
	if acceptOrigin == "" && customBody == "" {
		return fmt.Errorf("resolution requires either an accept origin or a custom body")
	}
	if acceptOrigin != "" && customBody != "" {
		return fmt.Errorf("specify only one of accept origin or custom body")
	}

	divRow, err := c.Get(divergenceID)
	if err != nil {
		return fmt.Errorf("divergence trace %q not found", divergenceID)
	}
	if divRow.Type != string(trace.TypeDivergence) {
		return fmt.Errorf("trace %q is not a divergence (type=%s)", divergenceID, divRow.Type)
	}
	if len(divRow.DerivedFrom) == 0 {
		return fmt.Errorf("divergence trace %q has no derived_from link to original trace", divergenceID)
	}

	originalID := divRow.DerivedFrom[0]

	if err := c.CheckSourceLock(originalID); err != nil {
		return err
	}

	divPath := c.filePath(divRow)

	if customBody != "" {
		if err := c.applyResolutionBody(originalID, customBody); err != nil {
			return err
		}
		return c.Trash(divergenceID)
	}

	t, err := trace.ParseFile(divPath)
	if err != nil {
		return fmt.Errorf("reading divergence trace: %w", err)
	}
	sections, err := splitDivergenceSections(t.Body)
	if err != nil {
		return fmt.Errorf("parsing divergence trace %q: %w", divergenceID, err)
	}
	for _, sec := range sections {
		if matchVersionLabel(acceptOrigin, sec.name, sec.cortexID) {
			if err := c.applyResolutionBody(originalID, strings.TrimSpace(sec.body)); err != nil {
				return err
			}
			return c.Trash(divergenceID)
		}
	}
	available := make([]string, 0, len(sections))
	for _, sec := range sections {
		available = append(available, formatVersionLabel(sec.name, sec.cortexID))
	}
	return fmt.Errorf("origin %q not found in divergence %q (available: %s)",
		acceptOrigin, divergenceID, strings.Join(available, ", "))
}

// applyResolutionBody overwrites the original trace's body and emits an update event.
func (c *Cortex) applyResolutionBody(originalID, newBody string) error {
	r, err := c.Get(originalID)
	if err != nil {
		return fmt.Errorf("original trace %q not found", originalID)
	}
	path := c.filePath(r)
	t, err := trace.ParseFile(path)
	if err != nil {
		return fmt.Errorf("reading original trace: %w", err)
	}
	t.Body = newBody
	if err := t.Write(path); err != nil {
		return err
	}
	return c.Update(originalID)
}

// idPrefixLen is how many characters of a cortex ULID are shown in
// divergence headers and conflict labels for human disambiguation. Eight
// characters of Crockford base32 collide with vanishingly low probability
// at any realistic federation size.
const idPrefixLen = 8

// formatVersionLabel renders a cortex display label for divergence headers
// in the form "<name> (<id-prefix>)". The id prefix lets users tell apart
// two peers that happen to share a display name (the same-name guardrail
// blocks this at config time, but historic records may still contain it).
func formatVersionLabel(name, cortexID string) string {
	if cortexID == "" {
		return name
	}
	prefix := cortexID
	if len(prefix) > idPrefixLen {
		prefix = prefix[:idPrefixLen]
	}
	return fmt.Sprintf("%s (%s)", name, prefix)
}

// matchVersionLabel reports whether `accept` selects the given (name, id)
// version. Users can pass either the display name, the full ULID, or the
// 8-char id prefix shown in the divergence header.
func matchVersionLabel(accept, name, cortexID string) bool {
	if accept == "" {
		return false
	}
	if accept == name {
		return true
	}
	if cortexID != "" && accept == cortexID {
		return true
	}
	if cortexID != "" && len(cortexID) >= idPrefixLen && accept == cortexID[:idPrefixLen] {
		return true
	}
	return false
}

// extractVersionBody returns the body content of the `### Version from <label>`
// section in a divergence trace, stripping the `**Vector clock:**` metadata
// line that follows the header. `accept` may be a display name, a full ULID,
// or the 8-char id prefix shown in the header.
func extractVersionBody(divID, path, accept string) (string, error) {
	t, err := trace.ParseFile(path)
	if err != nil {
		return "", fmt.Errorf("reading divergence trace: %w", err)
	}

	sections, err := splitDivergenceSections(t.Body)
	if err != nil {
		return "", fmt.Errorf("parsing divergence trace %q: %w", divID, err)
	}
	for _, sec := range sections {
		if matchVersionLabel(accept, sec.name, sec.cortexID) {
			return strings.TrimSpace(sec.body), nil
		}
	}
	return "", fmt.Errorf("divergence trace %q has no version matching %q", divID, accept)
}

// divergenceSection is one parsed `### Version from <label>` block.
type divergenceSection struct {
	name     string // peer display name
	cortexID string // peer cortex ULID, "" if header had no parenthetical
	body     string // version body, with the **Vector clock:** line stripped
}

// splitDivergenceSections walks the body of a divergence trace and returns
// every `### Version from <label>` block. The label is parsed back into its
// (name, id-prefix) components — id-prefix is a *prefix*, so callers should
// use matchVersionLabel for comparison rather than equality.
func splitDivergenceSections(body string) ([]divergenceSection, error) {
	const headerPrefix = "### Version from "
	var out []divergenceSection
	rest := body
	for {
		idx := strings.Index(rest, headerPrefix)
		if idx < 0 {
			break
		}
		rest = rest[idx+len(headerPrefix):]
		// Header label runs to end of line.
		nl := strings.Index(rest, "\n")
		var label string
		if nl < 0 {
			label = rest
			rest = ""
		} else {
			label = rest[:nl]
			rest = rest[nl+1:]
		}
		// Strip optional `**Vector clock:** ...` metadata line.
		if strings.HasPrefix(rest, "**Vector clock:**") {
			if nl := strings.Index(rest, "\n"); nl >= 0 {
				rest = rest[nl+1:]
			} else {
				rest = ""
			}
		}
		// Section body runs until the next `### Version from ` header.
		var sectionBody string
		if next := strings.Index(rest, "\n"+headerPrefix); next >= 0 {
			sectionBody = rest[:next]
			rest = rest[next+1:]
		} else {
			sectionBody = rest
			rest = ""
		}
		name, id := parseVersionLabel(label)
		out = append(out, divergenceSection{name: name, cortexID: id, body: sectionBody})
		if rest == "" {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no '### Version from ' headers found")
	}
	return out, nil
}

// parseVersionLabel splits a header label "<name> (<id-prefix>)" back into
// its components. Labels written by older binaries (no parenthetical) are
// returned with cortexID == "".
func parseVersionLabel(label string) (name, cortexID string) {
	label = strings.TrimSpace(label)
	if !strings.HasSuffix(label, ")") {
		return label, ""
	}
	open := strings.LastIndex(label, " (")
	if open < 0 {
		return label, ""
	}
	id := label[open+2 : len(label)-1]
	return label[:open], id
}

// emitEvent appends an event to the log inside the given transaction,
// incrementing the local vector clock. All reads/writes go through the tx
// to avoid SQLite lock contention. Vector clocks are keyed on the cortex
// ID (a stable ULID), not the cortex name — see docs/design/cortex-uuid-plan.md.
func (c *Cortex) emitEvent(tx *sql.Tx, action event.Action, traceID, timestamp string, data json.RawMessage) error {
	// Read clock from federation_state within the transaction.
	vc, err := getClockTx(tx)
	if err != nil {
		vc = make(federation.VClock)
	}
	vc.Increment(c.ID)

	e := &event.Event{
		ID:        event.NewULID(),
		Action:    action,
		TraceID:   traceID,
		CortexID:  c.ID,
		Origin:    c.Name,
		Timestamp: timestamp,
		Data:      data,
		VClock:    vc,
	}
	if err := event.Append(tx, e); err != nil {
		return err
	}
	// Persist updated clock within the same transaction.
	return setClockTx(tx, vc)
}

func getClockTx(tx *sql.Tx) (federation.VClock, error) {
	var val string
	err := tx.QueryRow(`SELECT value FROM federation_state WHERE key = 'vclock'`).Scan(&val)
	if err != nil {
		return make(federation.VClock), nil // not found or error — start fresh
	}
	var vc federation.VClock
	if err := json.Unmarshal([]byte(val), &vc); err != nil {
		return make(federation.VClock), nil
	}
	return vc, nil
}

func setClockTx(tx *sql.Tx, vc federation.VClock) error {
	data, err := json.Marshal(vc)
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		`INSERT INTO federation_state (key, value) VALUES ('vclock', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		string(data),
	)
	return err
}

// ReplayEvent materializes a remote event locally without emitting a new event.
// The remote event is stored in the local log with its original ID and origin.
func (c *Cortex) ReplayEvent(e event.Event) error {
	// Guard: reject trace IDs that could escape the cortex directory.
	if !trace.IsValidID(e.TraceID) {
		return fmt.Errorf("rejecting remote event %s: %w: %q", e.ID, trace.ErrInvalidTraceID, e.TraceID)
	}

	// Idempotency: skip if already in local log.
	existing, err := event.ForTrace(c.DB.DB, e.TraceID)
	if err != nil {
		return err
	}
	for _, ex := range existing {
		if ex.ID == e.ID {
			return nil // already replayed
		}
	}

	var replayErr error
	switch e.Action {
	case event.ActionCreate:
		replayErr = c.replayCreate(e)
	case event.ActionUpdate:
		replayErr = c.replayUpdate(e)
	case event.ActionArchive:
		replayErr = c.replayArchive(e)
	case event.ActionUnarchive:
		replayErr = c.replayUnarchive(e)
	case event.ActionTrash:
		replayErr = c.replayTrash(e)
	case event.ActionRecover:
		replayErr = c.replayRecover(e)
	case event.ActionPurge:
		replayErr = c.replayPurge(e)
	default:
		return fmt.Errorf("unknown event action: %s", e.Action)
	}
	// In a full-mesh federation, multiple peers may serve the same event.
	// Two sync goroutines can race past the idempotency check above and
	// both attempt the insert. Treat UNIQUE violations as success.
	if replayErr != nil && strings.Contains(replayErr.Error(), "UNIQUE constraint failed") {
		return nil
	}
	return replayErr
}

func (c *Cortex) replayCreate(e event.Event) error {
	var data struct {
		Title        string   `json:"title"`
		Type         string   `json:"type"`
		Author       string   `json:"author"`
		Tags         []string `json:"tags"`
		DerivedFrom  []string `json:"derived_from"`
		Origin       string   `json:"origin"`
		Body         string   `json:"body"`
		ContentHash  string   `json:"content_hash"`
		SourceHash   string   `json:"source_hash"`
		SourceLocked bool     `json:"source_locked"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return fmt.Errorf("parsing create event data: %w", err)
	}

	// Verify content hash integrity: the peer-supplied hash must match the
	// actual body. Without this check a compromised peer could send tampered
	// content with the original hash, and noema verify would report it clean.
	if data.ContentHash != "" {
		if got := trace.ContentHash(data.Body); got != data.ContentHash {
			return fmt.Errorf("content hash mismatch on create event %s for trace %s: expected %s, got %s", e.ID, e.TraceID, data.ContentHash, got)
		}
	}

	// Only honor source_locked from genuinely foreign events. A peer should
	// not be able to lock traces that originate from the local cortex.
	sourceLocked := data.SourceLocked && e.CortexID != c.ID

	t := &trace.Trace{
		Frontmatter: trace.Frontmatter{
			ID:           e.TraceID,
			Title:        data.Title,
			Type:         data.Type,
			Author:       data.Author,
			Tags:         data.Tags,
			DerivedFrom:  data.DerivedFrom,
			Origin:       data.Origin,
			Created:      e.Timestamp,
			Updated:      e.Timestamp,
			ContentHash:  data.ContentHash,
			SourceHash:   data.SourceHash,
			SourceLocked: sourceLocked,
		},
		Body: data.Body,
	}

	path := c.TraceFile(t.ID, false)
	if err := t.Write(path); err != nil {
		return fmt.Errorf("writing replayed trace file: %w", err)
	}

	// cleanupFile removes the trace file on error — but NOT when the error is
	// a UNIQUE constraint violation, because in a full-mesh race the winning
	// goroutine already owns that file.
	cleanupFile := func(err error) {
		if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
			os.Remove(path)
		}
	}

	tx, err := c.DB.Begin()
	if err != nil {
		os.Remove(path)
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO traces (id, title, type, author, origin, cortex_id, created_at, updated_at, content_hash, source_locked, source_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Title, t.Type, t.Author, t.Origin, e.CortexID, t.Created, t.Updated, t.ContentHash, boolToInt(t.SourceLocked), nullIfEmpty(t.SourceHash),
	)
	if err != nil {
		cleanupFile(err)
		return err
	}
	for _, tag := range t.Tags {
		if _, err := tx.Exec(`INSERT INTO trace_tags (trace_id, tag) VALUES (?, ?)`, t.ID, tag); err != nil {
			cleanupFile(err)
			return err
		}
	}
	for _, src := range t.DerivedFrom {
		if _, err := tx.Exec(`INSERT INTO trace_lineage (trace_id, derived_from) VALUES (?, ?)`, t.ID, src); err != nil {
			cleanupFile(err)
			return err
		}
	}
	if err := insertFTS(tx, t.ID, t.Title, t.Body, t.Tags); err != nil {
		cleanupFile(err)
		return err
	}
	if err := event.Append(tx, &e); err != nil {
		cleanupFile(err)
		return err
	}
	return tx.Commit()
}

func (c *Cortex) replayUpdate(e event.Event) error {
	var data struct {
		Title        string   `json:"title"`
		Type         string   `json:"type"`
		Author       string   `json:"author"`
		Tags         []string `json:"tags"`
		DerivedFrom  []string `json:"derived_from"`
		Origin       string   `json:"origin"`
		Body         string   `json:"body"`
		ContentHash  string   `json:"content_hash"`
		SourceHash   string   `json:"source_hash"`
		SourceLocked bool     `json:"source_locked"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return fmt.Errorf("parsing update event data: %w", err)
	}

	r, err := c.Get(e.TraceID)
	if err != nil {
		// Federation is eventually consistent: a peer's update event can
		// arrive before its create event (e.g. our cursor pointed past the
		// create after a partial sync, or the peer's create predates our
		// pairing). Update events carry the full trace snapshot via
		// marshalTraceData(), so we can materialize the trace from the
		// update alone instead of getting stuck waiting for a create that
		// may never come. This trades a missing "create" entry in the
		// audit trail for not losing the trace entirely.
		if errors.Is(err, sql.ErrNoRows) {
			return c.replayCreate(e)
		}
		return fmt.Errorf("trace %s not found for replay update: %w", e.TraceID, err)
	}

	// Verify content hash integrity before applying the update.
	if data.ContentHash != "" {
		if got := trace.ContentHash(data.Body); got != data.ContentHash {
			return fmt.Errorf("content hash mismatch on update event %s for trace %s: expected %s, got %s", e.ID, e.TraceID, data.ContentHash, got)
		}
	}

	// Conflict detection: compare vector clocks with last local mutation.
	if len(e.VClock) > 0 {
		localEvents, _ := event.ForTrace(c.DB.DB, e.TraceID)
		if lastLocal := lastMutationEvent(localEvents); lastLocal != nil && len(lastLocal.VClock) > 0 {
			rel := federation.Compare(lastLocal.VClock, e.VClock)
			if rel == 0 {
				// Concurrent — neither clock dominates. Create a divergence trace.
				return c.createDivergence(r, e, data.Body, lastLocal.VClock, e.VClock)
			}
		}
	}

	sourceLocked := data.SourceLocked && e.CortexID != c.ID

	t := &trace.Trace{
		Frontmatter: trace.Frontmatter{
			ID:           e.TraceID,
			Title:        data.Title,
			Type:         data.Type,
			Author:       data.Author,
			Tags:         data.Tags,
			DerivedFrom:  data.DerivedFrom,
			Origin:       data.Origin,
			Created:      r.CreatedAt,
			Updated:      e.Timestamp,
			ContentHash:  data.ContentHash,
			SourceHash:   data.SourceHash,
			SourceLocked: sourceLocked,
		},
		Body: data.Body,
	}

	path := c.filePath(r)
	if err := t.Write(path); err != nil {
		return err
	}

	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`UPDATE traces SET title=?, type=?, author=?, origin=?, updated_at=?, content_hash=?, source_locked=?, source_hash=? WHERE id=?`,
		t.Title, t.Type, t.Author, t.Origin, t.Updated, t.ContentHash, boolToInt(t.SourceLocked), nullIfEmpty(t.SourceHash), e.TraceID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM trace_tags WHERE trace_id = ?`, e.TraceID); err != nil {
		return err
	}
	for _, tag := range t.Tags {
		if _, err := tx.Exec(`INSERT INTO trace_tags (trace_id, tag) VALUES (?, ?)`, e.TraceID, tag); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM trace_lineage WHERE trace_id = ?`, e.TraceID); err != nil {
		return err
	}
	for _, src := range t.DerivedFrom {
		if _, err := tx.Exec(`INSERT INTO trace_lineage (trace_id, derived_from) VALUES (?, ?)`, e.TraceID, src); err != nil {
			return err
		}
	}
	if err := upsertFTS(tx, e.TraceID, t.Title, t.Body, t.Tags); err != nil {
		return err
	}
	if err := event.Append(tx, &e); err != nil {
		return err
	}
	return tx.Commit()
}

// lastMutationEvent returns the most recent create or update event from the
// given list, which is assumed to be in chronological order.
func lastMutationEvent(events []event.Event) *event.Event {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Action == event.ActionCreate || events[i].Action == event.ActionUpdate {
			return &events[i]
		}
	}
	return nil
}

// createDivergence builds a divergence trace that preserves both versions of
// a trace when concurrent edits are detected. The body is rendered
// deterministically (versions sorted by cortex_id, JSON map keys sorted by
// encoding/json) so every replica produces byte-identical content. Combined
// with the deterministic divergence ID, this means concurrent createDivergence
// calls across the cluster collapse into a single row via the UNIQUE
// constraint instead of fighting over the file. Version headers carry the
// peer's display name plus an 8-char id prefix so users can disambiguate
// peers that share a name without losing the human-readable label.
func (c *Cortex) createDivergence(
	localRow *Row,
	remoteEvent event.Event,
	remoteBody string,
	localClock, remoteClock federation.VClock,
) error {
	// Read the current local body.
	localPath := c.filePath(localRow)
	localTrace, err := trace.ParseFile(localPath)
	if err != nil {
		return fmt.Errorf("reading local trace for divergence: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	divID := trace.NewID("divergence-" + localRow.ID)

	// Collect both versions and sort by cortex_id so the rendered body is
	// identical regardless of which side observed the conflict first. Sorting
	// by ID (not name) keeps the order stable even if a peer is renamed.
	type version struct {
		origin   string
		cortexID string
		clock    federation.VClock
		body     string
	}
	versions := []version{
		{origin: c.Name, cortexID: c.ID, clock: localClock, body: localTrace.Body},
		{origin: remoteEvent.Origin, cortexID: remoteEvent.CortexID, clock: remoteClock, body: remoteBody},
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].cortexID < versions[j].cortexID })

	origins := make([]string, len(versions))
	for i, v := range versions {
		origins[i] = formatVersionLabel(v.origin, v.cortexID)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "## Concurrent edits detected\n\n")
	fmt.Fprintf(&sb, "**Trace:** %s\n", localRow.ID)
	fmt.Fprintf(&sb, "**Conflicting origins:** %s\n", strings.Join(origins, ", "))
	for _, v := range versions {
		clockJSON, _ := json.Marshal(v.clock)
		fmt.Fprintf(&sb, "\n### Version from %s\n", formatVersionLabel(v.origin, v.cortexID))
		fmt.Fprintf(&sb, "**Vector clock:** %s\n\n", string(clockJSON))
		sb.WriteString(v.body)
		if !strings.HasSuffix(v.body, "\n") {
			sb.WriteByte('\n')
		}
	}
	body := strings.TrimRight(sb.String(), "\n")

	divTrace := &trace.Trace{
		Frontmatter: trace.Frontmatter{
			ID:          divID,
			Title:       "Divergence: " + localRow.Title,
			Type:        string(trace.TypeDivergence),
			Author:      "system",
			Tags:        []string{"divergence", "needs-resolution"},
			DerivedFrom: []string{localRow.ID},
			Origin:      c.Name,
			Created:     now,
			Updated:     now,
		},
		Body: body,
	}

	// Write the divergence trace file.
	divPath := c.TraceFile(divID, false)
	if err := divTrace.Write(divPath); err != nil {
		return fmt.Errorf("writing divergence trace: %w", err)
	}

	// Insert into DB within a transaction.
	tx, err := c.DB.Begin()
	if err != nil {
		os.Remove(divPath)
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO traces (id, title, type, author, origin, cortex_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		divTrace.ID, divTrace.Title, divTrace.Type, divTrace.Author, divTrace.Origin, c.ID, divTrace.Created, divTrace.Updated,
	)
	if err != nil {
		os.Remove(divPath)
		return err
	}
	for _, tag := range divTrace.Tags {
		if _, err := tx.Exec(`INSERT INTO trace_tags (trace_id, tag) VALUES (?, ?)`, divID, tag); err != nil {
			os.Remove(divPath)
			return err
		}
	}
	for _, src := range divTrace.DerivedFrom {
		if _, err := tx.Exec(`INSERT INTO trace_lineage (trace_id, derived_from) VALUES (?, ?)`, divID, src); err != nil {
			os.Remove(divPath)
			return err
		}
	}
	if err := insertFTS(tx, divID, divTrace.Title, body, divTrace.Tags); err != nil {
		os.Remove(divPath)
		return err
	}

	// Emit a create event for the divergence trace.
	if err := c.emitEvent(tx, event.ActionCreate, divID, now, marshalTraceData(divTrace)); err != nil {
		os.Remove(divPath)
		return err
	}

	// Also store the remote event that triggered the divergence, so we don't
	// replay it again (idempotency).
	if err := event.Append(tx, &remoteEvent); err != nil {
		os.Remove(divPath)
		return err
	}

	return tx.Commit()
}

func (c *Cortex) replayArchive(e event.Event) error {
	r, err := c.Get(e.TraceID)
	if err != nil {
		// Trace doesn't exist locally (we missed the create, or it was
		// already hard-deleted by Remove()). Archive is a no-op against
		// nothing — record the event for the audit trail and move on
		// instead of pinning the cursor on a permanent failure.
		if errors.Is(err, sql.ErrNoRows) {
			return c.storeRemoteEvent(e)
		}
		return err
	}
	if r.ArchivedAt != "" {
		// Already archived — just store the event.
		return c.storeRemoteEvent(e)
	}
	src := c.TraceFile(e.TraceID, false)
	dst := c.TraceFile(e.TraceID, true)
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE traces SET archived_at = ? WHERE id = ?`, e.Timestamp, e.TraceID); err != nil {
		return err
	}
	if err := event.Append(tx, &e); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Cortex) replayUnarchive(e event.Event) error {
	r, err := c.Get(e.TraceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.storeRemoteEvent(e)
		}
		return err
	}
	if r.ArchivedAt == "" {
		return c.storeRemoteEvent(e)
	}
	src := c.TraceFile(e.TraceID, true)
	dst := c.TraceFile(e.TraceID, false)
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE traces SET archived_at = NULL WHERE id = ?`, e.TraceID); err != nil {
		return err
	}
	if err := event.Append(tx, &e); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Cortex) replayTrash(e event.Event) error {
	r, err := c.Get(e.TraceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.storeRemoteEvent(e)
		}
		return err
	}
	if r.TrashedAt != "" {
		return c.storeRemoteEvent(e)
	}
	src := c.filePath(r)
	dst := c.TrashFile(e.TraceID)
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE traces SET trashed_at = ?, archived_at = NULL WHERE id = ?`, e.Timestamp, e.TraceID); err != nil {
		return err
	}
	if err := event.Append(tx, &e); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Cortex) replayRecover(e event.Event) error {
	r, err := c.Get(e.TraceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.storeRemoteEvent(e)
		}
		return err
	}
	if r.TrashedAt == "" {
		return c.storeRemoteEvent(e)
	}
	src := c.TrashFile(e.TraceID)
	dst := c.TraceFile(e.TraceID, false)
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE traces SET trashed_at = NULL WHERE id = ?`, e.TraceID); err != nil {
		return err
	}
	if err := event.Append(tx, &e); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Cortex) replayPurge(e event.Event) error {
	// Just store the event — the trace may already be gone locally.
	_ = os.Remove(c.TrashFile(e.TraceID))
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Best-effort delete; trace may not exist locally.
	tx.Exec(`DELETE FROM traces WHERE id = ?`, e.TraceID)
	if err := event.Append(tx, &e); err != nil {
		return err
	}
	return tx.Commit()
}

// storeRemoteEvent stores a remote event without any state change (trace already in expected state).
func (c *Cortex) storeRemoteEvent(e event.Event) error {
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := event.Append(tx, &e); err != nil {
		return err
	}
	return tx.Commit()
}

// MergeClock merges a remote vector clock into the local clock. The
// merge is capped at federation.MaxVClockEntries to prevent a malicious
// peer from inflating the clock with synthetic cortex IDs.
func (c *Cortex) MergeClock(remote federation.VClock) error {
	state := federation.NewState(c.DB.DB)
	local, err := state.GetClock()
	if err != nil {
		return err
	}
	merged, err := federation.MergeCapped(local, remote)
	if err != nil {
		return err
	}
	return state.SetClock(merged)
}

// GetClock returns the current vector clock.
func (c *Cortex) GetClock() (federation.VClock, error) {
	return federation.NewState(c.DB.DB).GetClock()
}

// BackfillResult summarises a `noema events backfill` operation. The slices
// hold trace IDs (not row counts) so the caller can render them line-by-line
// for the operator's audit trail.
type BackfillResult struct {
	BackfilledIDs []string // active traces that received a synthetic create event
	SkippedIDs    []string // traces with no create event but currently archived/trashed
}

// BackfillCreateEvents emits synthetic `create` events for any active trace
// that lacks one in the event log. This folds traces that pre-date the event
// log — or that landed via `noema sync`, which intentionally emits no events
// because it is reconciliation, not a semantic mutation — back into the
// federated history so peers can replay them.
//
// Each backfilled event uses a fresh ULID, the local cortex_id and origin,
// the current wall-clock time as the event timestamp, and a JSON snapshot of
// the trace's current frontmatter + body. The trace's own `created` field
// (in the markdown frontmatter and the DB row) is left untouched, so the
// audit trail still surfaces "this happened on <real date>" — the event
// timestamp only records when the backfill ran. Using wall-clock time keeps
// per-cortex ULID monotonicity and avoids the event log lying about when
// the event was actually appended.
//
// Archived and trashed traces are skipped: emitting only a `create` event
// for them would leave federation diverged (peers would materialise the
// trace as active and never see the archive/trash). Recover or unarchive
// the trace first if it needs to federate.
//
// If dryRun is true, no events are written and the vector clock is not
// touched, but the returned result still lists every trace that would have
// been backfilled or skipped — so operators can preview before committing.
//
// The iteration is idempotent: traces that already have a create event in
// the log (whether locally emitted or replayed from a peer) are not in the
// candidate set, so running this twice is a no-op on the second pass.
func (c *Cortex) BackfillCreateEvents(dryRun bool) (BackfillResult, error) {
	var result BackfillResult

	// Candidate set: every trace whose ID is missing from the create-event
	// table. We deliberately do *not* filter on archived_at / trashed_at in
	// SQL — we want to surface those skipped IDs to the operator instead of
	// silently dropping them, so the filter happens in Go below.
	rows, err := c.DB.Query(`
		SELECT id, archived_at, trashed_at FROM traces
		WHERE id NOT IN (SELECT trace_id FROM events WHERE action = 'create')
		ORDER BY created_at, id
	`)
	if err != nil {
		return result, fmt.Errorf("scanning candidate traces: %w", err)
	}
	type candidate struct {
		id       string
		archived bool
		trashed  bool
	}
	var candidates []candidate
	for rows.Next() {
		var id string
		var archivedAt, trashedAt *string
		if err := rows.Scan(&id, &archivedAt, &trashedAt); err != nil {
			rows.Close()
			return result, err
		}
		candidates = append(candidates, candidate{
			id:       id,
			archived: archivedAt != nil,
			trashed:  trashedAt != nil,
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, err
	}

	for _, cand := range candidates {
		if cand.archived || cand.trashed {
			result.SkippedIDs = append(result.SkippedIDs, cand.id)
			continue
		}
		if dryRun {
			result.BackfilledIDs = append(result.BackfilledIDs, cand.id)
			continue
		}
		if err := c.backfillCreateEvent(cand.id); err != nil {
			return result, fmt.Errorf("backfilling %s: %w", cand.id, err)
		}
		result.BackfilledIDs = append(result.BackfilledIDs, cand.id)
	}

	return result, nil
}

// backfillCreateEvent loads the active trace's markdown file and emits a
// single `create` event in its own transaction. We open one transaction per
// trace rather than batching the entire backfill: a multi-hundred-trace
// backfill in one tx would hold a write lock long enough to stall any
// concurrent serve process, and a partial failure mid-batch is fine here
// because the iteration query is idempotent — re-running picks up exactly
// what wasn't yet committed.
func (c *Cortex) backfillCreateEvent(traceID string) error {
	path := c.TraceFile(traceID, false)
	t, err := trace.ParseFile(path)
	if err != nil {
		return fmt.Errorf("loading trace file %s: %w", path, err)
	}

	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Use wall-clock time as the event timestamp so per-cortex ULID
	// monotonicity holds and the event log doesn't claim a backfilled
	// row was appended years ago. The trace's own `created` field
	// preserves the original chronology.
	now := time.Now().UTC().Format(time.RFC3339)
	if err := c.emitEvent(tx, event.ActionCreate, traceID, now, marshalTraceData(t)); err != nil {
		return err
	}
	return tx.Commit()
}

// marshalTraceData builds a JSON snapshot of a trace for event payloads.
func marshalTraceData(t *trace.Trace) json.RawMessage {
	payload := struct {
		Title        string   `json:"title"`
		Type         string   `json:"type"`
		Author       string   `json:"author,omitempty"`
		Tags         []string `json:"tags,omitempty"`
		DerivedFrom  []string `json:"derived_from,omitempty"`
		Origin       string   `json:"origin,omitempty"`
		Body         string   `json:"body"`
		ContentHash  string   `json:"content_hash,omitempty"`
		SourceHash   string   `json:"source_hash,omitempty"`
		SourceLocked bool     `json:"source_locked,omitempty"`
	}{
		Title:        t.Title,
		Type:         t.Type,
		Author:       t.Author,
		Tags:         t.Tags,
		DerivedFrom:  t.DerivedFrom,
		Origin:       t.Origin,
		Body:         t.Body,
		ContentHash:  t.ContentHash,
		SourceHash:   t.SourceHash,
		SourceLocked: t.SourceLocked,
	}
	data, _ := json.Marshal(payload)
	return data
}

// ftsTagText joins a trace's tags into one space-delimited string for
// FTS5 indexing. The porter/ascii tokenizer splits on whitespace and
// indexes each tag as an independent token, so tagging a trace with
// e.g. `{"auth", "session", "oauth"}` makes each value findable via
// search_traces even when none of them appear in the title or body.
func ftsTagText(tags []string) string {
	return strings.Join(tags, " ")
}

// insertFTS writes a row into traces_fts. Used for fresh creates where
// no existing row for the id is possible. Inserting on top of a stale
// row would create duplicates (FTS5 does not treat id as a key), which
// is why the update paths use upsertFTS instead.
func insertFTS(tx *sql.Tx, id, title, body string, tags []string) error {
	_, err := tx.Exec(
		`INSERT INTO traces_fts (id, title, body, tags) VALUES (?, ?, ?, ?)`,
		id, title, body, ftsTagText(tags),
	)
	return err
}

// upsertFTS removes any existing row for id and re-inserts with the
// current content. Used from every path that may be running against
// a trace already in the index (updates, appends, federation replays,
// and the Sync reindex loop).
func upsertFTS(tx *sql.Tx, id, title, body string, tags []string) error {
	if _, err := tx.Exec(`DELETE FROM traces_fts WHERE id = ?`, id); err != nil {
		return err
	}
	return insertFTS(tx, id, title, body, tags)
}

// RebuildFTSIfStale repopulates traces_fts when it contains fewer rows
// than the traces table (including trashed, archived, and active rows
// — everything that should be searchable). The mismatch is expected
// exactly once per cortex, immediately after migration 008 runs and
// replaces the virtual table. On fresh cortexes and subsequent opens
// the counts match and this is a no-op.
//
// The rebuild walks every row in the traces table, reads the trace's
// body from disk (since body is not in SQL), and re-inserts all four
// FTS columns. Rows whose files are missing on disk are inserted with
// empty body so at least their title and tags remain searchable —
// operators can fix those with `noema sync` later.
//
// Runs outside any caller's transaction because it iterates many
// traces and couldn't share a short-lived transaction with them
// cleanly. Each row is reinserted in its own tx so a mid-rebuild
// failure leaves the index partially populated rather than empty.
func (c *Cortex) RebuildFTSIfStale() error {
	var tracesCount, ftsCount int
	if err := c.DB.QueryRow(`SELECT COUNT(*) FROM traces`).Scan(&tracesCount); err != nil {
		return fmt.Errorf("counting traces: %w", err)
	}
	if err := c.DB.QueryRow(`SELECT COUNT(*) FROM traces_fts`).Scan(&ftsCount); err != nil {
		return fmt.Errorf("counting traces_fts: %w", err)
	}
	if ftsCount >= tracesCount {
		return nil
	}

	rows, err := c.DB.Query(
		`SELECT id, title, archived_at, trashed_at FROM traces`,
	)
	if err != nil {
		return fmt.Errorf("selecting traces for reindex: %w", err)
	}
	defer rows.Close()

	type rebuildEntry struct {
		id, title   string
		archivedAt  string
		trashedAt   string
	}
	var entries []rebuildEntry
	for rows.Next() {
		var e rebuildEntry
		var archived, trashed *string
		if err := rows.Scan(&e.id, &e.title, &archived, &trashed); err != nil {
			return err
		}
		if archived != nil {
			e.archivedAt = *archived
		}
		if trashed != nil {
			e.trashedAt = *trashed
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr,
		"[cortex] reindexing FTS5 for %d traces (first boot after upgrade; this runs once)\n",
		len(entries),
	)

	for _, e := range entries {
		var path string
		switch {
		case e.trashedAt != "":
			path = c.TrashFile(e.id)
		case e.archivedAt != "":
			path = c.TraceFile(e.id, true)
		default:
			path = c.TraceFile(e.id, false)
		}

		var body string
		tags, err := c.tagsFor(e.id)
		if err != nil {
			return fmt.Errorf("loading tags for %s: %w", e.id, err)
		}
		if t, err := trace.ParseFile(path); err == nil {
			body = t.Body
		}
		// Parse failures leave body empty — the file may have been
		// deleted out of band. Title and tags are still indexed so
		// the trace is partially searchable until `noema sync`
		// rebuilds from source.

		tx, err := c.DB.Begin()
		if err != nil {
			return fmt.Errorf("opening reindex tx for %s: %w", e.id, err)
		}
		if err := upsertFTS(tx, e.id, e.title, body, tags); err != nil {
			tx.Rollback()
			return fmt.Errorf("reindexing %s: %w", e.id, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing reindex for %s: %w", e.id, err)
		}
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

