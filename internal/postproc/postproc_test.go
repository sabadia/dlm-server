package postproc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

func writeFile(t *testing.T, content string) (dir, name string) {
	t.Helper()
	dir = t.TempDir()
	name = "f.bin"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, name
}

func TestChecksumOfKnownVectors(t *testing.T) {
	dir, name := writeFile(t, "abc")
	path := filepath.Join(dir, name)
	for alg, want := range map[settings.ChecksumAlgorithm]string{
		settings.ChecksumMD5:    "900150983cd24fb0d6963f7d28e17f72",
		settings.ChecksumSHA1:   "a9993e364706816aba3e25717850c26c9cd0d89d",
		settings.ChecksumSHA256: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		settings.ChecksumBLAKE3: "6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85",
	} {
		got, err := ChecksumOf(t.Context(), path, alg)
		if err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", alg, got, err, want)
		}
	}
	if _, err := ChecksumOf(t.Context(), path, "crc32"); err == nil {
		t.Error("an unknown algorithm must fail")
	}
	if _, err := ChecksumOf(t.Context(), filepath.Join(dir, "missing"), settings.ChecksumMD5); err == nil {
		t.Error("a missing file must fail")
	}
	if _, err := ChecksumOf(t.Context(), dir, settings.ChecksumMD5); !errors.Is(err, ErrNotAFile) {
		t.Errorf("a directory: err = %v, want ErrNotAFile", err)
	}
	// A big file is read in blocks and a cancelled context stops it.
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, make([]byte, 5<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := ChecksumOf(t.Context(), big, settings.ChecksumSHA256); err != nil || len(got) != 64 {
		t.Errorf("big file: %q, %v", got, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ChecksumOf(ctx, big, settings.ChecksumSHA256); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v", err)
	}
}

func task(dir, name string, cs *settings.Checksum) (*domain.Task, settings.Settings) {
	t := &domain.Task{ID: "t", Dir: dir, Name: name}
	var eff settings.Settings
	if cs != nil {
		eff.Checksum = settings.Some(*cs)
	}
	return t, eff
}

func TestChecksumStep(t *testing.T) {
	dir, name := writeFile(t, "abc")
	p := Default(nil)
	// No checksum: nothing to do.
	tk, eff := task(dir, name, nil)
	if _, err := p.Process(t.Context(), tk, eff); err != nil {
		t.Fatal(err)
	}
	// A match, in either case of the hex digits.
	for _, exp := range []string{"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", strings.ToUpper("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")} {
		tk, eff = task(dir, name, &settings.Checksum{Algorithm: settings.ChecksumSHA256, Expected: exp})
		if _, err := p.Process(t.Context(), tk, eff); err != nil {
			t.Errorf("%q: %v", exp, err)
		}
	}
	// A mismatch is a typed error that says what was found.
	tk, eff = task(dir, name, &settings.Checksum{Algorithm: settings.ChecksumSHA256, Expected: strings.Repeat("0", 64)})
	_, err := p.Process(t.Context(), tk, eff)
	var mm *MismatchError
	if !errors.As(err, &mm) || mm.Got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" || mm.Expected != strings.Repeat("0", 64) || mm.Path != filepath.Join(dir, name) {
		t.Fatalf("err = %v, want a *MismatchError", err)
	}
	// A file that cannot be read is an error of the step, not a mismatch.
	tk, eff = task(dir, "missing", &settings.Checksum{Algorithm: settings.ChecksumMD5, Expected: strings.Repeat("0", 32)})
	_, err = p.Process(t.Context(), tk, eff)
	if err == nil || errors.As(err, &mm) || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("err = %v", err)
	}
}

type stepFunc struct {
	name string
	run  func(context.Context, *Job) error
}

func (s stepFunc) Name() string                          { return s.name }
func (s stepFunc) Run(ctx context.Context, j *Job) error { return s.run(ctx, j) }

func TestPipelineOrderStopAndPath(t *testing.T) {
	var order []string
	mk := func(name string, err error) Step {
		return stepFunc{name, func(_ context.Context, j *Job) error {
			order = append(order, name)
			if name == "move" {
				j.Path = filepath.Join(filepath.Dir(j.Path), "moved.bin")
			}
			return err
		}}
	}
	boom := errors.New("boom")
	p := New(nil, mk("a", nil), mk("move", nil), mk("b", boom), mk("c", nil))
	tk, eff := task("/dl", "f.bin", nil)
	j, err := p.Process(t.Context(), tk, eff)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "b:") {
		t.Errorf("err = %v, want boom from step b", err)
	}
	if strings.Join(order, ",") != "a,move,b" {
		t.Errorf("steps ran as %v, want a,move,b (c is skipped after the failure)", order)
	}
	if j.Path != filepath.Join("/dl", "moved.bin") || j.Task.ID != "t" {
		t.Errorf("job = %+v", j)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	order = nil
	if _, err := New(nil, mk("a", nil)).Process(ctx, tk, eff); !errors.Is(err, context.Canceled) || len(order) != 0 {
		t.Errorf("cancelled: err = %v, ran %v", err, order)
	}
}
