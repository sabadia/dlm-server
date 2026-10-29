// Package tlsinit creates the self-signed certificate a daemon needs to be
// reachable from another machine (server.allow_remote requires TLS). Clients
// pin the certificate by the fingerprint printed here, so no certificate
// authority is involved.
package tlsinit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// DefaultDays is how long a generated certificate is valid.
const DefaultDays = 825

// Options describe the certificate.
type Options struct {
	// Hosts are the names and IP addresses clients connect to; they become
	// the certificate's subject alternative names. localhost, 127.0.0.1 and
	// ::1 are always included.
	Hosts []string
	// Days is the validity; <= 0 means DefaultDays.
	Days int
	// Now is the start of the validity; zero means the current time.
	Now time.Time
}

// Result is a generated certificate and key in PEM form.
type Result struct {
	CertPEM, KeyPEM []byte
	// Fingerprint is "sha256:<hex>" of the certificate, the form clients pin.
	Fingerprint string
	NotAfter    time.Time
	// Names are the subject alternative names, as listed in the certificate.
	Names []string
}

// Generate creates an ECDSA P-256 self-signed certificate.
func Generate(o Options) (*Result, error) {
	days := o.Days
	if days <= 0 {
		days = DefaultDays
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("tlsinit: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("tlsinit: serial number: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "dlm", Organization: []string{"dlm"}},
		NotBefore:             now.Add(-time.Hour), // tolerate a client clock a little behind
		NotAfter:              now.AddDate(0, 0, days),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	seen := map[string]bool{}
	var names []string
	for _, h := range append([]string{"localhost", "127.0.0.1", "::1"}, o.Hosts...) {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		names = append(names, h)
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("tlsinit: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("tlsinit: encode key: %w", err)
	}
	sum := sha256.Sum256(der)
	return &Result{
		CertPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:      pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		Fingerprint: "sha256:" + hex.EncodeToString(sum[:]),
		NotAfter:    tmpl.NotAfter.UTC(),
		Names:       names,
	}, nil
}

// ErrExists means a file would be overwritten.
var ErrExists = errors.New("tlsinit: the file already exists")

// WriteFiles writes the certificate (0644) and the key (0600) next to each
// other, creating the directories. Without force an existing file is an
// error and nothing is written.
func WriteFiles(certPath, keyPath string, r *Result, force bool) error {
	if !force {
		for _, p := range []string{certPath, keyPath} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%w: %s (use --force to replace it)", ErrExists, p)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("tlsinit: %w", err)
			}
		}
	}
	for _, p := range []string{certPath, keyPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return fmt.Errorf("tlsinit: create %q: %w", filepath.Dir(p), err)
		}
	}
	if err := os.WriteFile(keyPath, r.KeyPEM, 0o600); err != nil {
		return fmt.Errorf("tlsinit: write key: %w", err)
	}
	if err := os.Chmod(keyPath, 0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return fmt.Errorf("tlsinit: protect key: %w", err)
	}
	if err := os.WriteFile(certPath, r.CertPEM, 0o644); err != nil { //nolint:gosec // a certificate is public
		return fmt.Errorf("tlsinit: write certificate: %w", err)
	}
	return nil
}
