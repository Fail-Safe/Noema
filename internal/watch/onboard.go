package watch

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Fail-Safe/Noema/internal/trace"
)

// autoOnboard rescues a non-conforming file dropped into traces/ by an
// external tool (Obsidian Web Clipper, Drafts, shortcut macros, a raw
// `cp` from another note) that didn't follow Noema's filename-ID
// convention. It synthesises valid frontmatter, generates a proper
// ID from the best available title signal, and writes a new file at
// the canonical path. The original file is removed afterwards so
// Obsidian / iCloud / the editor all converge on the renamed copy.
//
// Returns the new path and the onboarded trace on success. The
// caller — the watcher's reconcile path — then hands the trace to
// Cortex.Add as if a well-formed external create had happened.
// Returns an error for files that can't be rescued (empty, binary-
// looking, unreadable); callers should log the error and move on.
func (w *Watcher) onboardFile(path string) (string, *trace.Trace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return "", nil, fmt.Errorf("file is empty")
	}
	if !looksLikeText(data) {
		return "", nil, fmt.Errorf("file does not look like UTF-8 text")
	}

	// Preserve whatever fields a partial parse can salvage. Parse
	// fails cleanly on "no frontmatter delimiter" — in that case we
	// treat the whole file as body.
	var (
		title, body, traceType, author string
		tags                           []string
	)
	if partial, err := trace.Parse(data); err == nil && partial != nil {
		title = strings.TrimSpace(partial.Title)
		body = partial.Body
		traceType = partial.Type
		author = partial.Author
		tags = partial.Tags
	} else {
		body = string(data)
	}

	// Title source priority: existing frontmatter title > first '#'
	// heading in the body > filename stem with any trailing
	// timestamp suffix stripped > literal "Untitled" as a last resort.
	if title == "" {
		title = extractH1(body)
	}
	if title == "" {
		title = cleanFilenameStem(filepath.Base(path))
	}
	if title == "" {
		title = "Untitled"
	}

	// Type defaults to 'note'. If the partial had a recognised type,
	// keep it; otherwise overwrite. Web-clipped reference material
	// could plausibly be 'context', but picking automatically is
	// lossy either way and users can edit later.
	if !trace.IsValidType(traceType) {
		traceType = string(trace.TypeNote)
	}

	// Provenance banner so the user can tell the file was adopted,
	// not authored under the Noema convention. Original filename is
	// preserved in the banner for searchability.
	banner := fmt.Sprintf("> Auto-onboarded from `%s`\n\n", filepath.Base(path))
	body = banner + strings.TrimLeft(body, "\n")

	t := trace.New(title, traceType, author, tags, body)
	newPath := w.cx.TraceFile(t.ID, false)

	// Avoid overwriting an existing trace at the generated path. A
	// title collision in the same second is rare but real (NewID is
	// deterministic per-title-per-day), and overwriting would lose
	// a legitimate prior trace. Decline instead; next event will
	// try again with the new title if the user renamed, or a human
	// can resolve manually.
	if _, err := os.Stat(newPath); err == nil && newPath != path {
		return "", nil, fmt.Errorf("target path already exists: %s", newPath)
	}

	if err := t.Write(newPath); err != nil {
		return "", nil, fmt.Errorf("write rescued trace: %w", err)
	}

	// Remove the original only when it's actually a different path;
	// the filename-equals-ID case can slip through when the user's
	// filename happened to be slug-shaped already, in which case
	// Write already updated the file in place.
	if newPath != path {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			// Best-effort — leave the stale file in place and log.
			// The cortex still has the rescued copy so the user
			// sees their content land as a real trace.
			return newPath, t, fmt.Errorf("remove original after rescue: %w", err)
		}
	}
	return newPath, t, nil
}

// extractH1 returns the first Markdown level-1 heading in body, or ""
// if none is present. Only the first '# ' line is considered — later
// headings aren't title material. Trailing '#' characters and
// surrounding whitespace are trimmed.
func extractH1(body string) string {
	for _, line := range strings.Split(body, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "# ") {
			return strings.TrimSpace(strings.TrimRight(s[2:], "#"))
		}
	}
	return ""
}

// timestampSuffix matches the " - YYYY-MM-DDTHHMMSS±NNNN" tail that
// Obsidian Web Clipper appends to clipped titles by default, plus the
// handful of other ISO-ish shapes common in export filenames. Matching
// the suffix lets us recover the human-readable title from the
// filename rather than slugifying the whole thing (including the
// noisy timestamp).
var timestampSuffix = regexp.MustCompile(`(?i)\s*[-_]\s*\d{4}-\d{2}-\d{2}(?:[T ]\d{2}[:\-]?\d{2}[:\-]?\d{2}(?:[.,]\d+)?(?:[+\-]\d{2}:?\d{2}|Z)?)?$`)

// cleanFilenameStem strips the ".md" extension and any trailing ISO-
// style timestamp suffix from a filename, returning the best human-
// readable title candidate. Returns empty string if nothing useful
// remains after trimming.
func cleanFilenameStem(base string) string {
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	stem = timestampSuffix.ReplaceAllString(stem, "")
	return strings.TrimSpace(stem)
}

// looksLikeText is a cheap binary-file filter: no NUL bytes in the
// first 8KB. Auto-onboarding a PDF or image as a "note" would produce
// garbage frontmatter and a useless trace.
func looksLikeText(data []byte) bool {
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	return !bytes.ContainsRune(head, 0)
}
