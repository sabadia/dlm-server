package httpx

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestDetectFilenameContentDisposition(t *testing.T) {
	tests := []struct {
		name, cd, want string
	}{
		{"rfc5987 utf-8", `attachment; filename*=UTF-8''%E2%82%AC%20rates.pdf`, "€ rates.pdf"},
		{"rfc5987 iso-8859-1", `attachment; filename*=iso-8859-1'en'%A3%20rates.pdf`, "£ rates.pdf"},
		{"filename* wins over filename", `attachment; filename="fallback.pdf"; filename*=UTF-8''real.pdf`, "real.pdf"},
		{"bad filename* falls back", `attachment; filename*=KOI8-R''x.pdf; filename="ok.pdf"`, "ok.pdf"},
		{"quoted with spaces", `attachment; filename="my report 2026.pdf"`, "my report 2026.pdf"},
		{"quoted with escapes", `attachment; filename="a \"b\".pdf"`, "a _b_.pdf"},
		{"unquoted", `attachment; filename=file.zip`, "file.zip"},
		{"unquoted with spaces", `attachment; filename=my file.zip`, "my file.zip"},
		{"percent-encoded", `attachment; filename="%E4%B8%AD%E6%96%87.zip"`, "中文.zip"},
		{"rfc2047 encoded word", `attachment; filename="=?UTF-8?B?w6TDtsO8LnppcA==?="`, "äöü.zip"},
		{"raw latin-1 bytes", "attachment; filename=\"caf\xe9.txt\"", "café.txt"},
		{"inline type", `inline; filename="view.pdf"`, "view.pdf"},
		{"no disposition type", `filename="bare.zip"`, "bare.zip"},
		{"case-insensitive key", `Attachment; FILENAME="up.zip"`, "up.zip"},
		{"unterminated quote", `attachment; filename="broken.zip`, "broken.zip"},
		{"junk after quote", `attachment; filename="a.zip"junk; size=3`, "a.zip"},
		{"path traversal", `attachment; filename="../../evil.exe"`, "evil.exe"},
		{"windows traversal", `attachment; filename="..\\..\\evil.exe"`, "evil.exe"},
		{"dot-dot only falls back to URL", `attachment; filename=".."`, "file.bin"},
		{"empty filename falls back to URL", `attachment; filename=""`, "file.bin"},
		{"no filename param", `attachment`, "file.bin"},
		{"garbage", `;;;==;"`, "file.bin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{"Content-Disposition": {tt.cd}}}
			if got := DetectFilename(resp, mustURL(t, "https://example.com/dl/file.bin")); got != tt.want {
				t.Errorf("DetectFilename(%q) = %q, want %q", tt.cd, got, tt.want)
			}
		})
	}
}

func TestDetectFilenameURLFallbackAndExtension(t *testing.T) {
	tests := []struct {
		name, url, ctype, cd, want string
	}{
		{"last segment", "https://example.com/a/b/archive.tar.gz?x=1", "", "", "archive.tar.gz"},
		{"trailing slash", "https://example.com/files/report.pdf/", "", "", "report.pdf"},
		{"percent-decoded", "https://example.com/d/my%20file%E2%82%AC.zip", "", "", "my file€.zip"},
		{"encoded slash stays in segment", "https://example.com/d/a%2Fb.zip", "", "", "b.zip"},
		{"host fallback", "https://example.com/", "", "", "example.com"},
		{"host fallback gets ext", "https://example.com", "text/html; charset=utf-8", "", "example.com.html"},
		{"ext appended from type", "https://example.com/get/report", "application/pdf", "", "report.pdf"},
		{"ext appended to disposition name", "https://example.com/x", "application/zip", `attachment; filename="bundle"`, "bundle.zip"},
		{"preferred mp4", "https://example.com/v/clip", "video/mp4", "", "clip.mp4"},
		{"html appended", "https://example.com/page", "text/html", "", "page.html"},
		{"octet-stream adds nothing", "https://example.com/blob", "application/octet-stream", "", "blob"},
		{"existing ext kept", "https://example.com/a.iso", "application/zip", "", "a.iso"},
		{"version is not an ext", "https://example.com/tool-1.2", "application/zip", "", "tool-1.2.zip"},
		{"no url", "", "application/zip", "", "download.zip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.ctype != "" {
				h.Set("Content-Type", tt.ctype)
			}
			if tt.cd != "" {
				h.Set("Content-Disposition", tt.cd)
			}
			var u *url.URL
			if tt.url != "" {
				u = mustURL(t, tt.url)
			}
			if got := DetectFilename(&http.Response{Header: h}, u); got != tt.want {
				t.Errorf("DetectFilename = %q, want %q", got, tt.want)
			}
		})
	}
	if got := DetectFilename(nil, nil); got != "download" {
		t.Errorf("DetectFilename(nil, nil) = %q", got)
	}
}

