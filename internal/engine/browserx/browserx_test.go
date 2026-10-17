package browserx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/sabadia/dlm-server/internal/browser"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
)

type fakeFiles map[string]browser.Candidate

func (f fakeFiles) Candidate(id string, index int) (browser.Candidate, error) {
	c, ok := f[URL(id, index)]
	if !ok {
		return browser.Candidate{}, browser.ErrResultGone
	}
	return c, nil
}

func stage(t *testing.T, content string) (browser.Candidate, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "guid-1234")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return browser.Candidate{Index: 0, URL: "blob:http://x/1", Kind: browser.KindFile, MIME: "text/plain", Size: int64(len(content)), Filename: "made-in-js.txt", Source: "blob", File: file}, file
}

func TestURL(t *testing.T) {
	id, idx, err := ParseURL(URL("abc123", 7))
	if err != nil || id != "abc123" || idx != 7 {
		t.Errorf("ParseURL(URL) = %q, %d, %v", id, idx, err)
	}
	for _, bad := range []string{"", "http://x/1", "dlm-browser:///1", "dlm-browser://id/", "dlm-browser://id/x", "dlm-browser://id/-1"} {
		if _, _, err := ParseURL(bad); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ParseURL(%q) = %v, want ErrInvalidArgument", bad, err)
		}
	}
}

func TestMatchAndResolve(t *testing.T) {
	c, _ := stage(t, "hello")
	e := New(fakeFiles{URL("r1", 0): c})
	if !e.Match(domain.Source{CurrentURL: URL("r1", 0)}) || e.Match(domain.Source{CurrentURL: "https://x/y"}) || e.Match(domain.Source{}) {
		t.Error("Match misjudges")
	}
	probe, err := e.Resolve(context.Background(), engine.Request{Source: domain.Source{CurrentURL: URL("r1", 0), PageURL: "https://page/"}})
	if err != nil {
		t.Fatal(err)
	}
	if probe.Engine != EngineName || probe.Suggested != "made-in-js.txt" || probe.Resource.Size != 5 || probe.Source.Fingerprint.Size != 5 ||
		probe.Source.Fingerprint.ContentType != "text/plain" || probe.Source.PageURL != "https://page/" {
		t.Errorf("probe = %+v", probe)
	}
	for name, src := range map[string]string{"expired": URL("gone", 0), "malformed": "dlm-browser://x"} {
		if _, err := e.Resolve(context.Background(), engine.Request{Source: domain.Source{CurrentURL: src}}); engine.Classify(err) != engine.KindFatal {
			t.Errorf("%s: err = %v, want a fatal error", name, err)
		}
	}
	noFile := browser.Candidate{URL: "https://x/f.zip", Kind: browser.KindFile}
	if _, err := New(fakeFiles{URL("r", 0): noFile}).Resolve(context.Background(), engine.Request{Source: domain.Source{CurrentURL: URL("r", 0)}}); err == nil {
		t.Error("a candidate without a staged file resolved")
	}
}

type recorder struct{ cps []engine.Checkpoint }

func (r *recorder) Checkpoint(_ context.Context, cp engine.Checkpoint) error {
	r.cps = append(r.cps, cp)
	return nil
}

func spec(_ *testing.T, dest string, rec engine.Checkpointer) engine.Spec {
	return engine.Spec{TaskID: "t", Source: domain.Source{CurrentURL: URL("r1", 0)}, Path: dest, Checkpointer: rec}
}

