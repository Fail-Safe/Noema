package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/config"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/event"
)

// verifyCortexCmd implements the cortex health check ("doctor"). It is
// strictly read-only, never touches the network, and reports each
// validation as one line so the output stays scannable. Exits non-zero
// when any check is fail-level; warnings are informational only.
func verifyCortexCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cortex",
		Short: "Validate manifest, config, db, access posture, and federation",
		Long: `Runs a fast, read-only health check against the active cortex.

Each check reports as one of:
  [ok]   — passed
  [warn] — actionable, but not fatal (e.g. legacy manifest format)
  [fail] — broken or misconfigured

Exits 1 when any check is fail-level, 0 otherwise. Network checks and
deep trace-hash scans are out of scope here — use ` + "`noema verify traces`" + ` and
` + "`noema verify drift`" + ` for those.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerifyCortex(cmd.OutOrStdout())
		},
	}
}

type checkLevel int

const (
	checkOK checkLevel = iota
	checkWarn
	checkFail
)

func (l checkLevel) tag() string {
	switch l {
	case checkOK:
		return "[ok]  "
	case checkWarn:
		return "[warn]"
	case checkFail:
		return "[fail]"
	}
	return "[????]"
}

type checkResult struct {
	name    string
	level   checkLevel
	summary string
	detail  string // optional, indented under the summary
}

func runVerifyCortex(out io.Writer) error {
	cfg, cfgErr := config.Load()
	cx, err := resolveCortex()
	if err != nil {
		results := []checkResult{
			checkUserConfig(cfg, cfgErr),
			{name: "cortex selection", level: checkFail, summary: err.Error()},
		}
		writeResults(out, results)
		return errors.New("cortex doctor: fail-level checks reported")
	}
	defer cx.Close()
	return runVerifyCortexFor(out, cx, cfg, cfgErr)
}

// runVerifyCortexFor runs the doctor check list against an explicit
// cortex + config. Split out so tests can drive it without going
// through global flag/env resolution.
func runVerifyCortexFor(out io.Writer, cx *cortex.Cortex, cfg *config.Config, cfgErr error) error {
	results := []checkResult{
		checkUserConfig(cfg, cfgErr),
		checkCortexSelection(cx),
		checkCortexLayout(cx),
	}
	results = append(results, checkManifest(cx)...)
	results = append(results, checkDB(cx))
	results = append(results, checkAccess(cx))
	results = append(results, checkFederationConfig(cx)...)
	results = append(results, checkWatch(cx))
	results = append(results, checkConsolidation(cx))

	header := fmt.Sprintf("noema verify cortex — %s (%s)", cx.Name, cx.Dir)
	fmt.Fprintln(out, header)
	fmt.Fprintln(out, strings.Repeat("─", len(header)))
	writeResults(out, results)

	var ok, warn, fail int
	for _, r := range results {
		switch r.level {
		case checkOK:
			ok++
		case checkWarn:
			warn++
		case checkFail:
			fail++
		}
	}
	fmt.Fprintf(out, "\n%d ok, %d warn, %d fail\n", ok, warn, fail)
	if fail > 0 {
		return errors.New("cortex doctor: fail-level checks reported")
	}
	return nil
}

func writeResults(out io.Writer, results []checkResult) {
	width := 0
	for _, r := range results {
		if len(r.name) > width {
			width = len(r.name)
		}
	}
	for _, r := range results {
		fmt.Fprintf(out, "%s %-*s  %s\n", r.level.tag(), width, r.name, r.summary)
		if r.detail != "" {
			for line := range strings.SplitSeq(r.detail, "\n") {
				fmt.Fprintf(out, "       %s\n", line)
			}
		}
	}
}

func checkUserConfig(cfg *config.Config, loadErr error) checkResult {
	if loadErr != nil {
		return checkResult{
			name:    "user config",
			level:   checkFail,
			summary: fmt.Sprintf("could not load: %v", loadErr),
		}
	}
	if cfg.Default != "" {
		if _, ok := cfg.Cortexes[cfg.Default]; !ok {
			return checkResult{
				name:    "user config",
				level:   checkFail,
				summary: fmt.Sprintf("default %q not in registered cortexes", cfg.Default),
			}
		}
	}
	return checkResult{
		name:    "user config",
		level:   checkOK,
		summary: fmt.Sprintf("%d cortex(es) registered, default=%s", len(cfg.Cortexes), defaultDisplay(cfg.Default)),
	}
}

func defaultDisplay(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func checkCortexSelection(cx *cortex.Cortex) checkResult {
	source := "config default"
	switch {
	case cortexFlag != "":
		source = "--cortex flag"
	case os.Getenv("NOEMA_CORTEX") != "":
		source = "$NOEMA_CORTEX"
	}
	return checkResult{
		name:    "cortex selection",
		level:   checkOK,
		summary: fmt.Sprintf("%s (source: %s)", cx.Name, source),
	}
}

func checkCortexLayout(cx *cortex.Cortex) checkResult {
	required := map[string]string{
		"traces/":         cx.TracesDir(),
		"archive/traces/": cx.ArchiveDir(),
		"trash/traces/":   cx.TrashDir(),
		"db/":             filepath.Join(cx.Dir, "db"),
	}
	var missing []string
	for label, path := range required {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			missing = append(missing, label)
		}
	}
	if len(missing) > 0 {
		return checkResult{
			name:    "cortex layout",
			level:   checkFail,
			summary: "missing required dirs: " + strings.Join(missing, ", "),
		}
	}
	return checkResult{
		name:    "cortex layout",
		level:   checkOK,
		summary: "all required dirs present",
	}
}

func checkManifest(cx *cortex.Cortex) []checkResult {
	results := []checkResult{}

	manifestPath := filepath.Join(cx.Dir, "cortex.md")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return []checkResult{{
			name:    "manifest",
			level:   checkFail,
			summary: fmt.Sprintf("cannot read cortex.md: %v", err),
		}}
	}

	m, err := cortex.ReadManifest(cx.Dir)
	if err != nil {
		return []checkResult{{
			name:    "manifest",
			level:   checkFail,
			summary: fmt.Sprintf("cannot parse cortex.md: %v", err),
		}}
	}

	framed := strings.HasPrefix(string(raw), "---\n") || strings.HasPrefix(string(raw), "---\r\n")
	switch {
	case framed:
		results = append(results, checkResult{
			name:    "manifest",
			level:   checkOK,
			summary: "framed YAML frontmatter",
		})
	default:
		results = append(results, checkResult{
			name:    "manifest",
			level:   checkWarn,
			summary: "legacy bare-YAML format",
			detail:  "Will silently upgrade to framed form on the next manifest mutation.",
		})
	}

	switch {
	case m.Version == cortex.ManifestVersion:
		results = append(results, checkResult{
			name:    "manifest version",
			level:   checkOK,
			summary: fmt.Sprintf("v%d (current)", m.Version),
		})
	case m.Version < cortex.ManifestVersion:
		results = append(results, checkResult{
			name:    "manifest version",
			level:   checkFail,
			summary: fmt.Sprintf("v%d behind current v%d", m.Version, cortex.ManifestVersion),
			detail:  "Run `noema migrate cortex-id` to upgrade.",
		})
	default:
		results = append(results, checkResult{
			name:    "manifest version",
			level:   checkWarn,
			summary: fmt.Sprintf("v%d ahead of binary's v%d (newer noema?)", m.Version, cortex.ManifestVersion),
		})
	}

	switch {
	case m.ID == "":
		results = append(results, checkResult{
			name:    "manifest id",
			level:   checkFail,
			summary: "missing — required since manifest v2",
			detail:  "Run `noema migrate cortex-id` to assign one.",
		})
	case !event.IsValidULID(m.ID):
		results = append(results, checkResult{
			name:    "manifest id",
			level:   checkFail,
			summary: fmt.Sprintf("not a valid ULID: %q", m.ID),
		})
	default:
		results = append(results, checkResult{
			name:    "manifest id",
			level:   checkOK,
			summary: m.ID,
		})
	}

	if m.Name == "" {
		results = append(results, checkResult{
			name:    "manifest name",
			level:   checkFail,
			summary: "empty",
		})
	}

	if err := m.ValidateFederation(); err != nil {
		results = append(results, checkResult{
			name:    "federation schema",
			level:   checkFail,
			summary: err.Error(),
		})
	}

	return results
}

func checkDB(cx *cortex.Cortex) checkResult {
	var version int
	if err := cx.DB.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return checkResult{
			name:    "db",
			level:   checkFail,
			summary: fmt.Sprintf("schema_migrations query failed: %v", err),
		}
	}
	var journal string
	if err := cx.DB.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		return checkResult{
			name:    "db",
			level:   checkFail,
			summary: fmt.Sprintf("PRAGMA journal_mode failed: %v", err),
		}
	}
	if !strings.EqualFold(journal, "wal") {
		return checkResult{
			name:    "db",
			level:   checkWarn,
			summary: fmt.Sprintf("schema v%d, journal_mode=%s (expected wal)", version, journal),
		}
	}
	return checkResult{
		name:    "db",
		level:   checkOK,
		summary: fmt.Sprintf("schema v%d, WAL enabled", version),
	}
}

func checkAccess(cx *cortex.Cortex) checkResult {
	m, err := cortex.ReadManifest(cx.Dir)
	if err != nil {
		return checkResult{
			name:    "access",
			level:   checkFail,
			summary: fmt.Sprintf("manifest unavailable: %v", err),
		}
	}
	key, err := cortex.LoadAccessKey(cx.Dir, m.Access)
	if err != nil {
		return checkResult{
			name:    "access",
			level:   checkFail,
			summary: err.Error(),
		}
	}
	if !key.Keyed() {
		return checkResult{
			name:    "access",
			level:   checkOK,
			summary: "open mode (no shared key)",
			detail:  "Open mode is loopback-only. Set NOEMA_MCP_KEY or access.shared_key_file before exposing the HTTP transport.",
		}
	}
	detail := ""
	if key.EnvOverride() {
		detail = fmt.Sprintf("$NOEMA_MCP_KEY overrides configured shared_key_file (%s).", key.Path)
	}
	return checkResult{
		name:    "access",
		level:   checkOK,
		summary: fmt.Sprintf("keyed (source=%s, fp=%s)", key.Source, key.Fingerprint),
		detail:  detail,
	}
}

func checkFederationConfig(cx *cortex.Cortex) []checkResult {
	m, err := cortex.ReadManifest(cx.Dir)
	if err != nil {
		return []checkResult{{
			name:    "federation",
			level:   checkFail,
			summary: fmt.Sprintf("manifest unavailable: %v", err),
		}}
	}
	if m.Federation == nil || len(m.Federation.Peers) == 0 {
		return []checkResult{{
			name:    "federation",
			level:   checkOK,
			summary: "no peers configured",
		}}
	}

	results := []checkResult{}
	mode := m.Federation.EffectiveMode()
	results = append(results, checkResult{
		name:    "federation mode",
		level:   checkOK,
		summary: fmt.Sprintf("%s (%d peer(s))", mode, len(m.Federation.Peers)),
	})

	seen := map[string]int{}
	var dupes, collisions, incomplete []string
	for _, p := range m.Federation.Peers {
		seen[p.Name]++
		if seen[p.Name] == 2 {
			dupes = append(dupes, p.Name)
		}
		if m.PeerLabelCollidesWithSelf(p.Name) {
			collisions = append(collisions, p.Name)
		}
		if p.Name == "" || p.Endpoint == "" {
			incomplete = append(incomplete, fmt.Sprintf("%q@%q", p.Name, p.Endpoint))
		}
	}
	if len(dupes) > 0 {
		results = append(results, checkResult{
			name:    "federation peers",
			level:   checkFail,
			summary: "duplicate peer labels: " + strings.Join(dupes, ", "),
		})
	}
	if len(collisions) > 0 {
		results = append(results, checkResult{
			name:    "federation peers",
			level:   checkFail,
			summary: "peer label(s) collide with this cortex's name: " + strings.Join(collisions, ", "),
		})
	}
	if len(incomplete) > 0 {
		results = append(results, checkResult{
			name:    "federation peers",
			level:   checkFail,
			summary: "peer entries missing name or endpoint: " + strings.Join(incomplete, ", "),
		})
	}
	return results
}

func checkWatch(cx *cortex.Cortex) checkResult {
	m, err := cortex.ReadManifest(cx.Dir)
	if err != nil {
		return checkResult{
			name:    "watch",
			level:   checkFail,
			summary: fmt.Sprintf("manifest unavailable: %v", err),
		}
	}
	if m.Watch == nil {
		return checkResult{
			name:    "watch",
			level:   checkOK,
			summary: "default settings (enabled, debounce 300ms)",
		}
	}
	debounce := m.Watch.DebounceMs
	enabled := m.Watch.Enabled == nil || *m.Watch.Enabled
	switch {
	case debounce != 0 && (debounce < 50 || debounce > 10000):
		return checkResult{
			name:    "watch",
			level:   checkWarn,
			summary: fmt.Sprintf("enabled=%t, debounce_ms=%d (outside sane range 50–10000)", enabled, debounce),
		}
	default:
		return checkResult{
			name:    "watch",
			level:   checkOK,
			summary: fmt.Sprintf("enabled=%t, debounce_ms=%d", enabled, watchDebounceDisplay(debounce)),
		}
	}
}

func watchDebounceDisplay(d int) int {
	if d == 0 {
		return 300
	}
	return d
}

// checkConsolidation validates the consolidation config block. The
// feature ships off by default — a missing block is OK. When present
// with LLMEnabled, the LocalLLMEndpoint must parse as a URL and must
// not be empty. Graduation thresholds, if set, must be non-negative.
func checkConsolidation(cx *cortex.Cortex) checkResult {
	m, err := cortex.ReadManifest(cx.Dir)
	if err != nil {
		return checkResult{
			name:    "consolidation",
			level:   checkFail,
			summary: fmt.Sprintf("manifest unavailable: %v", err),
		}
	}
	if m.Consolidation == nil {
		return checkResult{
			name:    "consolidation",
			level:   checkOK,
			summary: "not configured",
		}
	}
	c := m.Consolidation
	if c.LLMEnabled && c.LocalLLMEndpoint == "" {
		return checkResult{
			name:    "consolidation",
			level:   checkWarn,
			summary: "llm_enabled but local_llm_endpoint is empty",
		}
	}
	if c.LLMEnabled && c.LocalLLMEndpoint != "" {
		if _, err := url.Parse(c.LocalLLMEndpoint); err != nil {
			return checkResult{
				name:    "consolidation",
				level:   checkFail,
				summary: fmt.Sprintf("local_llm_endpoint not a valid URL: %v", err),
			}
		}
	}
	if c.Graduation != nil {
		g := c.Graduation
		if g.MinAgeDays < 0 || g.MinReadCount < 0 {
			return checkResult{
				name:    "consolidation",
				level:   checkFail,
				summary: "graduation thresholds must be non-negative",
			}
		}
	}
	return checkResult{
		name:    "consolidation",
		level:   checkOK,
		summary: fmt.Sprintf("enabled=%t, llm_enabled=%t", c.Enabled, c.LLMEnabled),
	}
}

