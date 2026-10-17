package httpx

import (
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// defaultFilename is used when no usable name can be derived.
const defaultFilename = "download"

// maxFilenameBytes is the common file-name length limit (ext4, NTFS, APFS).
const maxFilenameBytes = 255

// DetectFilename picks the file name for a response, trying in order:
//  1. Content-Disposition filename* (RFC 6266 / RFC 5987; UTF-8 and
//     ISO-8859-1 charsets);
//  2. Content-Disposition filename (quoted or unquoted; percent-encoded and
//     RFC 2047 encoded-word values are decoded; malformed headers are parsed
//     leniently);
//  3. the last non-empty path segment of finalURL, percent-decoded;
//  4. the host name of finalURL.
//
// A candidate that sanitises to nothing (e.g. "..") falls through to the
// next source. When the chosen name has no extension, one is appended from
// the Content-Type, except for application/octet-stream (which says
// nothing about the format). Names taken from the host name always get the
// Content-Type extension because the host's dots are not an extension. The
// result is sanitised with SanitizeFilename and never empty.
func DetectFilename(resp *http.Response, finalURL *url.URL) string {
	var header http.Header
	if resp != nil {
		header = resp.Header
	}
	ctype := mediaType(header.Get("Content-Type"))

	if name := sanitize(dispositionFilename(header.Get("Content-Disposition"))); name != "" {
		return withExtension(name, ctype, false)
	}
	if finalURL != nil {
		if name := sanitize(lastPathSegment(finalURL)); name != "" {
			return withExtension(name, ctype, false)
		}
		if name := sanitize(finalURL.Hostname()); name != "" {
			return withExtension(name, ctype, true)
		}
	}
	return withExtension(defaultFilename, ctype, true)
}

// withExtension appends an extension for ctype when name has none (or
// always when force is set), then re-sanitises to keep the length limit.
func withExtension(name, ctype string, force bool) string {
	if !force && hasExtension(name) {
		return name
	}
	if ext := extensionForType(ctype); ext != "" && !strings.HasSuffix(strings.ToLower(name), ext) {
		name += ext
	}
	return SanitizeFilename(name)
}

// hasExtension reports whether name ends in ".ext" where ext is 1-8 ASCII
// letters/digits containing at least one letter, so "v1.2" or "a.b c" do
// not count as having an extension but "file.mp4" and "a.7z" do.
func hasExtension(name string) bool {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || i == len(name)-1 || len(name)-i-1 > 8 {
		return false
	}
	letter := false
	for _, r := range name[i+1:] {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			letter = true
		case r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return letter
}

// preferredExtension maps common media types to the extension users
// expect; mime.ExtensionsByType returns several in no useful order.
func preferredExtension(ctype string) string {
	switch ctype {
	case "application/zip", "application/x-zip-compressed":
		return ".zip"
	case "application/pdf":
		return ".pdf"
	case "application/gzip", "application/x-gzip":
		return ".gz"
	case "application/x-tar":
		return ".tar"
	case "application/x-bzip2":
		return ".bz2"
	case "application/x-xz":
		return ".xz"
	case "application/zstd":
		return ".zst"
	case "application/x-7z-compressed":
		return ".7z"
	case "application/vnd.rar", "application/x-rar-compressed":
		return ".rar"
	case "application/json":
		return ".json"
	case "application/xml", "text/xml":
		return ".xml"
	case "application/x-bittorrent":
		return ".torrent"
	case "application/vnd.apple.mpegurl", "application/x-mpegurl":
		return ".m3u8"
	case "application/dash+xml":
		return ".mpd"
	case "application/x-msdownload", "application/vnd.microsoft.portable-executable":
		return ".exe"
	case "application/x-msi":
		return ".msi"
	case "application/vnd.android.package-archive":
		return ".apk"
	case "application/x-iso9660-image":
		return ".iso"
	case "application/x-debian-package":
		return ".deb"
	case "application/x-rpm":
		return ".rpm"
	case "application/x-apple-diskimage":
		return ".dmg"
	case "application/epub+zip":
		return ".epub"
	case "text/plain":
		return ".txt"
	case "text/html":
		return ".html"
	case "text/csv":
		return ".csv"
	case "text/css":
		return ".css"
	case "text/javascript":
		return ".js"
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	case "image/avif":
		return ".avif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/x-matroska":
		return ".mkv"
	case "video/quicktime":
		return ".mov"
	case "video/mp2t":
		return ".ts"
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4":
		return ".m4a"
	case "audio/ogg":
		return ".ogg"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/flac":
		return ".flac"
	case "audio/webm":
		return ".weba"
	default:
		return ""
	}
}

// extensionForType returns the extension (with dot) for a media type, or ""
// when none is known or the type is generic binary.
func extensionForType(ctype string) string {
	if ctype == "" || ctype == "application/octet-stream" {
		return ""
	}
	if ext := preferredExtension(ctype); ext != "" {
		return ext
	}
	exts, err := mime.ExtensionsByType(ctype)
	if err != nil || len(exts) == 0 {
		return ""
	}
	return strings.ToLower(exts[0])
}

// lastPathSegment returns the last non-empty, percent-decoded path segment.
func lastPathSegment(u *url.URL) string {
	segs := strings.Split(u.EscapedPath(), "/")
	for i := len(segs) - 1; i >= 0; i-- {
		if segs[i] == "" {
			continue
		}
		if dec, err := url.PathUnescape(segs[i]); err == nil {
			return dec
		}
		return segs[i]
	}
	return ""
}

// dispositionFilename extracts the file name from a Content-Disposition
// value, preferring filename* over filename. It never fails: malformed
// input yields the best guess or "".
func dispositionFilename(v string) string {
	if strings.TrimSpace(v) == "" {
		return ""
	}
	params := parseDispositionParams(v)
	if ext, ok := params["filename*"]; ok {
		if name, ok := decodeExtValue(ext); ok && strings.TrimSpace(name) != "" {
			return name
		}
	}
	if name, ok := params["filename"]; ok {
		return decodePlainFilename(name)
	}
	return ""
}

// parseDispositionParams splits "type; k=v; k2="v 2"" into lower-cased
// keys and unquoted values. Values may be quoted strings (backslash
// escapes honoured, an unterminated quote runs to the end) or tokens that
// run to the next ';' (so unquoted names with spaces survive). The first
// occurrence of a key wins.
func parseDispositionParams(v string) map[string]string {
	params := make(map[string]string)
	i := 0
	for i < len(v) {
		// key
		j := i
		for j < len(v) && v[j] != '=' && v[j] != ';' {
			j++
		}
		key := strings.ToLower(strings.TrimSpace(v[i:j]))
		if j >= len(v) || v[j] == ';' {
			i = j + 1 // disposition type or a bare token
			continue
		}
		j++ // skip '='
		for j < len(v) && (v[j] == ' ' || v[j] == '\t') {
			j++
		}
		var val string
		if j < len(v) && v[j] == '"' {
			var b strings.Builder
			j++
			for j < len(v) && v[j] != '"' {
				if v[j] == '\\' && j+1 < len(v) {
					j++
				}
				b.WriteByte(v[j])
				j++
			}
			val = b.String()
			for j < len(v) && v[j] != ';' {
				j++ // ignore junk after the closing quote
			}
		} else {
			k := j
			for j < len(v) && v[j] != ';' {
				j++
			}
			val = strings.TrimSpace(v[k:j])
		}
		if _, dup := params[key]; !dup && key != "" {
			params[key] = val
		}
		i = j + 1
	}
	return params
}

// decodeExtValue decodes an RFC 5987 ext-value: charset'lang'pct-encoded.
// UTF-8 and ISO-8859-1 (and US-ASCII) are supported.
func decodeExtValue(v string) (string, bool) {
	charset, rest, ok := strings.Cut(v, "'")
	if !ok {
		return "", false
	}
	_, enc, ok := strings.Cut(rest, "'")
	if !ok {
		return "", false
	}
	raw, err := url.PathUnescape(enc)
	if err != nil {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "utf-8", "utf8", "us-ascii":
		if !utf8.ValidString(raw) {
			return "", false
		}
		return raw, true
	case "iso-8859-1", "latin1", "latin-1":
		return latin1ToUTF8(raw), true
	default:
		return "", false
	}
}

// decodePlainFilename decodes the forms found in the wild for the plain
// filename parameter: RFC 2047 encoded words, percent-encoding (as browsers
// accept), raw UTF-8, and raw ISO-8859-1 bytes.
func decodePlainFilename(v string) string {
	if strings.Contains(v, "=?") {
		if dec, err := new(mime.WordDecoder).DecodeHeader(v); err == nil {
			v = dec
		}
	}
	if strings.Contains(v, "%") {
		if dec, err := url.PathUnescape(v); err == nil && utf8.ValidString(dec) {
			v = dec
		}
	}
	if !utf8.ValidString(v) {
		v = latin1ToUTF8(v)
	}
	return v
}

func latin1ToUTF8(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		b.WriteRune(rune(s[i]))
	}
	return b.String()
}

// SanitizeFilename turns an untrusted name into a single, portable file
// name that is valid on Windows, macOS and Linux: it strips directory
// components (both / and \), removes control and bidi-override characters,
// replaces invalid UTF-8 and the characters <>:"|?* with '_', trims
// surrounding spaces and trailing dots, suffixes Windows reserved device
// names (CON, PRN, AUX, NUL, COM1-9, LPT1-9, with or without an extension,
// case-insensitive) with '_', and caps the length at 255 bytes while
// preserving the extension and valid UTF-8. It never returns "" (it falls
// back to "download").
func SanitizeFilename(name string) string {
	if s := sanitize(name); s != "" {
		return s
	}
	return defaultFilename
}

// sanitize is SanitizeFilename without the non-empty fallback.
func sanitize(name string) string {
	name = strings.ToValidUTF8(name, "_")
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), isBidiControl(r):
			return -1
		case strings.ContainsRune(`<>:"|?*`, r):
			return '_'
		default:
			return r
		}
	}, name)
	name = trimName(name)
	if name == "" {
		return ""
	}
	name = avoidReserved(name)
	return trimName(capLength(name))
}