func TestDownloadMovesTheFile(t *testing.T) {
	c, staged := stage(t, "payload")
	e := New(fakeFiles{URL("r1", 0): c})
	dest := filepath.Join(t.TempDir(), "sub", "out.txt")
	rec := &recorder{}
	dl, err := e.NewDownload(spec(t, dest, rec))
	if err != nil {
		t.Fatal(err)
	}
	if p := dl.Progress(); p.Downloaded != 0 {
		t.Errorf("progress before Run = %+v", p)
	}
	if err := dl.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "payload" {
		t.Errorf("destination = %q, %v", got, err)
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staged file still exists: %v", err)
	}
	if _, err := os.Stat(dest + PartSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".part file left: %v", err)
	}
	if p := dl.Progress(); p.Downloaded != 7 || p.Size != 7 {
		t.Errorf("progress = %+v", p)
	}
	if len(rec.cps) != 1 || rec.cps[0].Size != 7 || rec.cps[0].Downloaded != 7 {
		t.Errorf("checkpoints = %+v", rec.cps)
	}
	if err := dl.Run(context.Background()); engine.Classify(err) != engine.KindFatal {
		t.Errorf("second Run = %v, want a fatal error", err)
	}
}

func TestDownloadErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("destination exists", func(t *testing.T) {
		c, staged := stage(t, "x")
		dest := filepath.Join(t.TempDir(), "out.txt")
		if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		dl, _ := New(fakeFiles{URL("r1", 0): c}).NewDownload(spec(t, dest, nil))
		if err := dl.Run(ctx); !errors.Is(err, ErrDestinationExists) {
			t.Errorf("Run = %v, want ErrDestinationExists", err)
		}
		if got, _ := os.ReadFile(dest); string(got) != "old" {
			t.Errorf("destination overwritten: %q", got)
		}
		if _, err := os.Stat(staged); err != nil {
			t.Errorf("staged file lost: %v", err)
		}
	})
	t.Run("staged file is gone", func(t *testing.T) {
		c, staged := stage(t, "x")
		if err := os.Remove(staged); err != nil {
			t.Fatal(err)
		}
		dl, _ := New(fakeFiles{URL("r1", 0): c}).NewDownload(spec(t, filepath.Join(t.TempDir(), "o"), nil))
		if err := dl.Run(ctx); !errors.Is(err, ErrGone) {
			t.Errorf("Run = %v, want ErrGone", err)
		}
	})
	t.Run("result expired", func(t *testing.T) {
		dl, _ := New(fakeFiles{}).NewDownload(spec(t, filepath.Join(t.TempDir(), "o"), nil))
		if err := dl.Run(ctx); !errors.Is(err, ErrGone) {
			t.Errorf("Run = %v, want ErrGone", err)
		}
	})
	t.Run("an earlier run finished the move", func(t *testing.T) {
		c, staged := stage(t, "x")
		dest := filepath.Join(t.TempDir(), "o")
		if err := os.Remove(staged); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest+PartSuffix, []byte("complete"), 0o644); err != nil {
			t.Fatal(err)
		}
		dl, _ := New(fakeFiles{URL("r1", 0): c}).NewDownload(spec(t, dest, nil))
		if err := dl.Run(ctx); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(dest); string(got) != "complete" {
			t.Errorf("destination = %q", got)
		}
	})
	t.Run("invalid specs", func(t *testing.T) {
		e := New(fakeFiles{})
		if _, err := e.NewDownload(engine.Spec{Source: domain.Source{CurrentURL: URL("r", 0)}, Path: "relative"}); err == nil {
			t.Error("relative path accepted")
		}
		if _, err := e.NewDownload(engine.Spec{Source: domain.Source{CurrentURL: "https://x/y"}, Path: filepath.Join(t.TempDir(), "o")}); err == nil {
			t.Error("foreign source accepted")
		}
	})
}

func TestCopyContext(t *testing.T) {
	data := bytes.Repeat([]byte("abc"), 200_000)
	var n atomic.Int64
	var out bytes.Buffer
	if err := copyContext(context.Background(), &out, bytes.NewReader(data), &n); err != nil || !bytes.Equal(out.Bytes(), data) || n.Load() != int64(len(data)) {
		t.Errorf("copyContext = %v, %d bytes counted", err, n.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyContext(ctx, &out, bytes.NewReader(data), &n); !errors.Is(err, context.Canceled) {
		t.Errorf("copyContext on a cancelled context = %v", err)
	}
}
