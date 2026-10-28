package instance

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
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteReadRemove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := Info{
		PID: 4242, Address: "127.0.0.1:40123", URL: "http://127.0.0.1:40123", Version: "1.2.3", APIVersion: 1,
		StartedAt: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), TLSFingerprint: "sha256:ab",
	}
	if err := Write(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("read %+v, want %+v", got, want)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temporary file %s left behind", e.Name())
		}
	}
	if err := Remove(dir, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err != nil {
		t.Errorf("a foreign pid removed the file: %v", err)
	}
	if err := Remove(dir, want.PID); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after remove: %v", err)
	}
	if err := Remove(dir, want.PID); err != nil {
		t.Errorf("removing twice: %v", err)
	}
}

func TestWriteReplacesAtomically(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for i := range 20 {
		if err := Write(dir, Info{PID: i, Address: "a"}); err != nil {
			t.Fatal(err)
		}
		got, err := Read(dir)
		if err != nil || got.PID != i {
			t.Fatalf("round %d: %+v, %v", i, got, err)
		}
	}
}

func TestLockIsExclusiveAndReleased(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if held, err := Held(dir); err != nil || held {
		t.Fatalf("fresh dir: held=%v err=%v", held, err)
	}
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, Info{PID: 7, Address: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir); !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), "pid 7") {
		t.Errorf("second Acquire error = %v, want ErrLocked naming the holder", err)
	}
	if held, err := Held(dir); err != nil || !held {
		t.Errorf("Held while locked = %v, %v", held, err)
	}
	info, err := Discover(dir)
	if err != nil || info.PID != 7 {
		t.Errorf("Discover = %+v, %v", info, err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
	if _, err := Discover(dir); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Discover after release = %v, want ErrNotRunning (stale runtime file)", err)
	}
	again, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	_ = again.Release()
}

func TestFingerprint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dlm"}, NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Fingerprint(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	if want := "sha256:" + hex.EncodeToString(sum[:]); got != want {
		t.Errorf("fingerprint = %s, want %s", got, want)
	}
	if _, err := Fingerprint(certFile, filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing key must fail")
	}
}