func TestSanitizeFilename(t *testing.T) {
	long := strings.Repeat("é", 200) + ".mp4" // 404 bytes
	tests := []struct {
		name, in, want string
	}{
		{"plain", "file.zip", "file.zip"},
		{"empty", "", "download"},
		{"spaces only", "   ", "download"},
		{"dots only", "...", "download"},
		{"unix path", "a/b/c.txt", "c.txt"},
		{"windows path", `C:\Users\x\c.txt`, "c.txt"},
		{"traversal", "../../evil.exe", "evil.exe"},
		{"trailing separator", "dir/", "download"},
		{"invalid chars", `a<b>c:d"e|f?g*h.txt`, "a_b_c_d_e_f_g_h.txt"},
		{"control chars", "a\x00b\x1fc\x7f.txt", "abc.txt"},
		{"newline", "a\r\nb.txt", "ab.txt"},
		{"bidi override", "evil\u202Etxt.exe", "eviltxt.exe"},
		{"invalid utf-8", "a\xffb.txt", "a_b.txt"},
		{"trailing dots and spaces", "name. . ", "name"},
		{"leading spaces", "  name.txt", "name.txt"},
		{"hidden file kept", ".bashrc", ".bashrc"},
		{"reserved CON", "CON", "CON_"},
		{"reserved lower", "nul", "nul_"},
		{"reserved with ext", "con.txt", "con_.txt"},
		{"reserved double ext", "Aux.tar.gz", "Aux_.tar.gz"},
		{"reserved COM1", "COM1.log", "COM1_.log"},
		{"reserved LPT9", "lpt9", "lpt9_"},
		{"reserved trailing space stem", "PRN .txt", "PRN _.txt"},
		{"not reserved COM0", "COM0", "COM0"},
		{"not reserved CONSOLE", "console.txt", "console.txt"},
		{"not reserved COM10", "COM10", "COM10"},
		{"unicode kept", "日本語 ファイル.pdf", "日本語 ファイル.pdf"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeFilename(tt.in); got != tt.want {
				t.Errorf("SanitizeFilename(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	t.Run("long utf-8 keeps extension", func(t *testing.T) {
		got := SanitizeFilename(long)
		if len(got) > 255 || !utf8.ValidString(got) || !strings.HasSuffix(got, ".mp4") {
			t.Fatalf("len=%d valid=%v name=%q", len(got), utf8.ValidString(got), got)
		}
		if len(got) < 250 {
			t.Errorf("truncated too much: %d bytes", len(got))
		}
	})
	t.Run("long without extension", func(t *testing.T) {
		got := SanitizeFilename(strings.Repeat("ab€", 100))
		if len(got) > 255 || !utf8.ValidString(got) {
			t.Fatalf("len=%d valid=%v", len(got), utf8.ValidString(got))
		}
	})
	t.Run("idempotent", func(t *testing.T) {
		for _, tt := range tests {
			once := SanitizeFilename(tt.in)
			if twice := SanitizeFilename(once); twice != once {
				t.Errorf("not idempotent for %q: %q -> %q", tt.in, once, twice)
			}
		}
	})
}
