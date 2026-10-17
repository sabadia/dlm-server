package browser

import (
	"mime"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// Kind classifies a download candidate found on a page.
type Kind string

// Candidate kinds.
const (
	KindHLS   Kind = "hls"   // an HLS master or media playlist (.m3u8)
	KindDASH  Kind = "dash"  // a DASH manifest (.mpd)
	KindVideo Kind = "video" // a progressive video file
	KindAudio Kind = "audio" // a progressive audio file
	KindFile  Kind = "file"  // any other download (attachment, archive, installer, blob)
)

var (
	hlsMIME  = []string{"application/vnd.apple.mpegurl", "application/x-mpegurl", "audio/mpegurl", "audio/x-mpegurl"}
	dashMIME = []string{"application/dash+xml"}
	// segmentExt are pieces of a stream: the manifest is the download, not them.
	segmentExt  = []string{".ts", ".m4s", ".m4f", ".cmfv", ".cmfa", ".vtt", ".webvtt", ".srt", ".key"}
	segmentMIME = []string{"video/mp2t", "audio/mp2t", "video/iso.segment", "audio/iso.segment", "text/vtt"}
	videoExt    = []string{".mp4", ".webm", ".mkv", ".mov", ".m4v", ".flv", ".avi", ".wmv", ".mpg", ".mpeg"}
	audioExt    = []string{".mp3", ".m4a", ".ogg", ".opus", ".wav", ".flac", ".aac"}
	fileMIME    = []string{
		"application/octet-stream", "application/zip", "application/x-zip-compressed", "application/x-7z-compressed",
		"application/x-rar-compressed", "application/vnd.rar", "application/gzip", "application/x-gzip", "application/x-tar",
		"application/x-bzip2", "application/x-xz", "application/x-msdownload", "application/x-msi", "application/x-iso9660-image",
		"application/x-apple-diskimage", "application/vnd.android.package-archive", "application/java-archive", "application/x-bittorrent",
	}
	// ignoredTypes are resource types that are never downloads.
	ignoredTypes = []string{"Stylesheet", "Image", "Font", "Script", "TextTrack", "Ping", "CSPViolationReport", "Preflight"}
)

// classify decides whether a response is a download candidate and of which
// kind. resourceType is the browser's resource type, contentType the
// Content-Type header, disposition the Content-Disposition header and
// contentRange the Content-Range header.
func classify(rawURL, resourceType, contentType, disposition, contentRange string, status int) (Kind, bool) {
	if status < 200 || status > 299 || contains(ignoredTypes, resourceType) {
		return "", false
	}
	// A range request for a later part of a file (a player seeking, or a
	// stream's media segments) is not the file itself.
	if status == 206 && !rangeStartsAtZero(contentRange) {
		return "", false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	ext := strings.ToLower(path.Ext(u.Path))
	mt := mediaType(contentType)

	switch {
	case contains(segmentExt, ext) || contains(segmentMIME, mt):
		return "", false
	case ext == ".m3u8" || contains(hlsMIME, mt):
		return KindHLS, true
	case ext == ".mpd" || contains(dashMIME, mt):
		return KindDASH, true
	case isAttachment(disposition):
		return KindFile, true
	case strings.HasPrefix(mt, "video/") || (contains(videoExt, ext) && !strings.HasPrefix(mt, "text/") && mt != "application/json"):
		return KindVideo, true
	case strings.HasPrefix(mt, "audio/") || (contains(audioExt, ext) && !strings.HasPrefix(mt, "text/") && mt != "application/json"):
		return KindAudio, true
	case contains(fileMIME, mt) && resourceType != "XHR" && resourceType != "Fetch":
		return KindFile, true
	default:
		return "", false
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func mediaType(contentType string) string {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(contentType))
	}
	return mt
}

func isAttachment(disposition string) bool {
	if disposition == "" {
		return false
	}
	kind, _, err := mime.ParseMediaType(disposition)
	return err == nil && kind == "attachment"
}

// dispositionFilename returns the file name of a Content-Disposition value.
func dispositionFilename(disposition string) string {
	_, params, err := mime.ParseMediaType(disposition)
	if err != nil {
		return ""
	}
	return path.Base(strings.ReplaceAll(params["filename"], `\`, "/"))
}

// rangeStartsAtZero reports whether a Content-Range value ("bytes 0-99/200")
// starts at byte 0; an unparsable value counts as yes.
func rangeStartsAtZero(v string) bool {
	rest, ok := strings.CutPrefix(strings.TrimSpace(v), "bytes ")
	if !ok {
		return true
	}
	start, _, ok := strings.Cut(rest, "-")
	if !ok {
		return true
	}
	n, err := strconv.ParseInt(strings.TrimSpace(start), 10, 64)
	return err != nil || n == 0
}

// totalFromRange returns the total size of a Content-Range value, or -1.
func totalFromRange(v string) int64 {
	_, total, ok := strings.Cut(v, "/")
	if !ok {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}
