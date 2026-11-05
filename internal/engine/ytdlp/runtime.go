package ytdlp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// YouTube (and a few other sites) hide their streams behind JavaScript that
// yt-dlp has to run, so it needs a JavaScript runtime: deno is found on PATH by
// itself, node and bun have to be named. A daemon started by the desktop app
// does not get the PATH of a terminal (on macOS no /opt/homebrew/bin), so
// without the directories below yt-dlp finds no runtime and reports "Requested
// format is not available" for a signed-in YouTube download.

// jsRuntimeOptionSince is the first yt-dlp release that has --js-runtimes.
const jsRuntimeOptionSince = "2025.11.01"

// toolDirs are the places a user's command-line tools usually live, searched
// after PATH.
func toolDirs() []string {
	home, _ := os.UserHomeDir()
	dirs := []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/snap/bin"}
	if home != "" {
		dirs = append(dirs,
			filepath.Join(home, ".deno", "bin"), filepath.Join(home, ".bun", "bin"),
			filepath.Join(home, ".local", "bin"), filepath.Join(home, ".cargo", "bin"),
			filepath.Join(home, "scoop", "shims"))
	}
	if runtime.GOOS == "windows" {
		dirs = []string{`C:\Program Files\nodejs`, `C:\Program Files\deno`}
		if home != "" {
			dirs = append(dirs, filepath.Join(home, ".deno", "bin"), filepath.Join(home, "scoop", "shims"))
		}
	}
	return dirs
}

// toolPath is PATH with the tool directories that exist added at the end.
func toolPath(current string) string {
	have := map[string]bool{}
	parts := filepath.SplitList(current)
	for _, p := range parts {
		have[p] = true
	}
	for _, d := range toolDirs() {
		if have[d] {
			continue
		}
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			parts = append(parts, d)
			have[d] = true
		}
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// environ is the environment yt-dlp runs in: the daemon's, with PATH widened.
func environ() []string {
	env := os.Environ()
	for i, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "PATH") {
			env[i] = k + "=" + toolPath(v)
			return env
		}
	}
	return append(env, "PATH="+toolPath(""))
}

// runtimeArgs names a JavaScript runtime for yt-dlp when it would not find one
// itself: nothing if deno is available (yt-dlp's own default) or yt-dlp is too
// old to take the option.
func runtimeArgs(version string) []string {
	return runtimeArgsIn(version, toolPath(os.Getenv("PATH")))
}

// runtimeArgsIn is runtimeArgs searching the directories of path.
func runtimeArgsIn(version, path string) []string {
	find := func(name string) string {
		for _, d := range filepath.SplitList(path) {
			if p, err := exec.LookPath(filepath.Join(d, name)); err == nil {
				return p
			}
		}
		return ""
	}
	if find("deno") != "" || version < jsRuntimeOptionSince || version == "" {
		return nil
	}
	for _, name := range []string{"node", "bun"} {
		if p := find(name); p != "" {
			return []string{"--js-runtimes=" + name + ":" + p}
		}
	}
	return nil
}
