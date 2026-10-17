package platform

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowserCandidates(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	t.Run("darwin", func(t *testing.T) {
		got := browserCandidates("darwin", filepath.Join("h", "u"), env(nil))
		want := BrowserCandidate{Browser: BrowserChrome, Path: filepath.Join("/Applications", "Google Chrome.app", "Contents", "MacOS", "Google Chrome")}
		if got[0] != want {
			t.Errorf("first candidate = %+v, want %+v", got[0], want)
		}
		if len(got) != 8 { // four browsers, system and user applications
			t.Errorf("%d candidates, want 8", len(got))
		}
	})
	t.Run("windows", func(t *testing.T) {
		got := browserCandidates("windows", "", env(map[string]string{"ProgramFiles": `C:\PF`, "LocalAppData": `C:\L`}))
		var chrome []string
		for _, c := range got {
			if c.Browser == BrowserChrome {
				chrome = append(chrome, c.Path)
			}
		}
		want := []string{filepath.Join(`C:\PF`, "Google", "Chrome", "Application", "chrome.exe"), filepath.Join(`C:\L`, "Google", "Chrome", "Application", "chrome.exe")}
		if len(chrome) != 2 || chrome[0] != want[0] || chrome[1] != want[1] {
			t.Errorf("chrome candidates = %v, want %v", chrome, want)
		}
	})
	t.Run("linux", func(t *testing.T) {
		got := browserCandidates("linux", "/home/u", env(nil))
		if got[0] != (BrowserCandidate{Browser: BrowserChrome, Path: "google-chrome", Lookup: true}) {
			t.Errorf("first candidate = %+v", got[0])
		}
		seen := map[Browser]bool{}
		for _, c := range got {
			seen[c.Browser] = true
		}
		for _, b := range []Browser{BrowserChrome, BrowserChromium, BrowserEdge, BrowserBrave} {
			if !seen[b] {
				t.Errorf("no candidate for %s", b)
			}
		}
		if seen[BrowserFirefox] {
			t.Error("Firefox is not driven through CDP")
		}
	})
}

func TestChromeForTestingPlatform(t *testing.T) {
	for in, want := range map[string]string{
		"linux/amd64": "linux64", "linux/arm64": "linux-arm64", "darwin/arm64": "mac-arm64", "darwin/amd64": "mac-x64",
		"windows/amd64": "win64", "windows/arm64": "win64", "windows/386": "win32", "freebsd/amd64": "",
	} {
		goos, goarch := cutSlash(in)
		if got := chromeForTestingPlatform(goos, goarch); got != want {
			t.Errorf("%s = %q, want %q", in, got, want)
		}
	}
}

func cutSlash(s string) (string, string) {
	goos, goarch, _ := strings.Cut(s, "/")
	return goos, goarch
}

func TestFFmpegPlatform(t *testing.T) {
	for in, want := range map[string]string{
		"linux/amd64": "linux-x64", "linux/arm64": "linux-arm64", "linux/arm": "linux-arm", "linux/386": "linux-ia32",
		"darwin/arm64": "darwin-arm64", "darwin/amd64": "darwin-x64", "windows/amd64": "win32-x64", "windows/arm64": "win32-x64",
		"windows/386": "", "freebsd/amd64": "",
	} {
		goos, goarch := cutSlash(in)
		if got := ffmpegPlatform(goos, goarch); got != want {
			t.Errorf("%s = %q, want %q", in, got, want)
		}
	}
}
