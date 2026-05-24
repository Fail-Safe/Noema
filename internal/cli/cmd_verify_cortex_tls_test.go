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

	"github.com/Fail-Safe/Noema/internal/cortex"
)

func writeCertAndKey(t *testing.T, dir string, nb, na time.Time) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "noema-verify-test"},
		NotBefore:    nb,
		NotAfter:     na,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	certPath := filepath.Join(dir, "tls.crt")
	cf, _ := os.Create(certPath)
	_ = pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	cf.Close()

	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPath := filepath.Join(dir, "tls.key")
	kf, _ := os.Create(keyPath)
	_ = pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	kf.Close()
	return certPath, keyPath
}

// writeManifestWithTLS rewrites cortex.md to carry the access TLS
// fields, preserving the ID assigned by newSandboxedCortex.
func writeManifestWithTLS(t *testing.T, dir, certPath, keyPath string) {
	t.Helper()
	m, err := cortex.ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Access == nil {
		m.Access = &cortex.AccessConfig{}
	}
	m.Access.TLSCertPath = certPath
	m.Access.TLSKeyPath = keyPath
	if err := cortex.WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
}

func TestCheckTLSCerts_NotConfigured(t *testing.T) {
	dir, _ := newSandboxedCortex(t, "tls-none")
	cx := openCortexDir(t, "tls-none", dir)
	r := checkTLSCerts(cx, time.Now())
	if r.level != checkOK {
		t.Fatalf("level = %v, want ok", r.level)
	}
	if !strings.Contains(r.summary, "no TLS configured") {
		t.Errorf("summary = %q", r.summary)
	}
}

func TestCheckTLSCerts_HappyPath(t *testing.T) {
	dir, _ := newSandboxedCortex(t, "tls-ok")
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	certPath, keyPath := writeCertAndKey(t, dir, now.AddDate(0, 0, -30), now.AddDate(1, 0, 0))
	writeManifestWithTLS(t, dir, certPath, keyPath)
	cx := openCortexDir(t, "tls-ok", dir)
	r := checkTLSCerts(cx, now)
	if r.level != checkOK {
		t.Fatalf("level = %v want ok; summary=%q", r.level, r.summary)
	}
	if !strings.Contains(r.summary, "noema-verify-test") {
		t.Errorf("expected CN in summary, got %q", r.summary)
	}
}

func TestCheckTLSCerts_Expired(t *testing.T) {
	dir, _ := newSandboxedCortex(t, "tls-expired")
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	certPath, keyPath := writeCertAndKey(t, dir, now.AddDate(0, 0, -60), now.AddDate(0, 0, -1))
	writeManifestWithTLS(t, dir, certPath, keyPath)
	cx := openCortexDir(t, "tls-expired", dir)
	r := checkTLSCerts(cx, now)
	if r.level != checkFail {
		t.Fatalf("level = %v want fail; summary=%q", r.level, r.summary)
	}
	if !strings.Contains(r.summary, "expired") {
		t.Errorf("expected 'expired' in summary, got %q", r.summary)
	}
}

func TestCheckTLSCerts_NearExpiryWarns(t *testing.T) {
	dir, _ := newSandboxedCortex(t, "tls-soon")
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	certPath, keyPath := writeCertAndKey(t, dir, now.AddDate(0, 0, -30), now.AddDate(0, 0, 3))
	writeManifestWithTLS(t, dir, certPath, keyPath)
	cx := openCortexDir(t, "tls-soon", dir)
	r := checkTLSCerts(cx, now)
	if r.level != checkWarn {
		t.Fatalf("level = %v want warn; summary=%q", r.level, r.summary)
	}
	if !strings.Contains(r.summary, "expires in") {
		t.Errorf("summary = %q", r.summary)
	}
}

func TestCheckTLSCerts_OnlyOneFieldSet(t *testing.T) {
	dir, _ := newSandboxedCortex(t, "tls-partial")
	m, err := cortex.ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	m.Access = &cortex.AccessConfig{TLSCertPath: "/tmp/cert.pem"}
	if err := cortex.WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	cx := openCortexDir(t, "tls-partial", dir)
	r := checkTLSCerts(cx, time.Now())
	if r.level != checkFail {
		t.Fatalf("level = %v want fail; summary=%q", r.level, r.summary)
	}
	// Both field names should appear, and the wording must identify
	// tls_cert_path as the one that's set and tls_key_path as the empty one.
	if !strings.Contains(r.summary, "access.tls_cert_path is set") ||
		!strings.Contains(r.summary, "access.tls_key_path is empty") {
		t.Errorf("backwards or imprecise wording: %q", r.summary)
	}
}

// TestCheckTLSCerts_OnlyKeyFieldSet pins the mirror case so the
// present/missing identification stays correct in both directions.
func TestCheckTLSCerts_OnlyKeyFieldSet(t *testing.T) {
	dir, _ := newSandboxedCortex(t, "tls-partial-k")
	m, err := cortex.ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	m.Access = &cortex.AccessConfig{TLSKeyPath: "/tmp/key.pem"}
	if err := cortex.WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	cx := openCortexDir(t, "tls-partial-k", dir)
	r := checkTLSCerts(cx, time.Now())
	if r.level != checkFail {
		t.Fatalf("level = %v want fail; summary=%q", r.level, r.summary)
	}
	if !strings.Contains(r.summary, "access.tls_key_path is set") ||
		!strings.Contains(r.summary, "access.tls_cert_path is empty") {
		t.Errorf("backwards or imprecise wording: %q", r.summary)
	}
}

// TestCheckTLSCerts_VerifyCortexBytes pins the line as it appears in
// the full doctor output, so the row name+tag stays scannable.
func TestCheckTLSCerts_VerifyCortexBytes(t *testing.T) {
	dir, cfg := newSandboxedCortex(t, "tls-row")
	cortexFlag = ""
	t.Setenv("NOEMA_CORTEX", "")
	cx := openCortexDir(t, "tls-row", dir)
	var out bytes.Buffer
	if err := runVerifyCortexFor(&out, cx, cfg, nil); err != nil {
		t.Fatalf("runVerifyCortexFor: %v", err)
	}
	if !strings.Contains(out.String(), "[ok]   tls ") {
		t.Errorf("expected '[ok]   tls' row, got:\n%s", out.String())
	}
}
