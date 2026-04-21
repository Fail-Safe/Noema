// Package watch turns external edits to trace files (Obsidian, VS Code,
// Finder drags, iCloud sync, etc.) into first-class mutation events. It
// runs as a goroutine under `noema serve` on both the stdio and http
// transports, watching the cortex's traces/, archive/traces/, and
// trash/traces/ directories with fsnotify and dispatching to existing
// Cortex mutation methods so events, vector clocks, and the SQL write
// path are shared with MCP-initiated mutations. Federation propagation
// is still HTTP-only (peers need a network endpoint), but external-edit
// events land in the local log under stdio too and flow outward the next
// time an HTTP serve runs on this cortex.
package watch

import (
	"context"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

// Watcher observes the three trace directories and reconciles external
// filesystem changes into Cortex mutation events. Lifecycle mirrors
// federation.Syncer: construct, Start, Stop (cancels context and waits for
// the goroutine to drain).
type Watcher struct {
	cx          *cortex.Cortex
	debounce    time.Duration
	autoOnboard bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	fsn *fsnotify.Watcher

	mu      sync.Mutex
	pending map[string]*time.Timer

	// lastSkipErr dedupes skip-and-log messages for the same file —
	// external sync loops (iCloud, Obsidian reload) fire several
	// watcher events per user action, and logging the identical
	// error 5-10 times per file is noise. A new error string for
	// the same path is always logged.
	lastSkipErr map[string]string
}

// New creates a Watcher for the given cortex. An empty cfg is equivalent
// to the defaults (enabled=true, debounce=300ms); callers that want to
// gate on the enabled bit should check cfg.WatchEnabled() before calling.
func New(cx *cortex.Cortex, cfg *cortex.WatchConfig) (*Watcher, error) {
	fsn, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Watcher{
		cx:          cx,
		debounce:    cfg.EffectiveDebounce(),
		autoOnboard: cfg.AutoOnboardEnabled(),
		ctx:         ctx,
		cancel:      cancel,
		fsn:         fsn,
		pending:     make(map[string]*time.Timer),
		lastSkipErr: make(map[string]string),
	}, nil
}

// logSkip records and throttles a skip-and-log event. A file that keeps
// producing the same error (common with iCloud sync or a Web Clipper
// re-write loop) is logged once; the next distinct error for the same
// path is logged afresh. Keeps the skip log honest without flooding.
func (w *Watcher) logSkip(path, msg string) {
	w.mu.Lock()
	prev, seen := w.lastSkipErr[path]
	w.lastSkipErr[path] = msg
	w.mu.Unlock()
	if seen && prev == msg {
		return
	}
	log.Printf("[watch] skipping %s: %s", path, msg)
}

// forgetSkip drops the throttle entry for path. Call when a path
// transitions from skipped to successfully handled so the next skip
// (if any) is logged even if the message matches an older one.
func (w *Watcher) forgetSkip(path string) {
	w.mu.Lock()
	delete(w.lastSkipErr, path)
	w.mu.Unlock()
}

// Start registers the three trace directories with fsnotify and spawns the
// event-loop goroutine. Returns an error only if adding a directory fails
// (e.g. permission denied); transient fsnotify errors during runtime are
// logged, not surfaced.
func (w *Watcher) Start() error {
	dirs := []string{w.cx.TracesDir(), w.cx.ArchiveDir(), w.cx.TrashDir()}
	for _, d := range dirs {
		if err := w.fsn.Add(d); err != nil {
			w.fsn.Close()
			return err
		}
	}
	log.Printf("[watch] active, debounce=%s, dirs=[traces archive trash]", w.debounce)
	w.wg.Add(1)
	go w.run()
	return nil
}

// Stop cancels the context, closes fsnotify, and blocks until the run
// goroutine exits. Pending debounce timers are cancelled so no reconcile
// fires after Stop returns.
func (w *Watcher) Stop() {
	w.cancel()
	_ = w.fsn.Close()
	w.mu.Lock()
	for _, t := range w.pending {
		t.Stop()
	}
	w.pending = map[string]*time.Timer{}
	w.mu.Unlock()
	w.wg.Wait()
}

// run consumes fsnotify events and errors until the context is cancelled
// or the fsnotify channels close. It never panics — transient errors are
// logged and the loop continues. Exit happens only via Stop.
func (w *Watcher) run() {
	defer w.wg.Done()
	for {
		select {
		case <-w.ctx.Done():
			return
		case ev, ok := <-w.fsn.Events:
			if !ok {
				return
			}
			if !isTraceFile(ev.Name) {
				continue
			}
			w.schedule(ev.Name)
		case err, ok := <-w.fsn.Errors:
			if !ok {
				return
			}
			// Don't crash on transient errors (permission blips,
			// watched-dir recreated). Logging is enough — the watcher
			// stays up and catches the next event.
			log.Printf("[watch] fsnotify error: %v", err)
		}
	}
}

// schedule records the most recent event time for a path and (re)starts
// the per-path debounce timer. Rapid editor-triggered event bursts
// collapse into a single reconcile call at the tail of the burst.
func (w *Watcher) schedule(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.pending[path]; ok {
		t.Stop()
	}
	w.pending[path] = time.AfterFunc(w.debounce, func() {
		w.mu.Lock()
		delete(w.pending, path)
		w.mu.Unlock()
		// Reconcile outside the map lock so a long-running reconcile
		// doesn't block subsequent event scheduling.
		if err := w.reconcile(path); err != nil {
			log.Printf("[watch] reconcile %s: %v", path, err)
		}
	})
}

// isTraceFile returns true for paths that look like trace markdown files.
// fsnotify fires on every file in a watched directory — SQLite sidecars,
// editor swap files, .DS_Store, etc. — so cheap filtering here keeps the
// hot path out of parse / hash code.
func isTraceFile(path string) bool {
	base := filepath.Base(path)
	if len(base) == 0 || base[0] == '.' {
		return false
	}
	return filepath.Ext(base) == ".md"
}
