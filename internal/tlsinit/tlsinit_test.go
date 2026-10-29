package tlsinit

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/instance"
)

func TestGenerateMakesAUsableSelfSignedCertificate(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	r, err := Generate(Options{Hosts: []string{"nas.local", "192.168.1.10", "localhost"}, Days: 30, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Names, []string{"localhost", "127.0.0.1", "::1", "nas.local", "192.168.1.10"}) {
		t.Errorf("names = %v", r.Names)
	}
	pair, err := tls.X509KeyPair(r.CertPEM, r.KeyPEM)
	if err != nil {
		t.Fatalf("the pair does not load: %v", err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if !cert.NotAfter.Equal(now.AddDate(0, 0, 30)) || !cert.NotBefore.Before(now) {
		t.Errorf("validity %v .. %v", cert.NotBefore, cert.NotAfter)
	}
	for _, host := range []string{"nas.local", "localhost", "192.168.1.10", "127.0.0.1"} {
		if err := cert.VerifyHostname(host); err != nil {
			t.Errorf("not valid for %s: %v", host, err)
		}
	}
	if err := cert.VerifyHostname("evil.example"); err == nil {
		t.Error("valid for a host that was not asked for")
	}
	// A client that trusts exactly this certificate accepts it (pinning).
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "nas.local", CurrentTime: now}); err != nil {
		t.Errorf("not verifiable as its own root: %v", err)
	}
}

func TestFingerprintMatchesWhatTheDaemonReports(t *testing.T) {
	r, err := Generate(Options{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls", "cert.pem"), filepath.Join(dir, "tls", "key.pem")
	if err := WriteFiles(certPath, keyPath, r, false); err != nil {
		t.Fatal(err)
	}
	got, err := instance.Fingerprint(certPath, keyPath)
	if err != nil || got != r.Fingerprint {
		t.Errorf("daemon fingerprint %q (%v), generated %q", got, err, r.Fingerprint)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(keyPath); fi.Mode().Perm() != 0o600 {
			t.Errorf("key mode = %v", fi.Mode())
		}
	}
	if r.NotAfter.Before(time.Now().AddDate(2, 0, 0)) {
		t.Errorf("default validity ends %v", r.NotAfter)
	}
}

func TestWriteFilesRefusesToOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	first, _ := Generate(Options{})
	if err := WriteFiles(certPath, keyPath, first, false); err != nil {
		t.Fatal(err)
	}
	second, _ := Generate(Options{})
	if err := WriteFiles(certPath, keyPath, second, false); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	if data, _ := os.ReadFile(certPath); string(data) != string(first.CertPEM) {
		t.Error("a refused write changed the certificate")
	}
	if err := WriteFiles(certPath, keyPath, second, true); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(certPath); string(data) != string(second.CertPEM) {
		t.Error("--force did not replace the certificate")
	}
}