// trimName removes leading spaces and trailing spaces and dots (Windows
// silently drops trailing dots/spaces, so "a." and "a" would collide).
func trimName(s string) string {
	s = strings.TrimLeft(s, " ")
	return strings.TrimRight(s, " .")
}

func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F
}

// avoidReserved suffixes Windows reserved device names with '_'. Windows
// treats "CON", "con.txt" and "Con .tar.gz" alike: the part before the
// first dot, ignoring trailing spaces, is what matters.
func avoidReserved(name string) string {
	stem, rest, found := strings.Cut(name, ".")
	if !isReservedName(strings.TrimRight(stem, " ")) {
		return name
	}
	if !found {
		return stem + "_"
	}
	return stem + "_." + rest
}

func isReservedName(s string) bool {
	u := strings.ToUpper(s)
	switch u {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(u) >= 4 && (strings.HasPrefix(u, "COM") || strings.HasPrefix(u, "LPT")) {
		switch u[3:] {
		case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
			return true
		}
	}
	return false
}

// capLength truncates name to maxFilenameBytes on a rune boundary, keeping
// the extension when it is reasonably short.
func capLength(name string) string {
	if len(name) <= maxFilenameBytes {
		return name
	}
	ext := ""
	if i := strings.LastIndexByte(name, '.'); i > 0 && len(name)-i <= 32 {
		ext = name[i:]
	}
	return truncateUTF8(strings.TrimSuffix(name, ext), maxFilenameBytes-len(ext)) + ext
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
