// Package embed runs a background maintainer that keeps a cortex's semantic
// embedding index fresh under `noema serve`. Traces created or edited while
// the server runs get embedded automatically, without a manual `noema
// embeddings backfill`.
//
// It is deliberately NOT a per-mutation hook: embedding an external endpoint
// must never block or fail a trace write. Instead the maintainer runs an
// idempotent backfill pass on an interval (only missing/stale traces are
// embedded), mirroring the lifecycle of the filesystem watcher and
// federation syncer — construct, Start, Stop. Freshness is eventual, bounded
// by the interval.
package embed

import (
	"context"
	"log"
	"sync"
	"time"
)

// Maintainer periodically runs a backfill pass. The pass function is
// injected (the serve layer wires it to Cortex.EmbedBackfill) so the loop
// stays free of cortex/embedder dependencies and is trivially testable.
type Maintainer struct {
	backfill func(context.Context) (int, error)
	interval time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New builds a Maintainer that runs backfill every interval. backfill
// returns the number of traces embedded and an error; both are for logging
// only — a failing pass is retried on the next tick.
func New(interval time.Duration, backfill func(context.Context) (int, error)) *Maintainer {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Maintainer{backfill: backfill, interval: interval, ctx: ctx, cancel: cancel}
}

// Start launches the background loop. It runs one pass immediately (so a
// freshly-started server catches up on anything written while it was down),
// then once per interval until Stop.
func (m *Maintainer) Start() {
	log.Printf("[embed] maintainer active, interval=%s", m.interval)
	m.wg.Add(1)
	go m.run()
}

func (m *Maintainer) run() {
	defer m.wg.Done()
	m.pass()
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.pass()
		}
	}
}

func (m *Maintainer) pass() {
	n, err := m.backfill(m.ctx)
	switch {
	case m.ctx.Err() != nil:
		return // shutting down — swallow any partial-pass error
	case err != nil:
		log.Printf("[embed] backfill pass failed: %v", err)
	case n > 0:
		log.Printf("[embed] embedded %d trace(s)", n)
	}
}

// Stop cancels the loop and blocks until the goroutine drains.
func (m *Maintainer) Stop() {
	m.cancel()
	m.wg.Wait()
}
