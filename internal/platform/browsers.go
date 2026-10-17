package platform

import (
	"os"
	"path/filepath"
	"runtime"
)

// BrowserCandidate is a place where a Chromium-family browser may be installed.
type BrowserCandidate struct {
	Browser Browser
	// Path is an absolute path to the executable, or, when Lookup is set, the
	// executable name to find on PATH.
	Path   string
	Lookup bool
}

// BrowserCandidates lists, in preference order, where the supported
// Chromium-family browsers (Chrome, Chromium, Edge, Brave) usually live on
// the running operating system. It does not check that anything exists.
func BrowserCandidates() []BrowserCandidate {
	home, _ := os.UserHomeDir() // empty when unknown: user-level candidates are then skipped
	return browserCandidates(runtime.GOOS, home, os.Getenv)
}

// browserCandidates is BrowserCandidates for an explicit OS, home directory
// and environment.
func browserCandidates(goos, home string, getenv func(string) string) []BrowserCandidate {
	var out []BrowserCandidate
	add := func(b Browser, path string, lookup bool) {
		if path != "" {
			out = append(out, BrowserCandidate{Browser: b, Path: path, Lookup: lookup})
		}
	}
	switch goos {
	case "darwin":
		apps := []struct {
			b    Browser
			app  string
			exec string
		}{
			{BrowserChrome, "Google Chrome.app", "Google Chrome"},
			{BrowserChromium, "Chromium.app", "Chromium"},
			{BrowserEdge, "Microsoft Edge.app", "Microsoft Edge"},
			{BrowserBrave, "Brave Browser.app", "Brave Browser"},
		}
		for _, a := range apps {
			add(a.b, filepath.Join("/Applications", a.app, "Contents", "MacOS", a.exec), false)
			if home != "" {
				add(a.b, filepath.Join(home, "Applications", a.app, "Contents", "MacOS", a.exec), false)
			}
		}
	case "windows":
		var roots []string
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			if dir := getenv(env); dir != "" {
				roots = append(roots, dir)
			}
		}
		apps := []struct {
			b   Browser
			rel string
		}{
			{BrowserChrome, filepath.Join("Google", "Chrome", "Application", "chrome.exe")},
			{BrowserChromium, filepath.Join("Chromium", "Application", "chrome.exe")},
			{BrowserEdge, filepath.Join("Microsoft", "Edge", "Application", "msedge.exe")},
			{BrowserBrave, filepath.Join("BraveSoftware", "Brave-Browser", "Application", "brave.exe")},
		}
		for _, a := range apps {
			for _, root := range roots {
				add(a.b, filepath.Join(root, a.rel), false)
			}
		}
	default: // Linux and the BSDs: look on PATH, then the usual absolute locations
		names := []struct {
			b    Browser
			name string
		}{
			{BrowserChrome, "google-chrome"},
			{BrowserChrome, "google-chrome-stable"},
			{BrowserChromium, "chromium"},
			{BrowserChromium, "chromium-browser"},
			{BrowserEdge, "microsoft-edge"},
			{BrowserEdge, "microsoft-edge-stable"},
			{BrowserBrave, "brave-browser"},
			{BrowserBrave, "brave"},
		}
		for _, n := range names {
			add(n.b, n.name, true)
		}
		add(BrowserChromium, "/snap/bin/chromium", false)
		add(BrowserChrome, "/opt/google/chrome/chrome", false)
	}
	return out
}

// ChromeForTestingPlatform returns the platform name Chrome for Testing uses
// for the running OS and architecture ("linux64", "mac-arm64", "win64"…), or
// "" when it publishes no build for it.
func ChromeForTestingPlatform() string {
	return chromeForTestingPlatform(runtime.GOOS, runtime.GOARCH)
}

func chromeForTestingPlatform(goos, goarch string) string {
	switch goos + "/" + goarch {
	case "linux/amd64":
		return "linux64"
	case "linux/arm64":
		return "linux-arm64"
	case "darwin/arm64":
		return "mac-arm64"
	case "darwin/amd64":
		return "mac-x64"
	case "windows/amd64", "windows/arm64": // arm64 Windows runs the x64 build
		return "win64"
	case "windows/386":
		return "win32"
	default:
		return ""
	}
}

// HeadlessShellExecutable returns the executable name of chrome-headless-shell
// inside its archive directory on the running OS.
func HeadlessShellExecutable() string {
	if runtime.GOOS == "windows" {
		return "chrome-headless-shell.exe"
	}
	return "chrome-headless-shell"
}

// FFmpegPlatform returns the platform name of the static ffmpeg builds dlm
// downloads ("linux-x64", "darwin-arm64", "win32-x64"…) for the running OS
// and architecture, or "" when there is none.
func FFmpegPlatform() string { return ffmpegPlatform(runtime.GOOS, runtime.GOARCH) }

func ffmpegPlatform(goos, goarch string) string {
	switch goos + "/" + goarch {
	case "linux/amd64":
		return "linux-x64"
	case "linux/arm64":
		return "linux-arm64"
	case "linux/arm":
		return "linux-arm"
	case "linux/386":
		return "linux-ia32"
	case "darwin/arm64":
		return "darwin-arm64"
	case "darwin/amd64":
		return "darwin-x64"
	case "windows/amd64", "windows/arm64": // arm64 Windows runs the x64 build
		return "win32-x64"
	default:
		return ""
	}
}

// FFmpegExecutable returns the file name of ffmpeg on the running OS.
func FFmpegExecutable() string {
	if runtime.GOOS == "windows" {
		return "ffmpeg.exe"
	}
	return "ffmpeg"
}
