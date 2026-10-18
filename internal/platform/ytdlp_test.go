package platform

import "testing"

func TestYtdlpAsset(t *testing.T) {
	for _, c := range []struct{ goos, arch, want string }{
		{"linux", "amd64", "yt-dlp_linux"},
		{"linux", "arm64", "yt-dlp_linux_aarch64"},
		{"linux", "riscv64", ""},
		{"darwin", "arm64", "yt-dlp_macos"},
		{"darwin", "amd64", "yt-dlp_macos"},
		{"windows", "amd64", "yt-dlp.exe"},
		{"windows", "arm64", "yt-dlp_arm64.exe"},
		{"windows", "386", "yt-dlp_x86.exe"},
		{"freebsd", "amd64", ""},
	} {
		if got := ytdlpAsset(c.goos, c.arch); got != c.want {
			t.Errorf("ytdlpAsset(%s, %s) = %q, want %q", c.goos, c.arch, got, c.want)
		}
	}
	if YtdlpExecutable() == "" {
		t.Error("no executable name")
	}
}
