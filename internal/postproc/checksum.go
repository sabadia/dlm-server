package postproc

import (
	"context"
	"crypto/md5"  //nolint:gosec // md5 is offered because sites publish it; it is not used for security
	"crypto/sha1" //nolint:gosec // sha1 is offered because sites publish it; it is not used for security
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"

	"lukechampine.com/blake3"

	"github.com/sabadia/dlm-server/internal/settings"
)

// ErrNotAFile is returned when a checksum is asked for something that is not a
// regular file (a torrent with several files has no single checksum).
var ErrNotAFile = errors.New("a checksum can only be verified for a single file")

// newHash returns the hash of an algorithm.
func newHash(a settings.ChecksumAlgorithm) (hash.Hash, error) {
	switch a {
	case settings.ChecksumMD5:
		return md5.New(), nil //nolint:gosec // see the import
	case settings.ChecksumSHA1:
		return sha1.New(), nil //nolint:gosec // see the import
	case settings.ChecksumSHA256:
		return sha256.New(), nil
	case settings.ChecksumBLAKE3:
		return blake3.New(32, nil), nil
	default:
		return nil, fmt.Errorf("unknown checksum algorithm %q", a)
	}
}

// ChecksumOf returns the lower-case hex digest of the file at path. It stops
// with ctx.
func ChecksumOf(ctx context.Context, path string, a settings.ChecksumAlgorithm) (string, error) {
	h, err := newHash(a)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path) //nolint:gosec // the task's own file
	if err != nil {
		return "", fmt.Errorf("open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %q", ErrNotAFile, path)
	}
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", err //nolint:wrapcheck // cancellation is reported as is
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n]) // hash.Hash.Write never fails
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return "", fmt.Errorf("read %q: %w", path, rerr)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Checksum verifies the downloaded file against the checksum setting, when
// there is one.
type Checksum struct{}

// Name implements Step.
func (Checksum) Name() string { return "checksum" }

// Run implements Step. A file that does not match gives a *MismatchError.
func (Checksum) Run(ctx context.Context, j *Job) error {
	want, ok := j.Settings.Checksum.Get()
	if !ok {
		return nil
	}
	got, err := ChecksumOf(ctx, j.Path, want.Algorithm)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(want.Expected))) != 1 {
		return &MismatchError{Path: j.Path, Algorithm: want.Algorithm, Expected: strings.ToLower(want.Expected), Got: got}
	}
	return nil
}
