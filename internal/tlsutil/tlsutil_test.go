package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCertPEM(t *testing.T, dir string, notBefore, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "noema-test"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	path := filepath.Join(dir, "cert.pem")
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

func TestLoadLeaf_RoundTrip(t *testing.T) {
	now := time.Now()
	path := writeCertPEM(t, t.TempDir(), now.Add(-time.Hour), now.Add(24*time.Hour))
	cert, err := LoadLeaf(path)
	if err != nil {
		t.Fatalf("LoadLeaf: %v", err)
	}
	if cert.Subject.CommonName != "noema-test" {
		t.Fatalf("unexpected CN: %q", cert.Subject.CommonName)
	}
}

func TestLoadLeaf_MissingFile(t *testing.T) {
	_, err := LoadLeaf(filepath.Join(t.TempDir(), "nope.pem"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadLeaf_EmptyPath(t *testing.T) {
	if _, err := LoadLeaf(""); err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestLoadLeaf_NoCertBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(path, []byte("-----BEGIN RSA PRIVATE KEY-----\nMIIB\n-----END RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadLeaf(path); err == nil {
		t.Fatal("expected error when no CERTIFICATE block present")
	}
}

func TestLoadLeaf_SkipsNonCertBlocksToReachCert(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// Write a key block, then a cert block, in one file. LoadLeaf
	// should skip the key and return the cert.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "chained"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
	}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	path := filepath.Join(dir, "bundle.pem")
	f, _ := os.Create(path)
	defer f.Close()
	_ = pem.Encode(f, &pem.Block{Type: "EC PARAMETERS", Bytes: []byte{0x06, 0x08}})
	_ = pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	cert, err := LoadLeaf(path)
	if err != nil {
		t.Fatalf("LoadLeaf: %v", err)
	}
	if cert.Subject.CommonName != "chained" {
		t.Fatalf("got CN %q", cert.Subject.CommonName)
	}
}

func TestClassify(t *testing.T) {
	ref := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		notBefore time.Time
		notAfter  time.Time
		want      ExpiryStatus
		wantDays  int
	}{
		{"valid_far_future", ref.AddDate(0, 0, -1), ref.AddDate(1, 0, 0), StatusOK, 365},
		{"valid_just_outside_window", ref.AddDate(0, 0, -1), ref.Add(NearExpiryWindow + time.Hour), StatusOK, 7},
		{"near_expiry_inside_window", ref.AddDate(0, 0, -1), ref.Add(3 * 24 * time.Hour), StatusNearExpiry, 3},
		{"near_expiry_on_boundary", ref.AddDate(0, 0, -1), ref.Add(NearExpiryWindow), StatusNearExpiry, 7},
		{"expired_one_hour", ref.AddDate(0, 0, -2), ref.Add(-time.Hour), StatusExpired, 0},
		{"expired_long_ago", ref.AddDate(-1, 0, 0), ref.AddDate(0, 0, -30), StatusExpired, -30},
		{"not_yet_valid", ref.AddDate(0, 0, 5), ref.AddDate(0, 0, 10), StatusNotYetValid, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cert := &x509.Certificate{NotBefore: tc.notBefore, NotAfter: tc.notAfter}
			got := Classify(cert, ref)
			if got.Status != tc.want {
				t.Errorf("status = %v, want %v", got.Status, tc.want)
			}
			if got.DaysRemaining != tc.wantDays {
				t.Errorf("days = %d, want %d", got.DaysRemaining, tc.wantDays)
			}
			if !got.NotAfter.Equal(tc.notAfter) {
				t.Errorf("NotAfter = %v, want %v", got.NotAfter, tc.notAfter)
			}
		})
	}
}
