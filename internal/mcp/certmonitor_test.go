package mcp

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCert(t *testing.T, dir string, nb, na time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "noema-test"},
		NotBefore:    nb,
		NotAfter:     na,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	p := filepath.Join(dir, "cert.pem")
	f, _ := os.Create(p)
	defer f.Close()
	_ = pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	return p
}

func TestCertMonitor_ClassifyBands(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		na   time.Time
		want warningBand
	}{
		{"fresh", now.AddDate(1, 0, 0), bandFresh},
		{"band90", now.AddDate(0, 0, 60), band90},
		{"band30", now.AddDate(0, 0, 20), band30},
		{"band7", now.AddDate(0, 0, 3), band7},
		{"expired", now.AddDate(0, 0, -1), bandExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeCert(t, t.TempDir(), now.AddDate(0, 0, -30), tc.na)
			m := NewCertMonitor(path, nil)
			band, _ := m.classify(now)
			if band != tc.want {
				t.Fatalf("band = %v, want %v", band, tc.want)
			}
		})
	}
}

func TestCertMonitor_UnreadableCert(t *testing.T) {
	m := NewCertMonitor(filepath.Join(t.TempDir(), "missing.pem"), nil)
	band, msg := m.classify(time.Now())
	if band != bandUnknown {
		t.Fatalf("band = %v, want unknown", band)
	}
	if !strings.Contains(msg, "cannot read") {
		t.Fatalf("expected 'cannot read' in msg, got %q", msg)
	}
}

func TestCertMonitor_LogsOnlyOnBandChange(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	path := writeCert(t, dir, now.AddDate(0, 0, -30), now.AddDate(0, 0, 60))
	var buf bytes.Buffer
	m := NewCertMonitor(path, &buf)
	m.checkOnce(now)        // first observation: band90 → logged
	first := buf.Len()
	m.checkOnce(now)        // same band → no new line
	second := buf.Len()
	m.checkOnce(now.AddDate(0, 0, 40)) // bumps into band30
	third := buf.Len()
	if first == 0 {
		t.Fatal("expected first observation to log")
	}
	if second != first {
		t.Fatalf("expected no log on second observation, got %d -> %d", first, second)
	}
	if third <= second {
		t.Fatalf("expected log on band transition, got %d -> %d", second, third)
	}
	if !strings.Contains(buf.String(), "≤90d") {
		t.Errorf("expected ≤90d band in log, got: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "≤30d") {
		t.Errorf("expected ≤30d band in log, got: %q", buf.String())
	}
}
