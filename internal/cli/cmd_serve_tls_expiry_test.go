package cli

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

func writeTestCert(t *testing.T, nb, na time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "noema-test"},
		NotBefore:    nb,
		NotAfter:     na,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	path := filepath.Join(t.TempDir(), "cert.pem")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("pem encode: %v", err)
	}
	return path
}

func TestGateTLSExpiry_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	path := writeTestCert(t, now.AddDate(0, 0, -30), now.AddDate(1, 0, 0))
	var buf bytes.Buffer
	if err := gateTLSExpiry(path, false, now, &buf); err != nil {
		t.Fatalf("happy path returned error: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no warning, got: %q", buf.String())
	}
}

func TestGateTLSExpiry_NearExpiryWarns(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	path := writeTestCert(t, now.AddDate(0, 0, -30), now.AddDate(0, 0, 3))
	var buf bytes.Buffer
	if err := gateTLSExpiry(path, false, now, &buf); err != nil {
		t.Fatalf("near-expiry returned error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "rotate soon") {
		t.Errorf("expected warning with 'rotate soon', got: %q", out)
	}
}

func TestGateTLSExpiry_ExpiredRefuses(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	path := writeTestCert(t, now.AddDate(0, 0, -60), now.AddDate(0, 0, -1))
	var buf bytes.Buffer
	err := gateTLSExpiry(path, false, now, &buf)
	if err == nil {
		t.Fatal("expected refusal on expired cert")
	}
	if !strings.Contains(err.Error(), "refusing to start") || !strings.Contains(err.Error(), "expired") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGateTLSExpiry_ExpiredAllowedWithEscapeHatch(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	path := writeTestCert(t, now.AddDate(0, 0, -60), now.AddDate(0, 0, -1))
	var buf bytes.Buffer
	if err := gateTLSExpiry(path, true, now, &buf); err != nil {
		t.Fatalf("--insecure-allow-expired should bypass refusal: %v", err)
	}
	if !strings.Contains(buf.String(), "WARN --insecure-allow-expired") {
		t.Errorf("expected loud bypass warning, got: %q", buf.String())
	}
}

func TestGateTLSExpiry_NotYetValid(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	path := writeTestCert(t, now.AddDate(0, 0, 5), now.AddDate(0, 0, 30))
	var buf bytes.Buffer
	if err := gateTLSExpiry(path, false, now, &buf); err == nil {
		t.Fatal("expected refusal on not-yet-valid cert")
	}
}

func TestGateTLSExpiry_UnreadableCert(t *testing.T) {
	err := gateTLSExpiry(filepath.Join(t.TempDir(), "missing.pem"), false, time.Now(), &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for missing cert file")
	}
	if !strings.Contains(err.Error(), "cannot read TLS certificate") {
		t.Errorf("unexpected error wording: %v", err)
	}
}
