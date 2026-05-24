package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/Fail-Safe/Noema/internal/tlsutil"
)

// CertMonitor periodically re-parses the TLS leaf cert the HTTP server
// is presenting and logs a warning when the cert's expiry crosses a
// reporting band. It is a no-op safety net — the server keeps serving
// regardless of what the monitor finds, because rotating the cert
// requires file changes plus a restart. The monitor's job is to make
// sure an operator hears about it before clients start failing.
//
// Bands are emitted on transitions only (idempotent): if the cert is
// already inside the 30-day band when the monitor starts, a single
// "≤30d" line is logged once and not repeated until the band changes.
// Once expired, the monitor emits one final "expired" line per startup
// and stops re-logging until the band moves again (which it won't —
// expiry is a one-way trip).
//
// Mirrors the federation.Syncer goroutine pattern: ctx/cancel/wg with
// a single goroutine that selects on ctx.Done() and a ticker.
type CertMonitor struct {
	certPath string
	interval time.Duration
	logger   io.Writer

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	lastBnd warningBand
}

// warningBand enumerates the warning levels the monitor reports. Higher
// values are more urgent; transitions between any two distinct bands
// emit a log line.
type warningBand int

const (
	bandFresh    warningBand = iota // > 90 days
	band90                          // ≤ 90 days
	band30                          // ≤ 30 days
	band7                           // ≤ 7 days
	bandExpired                     // past NotAfter
	bandUnknown                     // cert unreadable
)

func (b warningBand) String() string {
	switch b {
	case bandFresh:
		return "fresh"
	case band90:
		return "≤90d"
	case band30:
		return "≤30d"
	case band7:
		return "≤7d"
	case bandExpired:
		return "expired"
	case bandUnknown:
		return "unknown"
	}
	return "?"
}

// CertMonitorInterval is the default re-check cadence. One hour is the
// sweet spot: tight enough that a 7-day band transition is caught the
// same operational day, loose enough that a long-running server doesn't
// spam its journal.
const CertMonitorInterval = time.Hour

// NewCertMonitor builds a monitor for the given cert path. The logger
// is where each band-transition line is written; pass os.Stderr in
// production. When logger is nil the monitor writes to os.Stderr.
func NewCertMonitor(certPath string, logger io.Writer) *CertMonitor {
	if logger == nil {
		logger = os.Stderr
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &CertMonitor{
		certPath: certPath,
		interval: CertMonitorInterval,
		logger:   logger,
		ctx:      ctx,
		cancel:   cancel,
		// Seed lastBnd to a sentinel so the first observation always
		// emits exactly one line — operators want a confirmation that
		// the monitor is alive at startup, not silence.
		lastBnd: -1,
	}
}

// Start launches the background loop. Idempotent; calling Start more
// than once is a programmer error and will panic the goroutine count.
func (m *CertMonitor) Start() {
	m.wg.Add(1)
	go m.loop()
}

// Stop cancels the loop and waits for the goroutine to exit.
func (m *CertMonitor) Stop() {
	m.cancel()
	m.wg.Wait()
}

func (m *CertMonitor) loop() {
	defer m.wg.Done()
	// First check fires immediately so the startup-time band is logged
	// without a one-hour delay. Subsequent checks tick on the interval.
	m.checkOnce(time.Now())
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-t.C:
			m.checkOnce(now)
		}
	}
}

func (m *CertMonitor) checkOnce(now time.Time) {
	band, msg := m.classify(now)
	m.mu.Lock()
	prev := m.lastBnd
	m.lastBnd = band
	m.mu.Unlock()
	if band == prev {
		return
	}
	fmt.Fprintf(m.logger, "[cert-monitor] %s: %s\n", band, msg)
}

func (m *CertMonitor) classify(now time.Time) (warningBand, string) {
	cert, err := tlsutil.LoadLeaf(m.certPath)
	if err != nil {
		return bandUnknown, fmt.Sprintf("cannot read %s: %v", m.certPath, err)
	}
	c := tlsutil.Classify(cert, now)
	switch c.Status {
	case tlsutil.StatusExpired:
		return bandExpired, fmt.Sprintf("%s expired %d day(s) ago (NotAfter=%s) — rotate the cert and restart `noema serve`",
			m.certPath, -c.DaysRemaining, c.NotAfter.UTC().Format(time.RFC3339))
	case tlsutil.StatusNotYetValid:
		return bandExpired, fmt.Sprintf("%s NotBefore is in the future (NotAfter=%s) — clock skew or wrong cert",
			m.certPath, c.NotAfter.UTC().Format(time.RFC3339))
	case tlsutil.StatusNearExpiry:
		return band7, fmt.Sprintf("%s expires in %d day(s) (NotAfter=%s)",
			m.certPath, c.DaysRemaining, c.NotAfter.UTC().Format(time.RFC3339))
	}
	// StatusOK — refine into 90/30/fresh bands.
	switch {
	case c.DaysRemaining <= 30:
		return band30, fmt.Sprintf("%s expires in %d day(s) (NotAfter=%s)",
			m.certPath, c.DaysRemaining, c.NotAfter.UTC().Format(time.RFC3339))
	case c.DaysRemaining <= 90:
		return band90, fmt.Sprintf("%s expires in %d day(s) (NotAfter=%s)",
			m.certPath, c.DaysRemaining, c.NotAfter.UTC().Format(time.RFC3339))
	default:
		return bandFresh, fmt.Sprintf("%s ok, %d days until NotAfter=%s",
			m.certPath, c.DaysRemaining, c.NotAfter.UTC().Format(time.RFC3339))
	}
}
