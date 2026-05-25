// Package tlsutil parses PEM-encoded X.509 certificates and classifies
// their expiry state. It is used by `noema serve` to refuse startup on
// an already-expired cert, by `noema verify cortex` to surface upcoming
// expiry, and by the periodic cert monitor that runs alongside the HTTP
// MCP server.
//
// No I/O beyond reading the cert file. No network calls. Time is
// injectable so tests don't need to backdate certs.
package tlsutil

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"os"
	"time"
)

// ExpiryStatus is the classification bucket for a leaf certificate
// relative to a reference time.
type ExpiryStatus int

const (
	// StatusOK means the cert is currently valid and not within any
	// warning band. NotBefore <= now < NotAfter, with NotAfter strictly
	// more than 7 days away.
	StatusOK ExpiryStatus = iota

	// StatusNearExpiry means the cert is still valid but expires within
	// 7 days. Serve continues; a warning is logged.
	StatusNearExpiry

	// StatusExpired means now >= NotAfter. Serve refuses to start
	// unless the operator passes --insecure-allow-expired.
	StatusExpired

	// StatusNotYetValid means now < NotBefore. Treated as a hard failure
	// in the same bucket as expired: a client cannot use this cert.
	StatusNotYetValid
)

// String returns a short tag suitable for logs.
func (s ExpiryStatus) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusNearExpiry:
		return "near-expiry"
	case StatusExpired:
		return "expired"
	case StatusNotYetValid:
		return "not-yet-valid"
	}
	return "unknown"
}

// NearExpiryWindow is the threshold at which Classify flips from
// StatusOK to StatusNearExpiry. The cert monitor uses a finer-grained
// banded warning schedule (90/30/7/expired) but the startup gate only
// distinguishes "fine" from "warn loudly."
const NearExpiryWindow = 7 * 24 * time.Hour

// LoadLeaf reads the PEM file at path and returns the first
// CERTIFICATE block parsed as an *x509.Certificate. A file containing
// a chain returns the leaf (which by Go and most tooling conventions
// is the first block); intermediates and roots after it are ignored
// because the local server doesn't need to validate them — we only
// care about expiry of the cert it serves.
func LoadLeaf(path string) (*x509.Certificate, error) {
	if path == "" {
		return nil, errors.New("tlsutil: empty cert path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tlsutil: reading %s: %w", path, err)
	}
	for {
		block, rest := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("tlsutil: no CERTIFICATE block in %s", path)
		}
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("tlsutil: parsing certificate from %s: %w", path, err)
			}
			return cert, nil
		}
		data = rest
	}
}

// Classification is the result of Classify.
type Classification struct {
	Status   ExpiryStatus
	NotAfter time.Time
	// DaysRemaining is rounded down. Negative when the cert is already
	// expired. Zero when StatusNotYetValid (the field is meaningful
	// only against NotAfter).
	DaysRemaining int
}

// Classify buckets a cert against the reference time `now`. Pass
// time.Now() in production and a fixed value in tests.
func Classify(cert *x509.Certificate, now time.Time) Classification {
	c := Classification{NotAfter: cert.NotAfter}
	switch {
	case now.Before(cert.NotBefore):
		c.Status = StatusNotYetValid
	case !now.Before(cert.NotAfter):
		c.Status = StatusExpired
		c.DaysRemaining = daysBetween(cert.NotAfter, now) * -1
	case cert.NotAfter.Sub(now) <= NearExpiryWindow:
		c.Status = StatusNearExpiry
		c.DaysRemaining = daysBetween(now, cert.NotAfter)
	default:
		c.Status = StatusOK
		c.DaysRemaining = daysBetween(now, cert.NotAfter)
	}
	return c
}

func daysBetween(a, b time.Time) int {
	d := b.Sub(a).Hours() / 24
	if d < 0 {
		d = -d
	}
	return int(math.Floor(d))
}
