package platform

import "runtime"

// YtdlpAsset returns the file name of the standalone yt-dlp release build for
// the running OS and architecture ("" when there is none).
func YtdlpAsset() string { return ytdlpAsset(runtime.GOOS, runtime.GOARCH) }

func ytdlpAsset(goos, goarch string) string {
	switch goos {
	case "linux":
		switch goarch {
		case "amd64":
			return "yt-dlp_linux"
		case "arm64":
			return "yt-dlp_linux_aarch64"
		}
	case "darwin":
		return "yt-dlp_macos" // one universal build
	case "windows":
		switch goarch {
		case "amd64":
			return "yt-dlp.exe"
		case "arm64":
			return "yt-dlp_arm64.exe"
		case "386":
			return "yt-dlp_x86.exe"
		}
	}
	return ""
}

// YtdlpExecutable returns the name the downloaded yt-dlp is stored under.
func YtdlpExecutable() string {
	if runtime.GOOS == "windows" {
		return "yt-dlp.exe"
	}
	return "yt-dlp"
}
