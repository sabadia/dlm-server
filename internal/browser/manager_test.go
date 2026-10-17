package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
)

func TestSiteGroup(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.example.com/a/b":      "example.com",
		"https://a.b.example.co.uk/":       "example.co.uk",
		"http://127.0.0.1:8080/x":          "127.0.0.1",
		"http://localhost:3000/":           "localhost",
		"https://EXAMPLE.org":              "example.org",
		"":                                 "default",
		"not a url":                        "default",
		"https://[::1]:80/":                "__1",
		"https://user@sub.github.io/repo/": "sub.github.io", // github.io is a public suffix
	} {
		if got := SiteGroup(in); got != want {
			t.Errorf("SiteGroup(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeGroup(t *testing.T) {
	for in, want := range map[string]string{
		"example.com": "example.com", "Ex Ample/..": "ex_ample_..", "..": "_..", ".hidden": "_.hidden", "": "_", "a/b\\c": "a_b_c",
	} {
		got := sanitizeGroup(in)
		if got != want {
			t.Errorf("sanitizeGroup(%q) = %q, want %q", in, got, want)
		}
		if filepath.Base(got) != got || got == ".." || got == "." {
			t.Errorf("sanitizeGroup(%q) = %q is not a plain directory name", in, got)
		}
	}
	if long := sanitizeGroup(string(make([]byte, 500))); len(long) > 100 {
		t.Errorf("long group has %d bytes", len(long))
	}
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	exe := filepath.Join(dir, "browser")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	none := func() []Installation { return nil }

	t.Run("configured path wins", func(t *testing.T) {
		m := NewManager(Options{Path: exe, Discover: func() []Installation { return []Installation{{Name: "chrome", Path: "/other"}} }})
		if inst, err := m.Resolve(ctx); err != nil || inst.Path != exe {
			t.Errorf("Resolve = %+v, %v", inst, err)
		}
	})
	t.Run("configured path must exist", func(t *testing.T) {
		m := NewManager(Options{Path: filepath.Join(dir, "missing"), Discover: none})
		if _, err := m.Resolve(ctx); err == nil {
			t.Error("Resolve accepted a missing path")
		}
	})
	t.Run("discovered browser", func(t *testing.T) {
		m := NewManager(Options{Discover: func() []Installation { return []Installation{{Name: "edge", Path: exe}} }})
		inst, err := m.Resolve(ctx)
		if err != nil || inst.Name != "edge" {
			t.Fatalf("Resolve = %+v, %v", inst, err)
		}
		if st := m.Status(); st.Browser == nil || st.Browser.Path != exe || st.Max != DefaultMaxBrowsers || len(st.Running) != 0 {
			t.Errorf("Status = %+v", st)
		}
	})
	t.Run("nothing found and no download", func(t *testing.T) {
		m := NewManager(Options{Discover: none, CacheDir: t.TempDir()})
		if _, err := m.Resolve(ctx); !errors.Is(err, ErrNoBrowser) {
			t.Errorf("Resolve = %v, want ErrNoBrowser", err)
		}
	})
	t.Run("cached headless shell", func(t *testing.T) {
		cache := t.TempDir()
		f := &Fetcher{Dir: cache}
		p := filepath.Join(f.root(), "1.2.3.4", "x", filepathExe())
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		m := NewManager(Options{Discover: none, CacheDir: cache})
		if inst, err := m.Resolve(ctx); err != nil || inst.Name != NameHeadlessShell {
			t.Errorf("Resolve = %+v, %v", inst, err)
		}
	})
	t.Run("download when allowed", func(t *testing.T) {
		srv := newCFT(t, "155.0.1.2", shellZip(t))
		m := NewManager(Options{
			Discover: none, AutoDownload: true,
			Fetcher: &Fetcher{Dir: t.TempDir(), VersionsURL: srv.URL + "/versions.json", Platform: "linux64"},
		})
		if inst, err := m.Resolve(ctx); err != nil || inst.Version != "155.0.1.2" {
			t.Errorf("Resolve = %+v, %v", inst, err)
		}
		if _, err := m.Resolve(ctx); err != nil || srv.downloads.Load() != 1 {
			t.Errorf("second Resolve: %v after %d downloads", err, srv.downloads.Load())
		}
	})
	t.Run("download failure is reported as no browser", func(t *testing.T) {
		m := NewManager(Options{
			Discover: none, AutoDownload: true,
			Fetcher: &Fetcher{Dir: t.TempDir(), VersionsURL: "http://127.0.0.1:1/versions.json", Platform: "linux64"},
		})
		if _, err := m.Resolve(ctx); !errors.Is(err, ErrNoBrowser) {
			t.Errorf("Resolve = %v, want ErrNoBrowser", err)
		}
	})
}

func filepathExe() string {
	return (&Fetcher{}).exeName()
}

func TestAcquireWithoutBrowserAndAfterClose(t *testing.T) {
	m := NewManager(Options{Discover: func() []Installation { return nil }, ProfilesDir: t.TempDir()})
	if _, err := m.Acquire(context.Background(), "example.com"); !errors.Is(err, ErrNoBrowser) {
		t.Errorf("Acquire without a browser = %v, want ErrNoBrowser", err)
	}
	if st := m.Status(); len(st.Running) != 0 {
		t.Errorf("a failed start left %+v running", st.Running)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(context.Background(), "example.com"); !errors.Is(err, ErrClosed) {
		t.Errorf("Acquire after Close = %v, want ErrClosed", err)
	}
}

func TestProfiles(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Options{ProfilesDir: filepath.Join(dir, "profiles")})
	if got, err := m.Profiles(); err != nil || len(got) != 0 {
		t.Fatalf("Profiles of a missing directory = %v, %v", got, err)
	}
	for _, g := range []string{"a.com", "b.com"} {
		if err := os.MkdirAll(filepath.Join(dir, "profiles", g, "Default"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := m.Profiles(); err != nil || len(got) != 2 {
		t.Fatalf("Profiles = %v, %v", got, err)
	}
	if err := m.DeleteProfile("a.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles", "a.com")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("profile still on disk: %v", err)
	}
	if err := m.DeleteProfile("a.com"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("deleting twice = %v, want ErrNotFound", err)
	}
	// A hostile group name cannot reach outside the profiles directory.
	if err := m.DeleteProfile("../.."); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("DeleteProfile(../..) = %v, want ErrNotFound (sanitized)", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the parent directory is gone: %v", err)
	}
	if _, err := NewManager(Options{}).Profiles(); err == nil {
		t.Error("Profiles without a directory succeeded")
	}
}
