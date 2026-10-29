package nativehost

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/platform"
)

type fakeRegistry struct {
	keys map[string]string
}

func (f *fakeRegistry) Set(key, path string) error { f.keys[key] = path; return nil }
func (f *fakeRegistry) Delete(key string) error    { delete(f.keys, key); return nil }

const (
	extID     = "abcdefghijklmnopabcdefghijklmnop"
	extOrigin = "chrome-extension://" + extID + "/"
)

func newInstaller(t *testing.T, goos string) (*Installer, *fakeRegistry) {
	t.Helper()
	root := t.TempDir()
	reg := &fakeRegistry{keys: map[string]string{}}
	userCfg := filepath.Join(root, ".config")
	if goos == "darwin" {
		userCfg = filepath.Join(root, "Library", "Application Support")
	}
	return &Installer{
		Executable: filepath.Join(root, "bin", "dlm"),
		ConfigDir:  filepath.Join(root, "dlmcfg"),
		ServerURL:  "http://127.0.0.1:17171",
		DataDir:    filepath.Join(root, "dlmcfg"),
		GOOS:       goos, Home: root, UserConfigDir: userCfg, Registry: reg,
	}, reg
}

func readManifest(t *testing.T, path string) Manifest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func TestInstallAndUninstall(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			in, reg := newInstaller(t, goos)
			results, err := in.Install(Target{ChromiumOrigins: []string{extOrigin}, FirefoxIDs: []string{"dlm@example.org"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != len(platform.Browsers()) {
				t.Fatalf("results = %+v", results)
			}
			launcher := in.LauncherPath()
			for _, r := range results {
				if r.Status != StatusInstalled {
					t.Errorf("%s: status %q", r.Browser, r.Status)
					continue
				}
				m := readManifest(t, r.Manifest)
				if m.Name != HostName || m.Path != launcher || m.Type != "stdio" {
					t.Errorf("%s: manifest = %+v", r.Browser, m)
				}
				if r.Browser.Firefox() {
					if !slices.Equal(m.AllowedExtensions, []string{"dlm@example.org"}) || m.AllowedOrigins != nil {
						t.Errorf("firefox manifest = %+v", m)
					}
				} else if !slices.Equal(m.AllowedOrigins, []string{extOrigin}) || m.AllowedExtensions != nil {
					t.Errorf("%s manifest = %+v", r.Browser, m)
				}
				if goos == "windows" {
					if reg.keys[r.Registry] != r.Manifest || !strings.HasSuffix(r.Registry, `\`+HostName) {
						t.Errorf("%s: registry %q = %q, want the manifest path %q", r.Browser, r.Registry, reg.keys[r.Registry], r.Manifest)
					}
				} else if r.Registry != "" {
					t.Errorf("%s: unexpected registry key %q", r.Browser, r.Registry)
				}
			}
			if goos == "windows" && len(reg.keys) != len(platform.Browsers()) {
				t.Errorf("registry keys = %v", reg.keys)
			}
			script, err := os.ReadFile(launcher)
			if err != nil {
				t.Fatal(err)
			}
			wantScript, _ := LauncherScript(goos, in.Executable, in.ConfigDir, in.ServerURL)
			if string(script) != wantScript {
				t.Errorf("launcher =\n%s\nwant\n%s", script, wantScript)
			}
			if runtime.GOOS != "windows" {
				if info, err := os.Stat(launcher); err != nil || info.Mode().Perm()&0o111 == 0 {
					t.Errorf("launcher is not executable: %v, %v", info, err)
				}
			}

			// Installing again replaces everything without error.
			if _, err := in.Install(Target{ChromiumOrigins: []string{extOrigin}, FirefoxIDs: []string{"dlm@example.org"}}); err != nil {
				t.Fatalf("second Install: %v", err)
			}

			// Removing one browser keeps the launcher; removing all removes it.
			removed, err := in.Uninstall([]platform.Browser{platform.BrowserChrome})
			if err != nil || len(removed) != 1 || removed[0].Status != StatusRemoved {
				t.Fatalf("Uninstall(chrome) = %+v, %v", removed, err)
			}
			if _, err := os.Stat(launcher); err != nil {
				t.Errorf("launcher removed with one browser still installed: %v", err)
			}
			removed, err = in.Uninstall(nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range removed {
				want := StatusRemoved
				if r.Browser == platform.BrowserChrome {
					want = StatusAbsent
				}
				if r.Status != want {
					t.Errorf("%s: status %q, want %q", r.Browser, r.Status, want)
				}
				if _, err := os.Stat(r.Manifest); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s: manifest still exists: %v", r.Browser, err)
				}
			}
			if _, err := os.Stat(launcher); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("launcher still exists: %v", err)
			}
			if len(reg.keys) != 0 {
				t.Errorf("registry keys left: %v", reg.keys)
			}
		})
	}
}

func TestInstallSkipsBrowsersWithoutIDs(t *testing.T) {
	in, _ := newInstaller(t, "linux")
	results, err := in.Install(Target{ChromiumOrigins: []string{extOrigin}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		want := StatusInstalled
		if r.Browser.Firefox() {
			want = StatusSkipped
		}
		if r.Status != want {
			t.Errorf("%s: status %q, want %q", r.Browser, r.Status, want)
		}
		if r.Status == StatusSkipped && (r.Reason == "" || r.Manifest != "") {
			t.Errorf("%s: skipped result = %+v", r.Browser, r)
		}
	}

	in, _ = newInstaller(t, "linux")
	if results, err := in.Install(Target{}); err == nil || len(results) != len(platform.Browsers()) {
		t.Errorf("Install without any ID = %+v, %v; want an error and one skipped result per browser", results, err)
	}
	if _, err := os.Stat(in.LauncherPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("launcher written although nothing was installed: %v", err)
	}
}

func TestInstallBrowserSelectionAndValidation(t *testing.T) {
	in, _ := newInstaller(t, "linux")
	results, err := in.Install(Target{Browsers: []platform.Browser{platform.BrowserBrave}, ChromiumOrigins: []string{extOrigin}})
	if err != nil || len(results) != 1 || results[0].Browser != platform.BrowserBrave {
		t.Fatalf("Install(brave) = %+v, %v", results, err)
	}
	if _, err := in.Install(Target{Browsers: []platform.Browser{"netscape"}, ChromiumOrigins: []string{extOrigin}}); err == nil {
		t.Error("unknown browser accepted")
	}
	if _, err := in.Uninstall([]platform.Browser{"netscape"}); err == nil {
		t.Error("unknown browser accepted by Uninstall")
	}
	bad := *in
	bad.Executable = "relative/dlm"
	if _, err := bad.Install(Target{ChromiumOrigins: []string{extOrigin}}); err == nil {
		t.Error("relative executable accepted")
	}
}

func TestLauncherScript(t *testing.T) {
	got, err := LauncherScript("linux", "/opt/dlm app/dlm", "/home/u/it's/cfg", "http://127.0.0.1:17171")
	if err != nil {
		t.Fatal(err)
	}
	want := "#!/bin/sh\nexec '/opt/dlm app/dlm' native-host --config-dir '/home/u/it'\\''s/cfg' --server 'http://127.0.0.1:17171' \"$@\"\n"
	if got != want {
		t.Errorf("unix launcher =\n%q\nwant\n%q", got, want)
	}
	got, err = LauncherScript("windows", `C:\Program Files\dlm\dlm.exe`, `C:\Users\u 100%\cfg`, "http://127.0.0.1:17171")
	if err != nil {
		t.Fatal(err)
	}
	want = "@echo off\r\n\"C:\\Program Files\\dlm\\dlm.exe\" native-host --config-dir \"C:\\Users\\u 100%%\\cfg\" --server \"http://127.0.0.1:17171\" %*\r\n"
	if got != want {
		t.Errorf("windows launcher =\n%q\nwant\n%q", got, want)
	}
	for _, goos := range []string{"linux", "windows"} {
		if _, err := LauncherScript(goos, "/x\ny", "/c", "http://h"); err == nil {
			t.Errorf("%s: line break in a path accepted", goos)
		}
	}
	if _, err := LauncherScript("windows", `C:\x"y\dlm.exe`, `C:\c`, "http://h"); err == nil {
		t.Error("double quote in a Windows path accepted")
	}
}

func TestStatusFollowsInstallAndUninstall(t *testing.T) {
	in, _ := newInstaller(t, "linux")
	byBrowser := func(rs []Result) map[platform.Browser]string {
		m := map[platform.Browser]string{}
		for _, r := range rs {
			m[r.Browser] = r.Status
		}
		return m
	}
	before, err := in.Status(nil)
	if err != nil {
		t.Fatal(err)
	}
	for b, st := range byBrowser(before) {
		if st != StatusAbsent {
			t.Errorf("%s = %s before installing", b, st)
		}
	}
	if len(before) != len(platform.Browsers()) {
		t.Errorf("status lists %d browsers, want %d", len(before), len(platform.Browsers()))
	}
	if _, err := in.Install(Target{Browsers: []platform.Browser{platform.BrowserChrome}, ChromiumOrigins: []string{extOrigin}}); err != nil {
		t.Fatal(err)
	}
	after, _ := in.Status(nil)
	got := byBrowser(after)
	if got[platform.BrowserChrome] != StatusInstalled || got[platform.BrowserEdge] != StatusAbsent {
		t.Errorf("after installing Chrome: %v", got)
	}
	one, _ := in.Status([]platform.Browser{platform.BrowserChrome})
	if len(one) != 1 || one[0].Manifest == "" {
		t.Errorf("one browser = %+v", one)
	}
	if _, err := in.Status([]platform.Browser{"netscape"}); err == nil {
		t.Error("an unknown browser was accepted")
	}
	if _, err := in.Uninstall(nil); err != nil {
		t.Fatal(err)
	}
	if end, _ := in.Status(nil); byBrowser(end)[platform.BrowserChrome] != StatusAbsent {
		t.Error("still installed after uninstalling")
	}
}
