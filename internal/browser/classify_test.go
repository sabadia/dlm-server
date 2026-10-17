package browser

import (
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"

	"github.com/sabadia/dlm-server/internal/domain"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name                                         string
		url, rtype, ctype, disposition, contentRange string
		status                                       int
		want                                         Kind
	}{
		{"hls by type", "https://x/live", "Fetch", "application/vnd.apple.mpegurl", "", "", 200, KindHLS},
		{"hls by extension", "https://x/a/master.m3u8?t=1", "XHR", "text/plain", "", "", 200, KindHLS},
		{"hls x-mpegurl", "https://x/p", "Fetch", "application/x-mpegURL; charset=utf-8", "", "", 200, KindHLS},
		{"dash by extension", "https://x/m.mpd", "Fetch", "application/octet-stream", "", "", 200, KindDASH},
		{"dash by type", "https://x/m", "Fetch", "application/dash+xml", "", "", 200, KindDASH},
		{"progressive video", "https://x/v.mp4", "Media", "video/mp4", "", "", 200, KindVideo},
		{"video by extension only", "https://x/v.webm", "Media", "", "", "", 200, KindVideo},
		{"first range of a video", "https://x/v.mp4", "Media", "video/mp4", "", "bytes 0-1023/99999", 206, KindVideo},
		{"audio", "https://x/a.mp3", "Media", "audio/mpeg", "", "", 200, KindAudio},
		{"attachment", "https://x/get?id=1", "Document", "text/plain", `attachment; filename="r.txt"`, "", 200, KindFile},
		{"archive", "https://x/f.zip", "Document", "application/zip", "", "", 200, KindFile},
		{"octet-stream document", "https://x/blob", "Document", "application/octet-stream", "", "", 200, KindFile},

		{"ts segment", "https://x/seg1.ts", "Media", "video/mp2t", "", "", 200, ""},
		{"m4s segment", "https://x/seg1.m4s", "Fetch", "video/mp4", "", "", 200, ""},
		{"later range", "https://x/v.mp4", "Media", "video/mp4", "", "bytes 1024-2047/99999", 206, ""},
		{"image", "https://x/a.png", "Image", "image/png", "", "", 200, ""},
		{"html page", "https://x/", "Document", "text/html", "", "", 200, ""},
		{"script", "https://x/a.js", "Script", "application/javascript", "", "", 200, ""},
		{"json api", "https://x/api", "Fetch", "application/json", "", "", 200, ""},
		{"not found", "https://x/v.mp4", "Media", "video/mp4", "", "", 404, ""},
		{"redirect", "https://x/v.mp4", "Media", "video/mp4", "", "", 302, ""},
		{"video extension but text", "https://x/v.mp4", "Fetch", "text/html", "", "", 200, ""},
		{"octet-stream xhr", "https://x/data", "XHR", "application/octet-stream", "", "", 200, ""},
		{"inline disposition", "https://x/doc", "Document", "text/plain", "inline", "", 200, ""},
		{"bad url", "://", "Media", "video/mp4", "", "", 200, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := classify(tt.url, tt.rtype, tt.ctype, tt.disposition, tt.contentRange, tt.status)
			if ok != (tt.want != "") || got != tt.want {
				t.Errorf("classify = %q, %v; want %q", got, ok, tt.want)
			}
		})
	}
}

func TestHeaderHelpers(t *testing.T) {
	if got := dispositionFilename(`attachment; filename="dir\a b.zip"`); got != "a b.zip" {
		t.Errorf("dispositionFilename = %q", got)
	}
	if got := dispositionFilename("garbage;;"); got != "." && got != "" {
		t.Errorf("dispositionFilename(garbage) = %q", got)
	}
	for in, want := range map[string]int64{"bytes 0-9/100": 100, "bytes 0-9/*": -1, "": -1, "bytes 0-9/x": -1} {
		if got := totalFromRange(in); got != want {
			t.Errorf("totalFromRange(%q) = %d, want %d", in, got, want)
		}
	}
	if dedupeKey("https://x/a?b=1#frag") != "https://x/a?b=1" {
		t.Error("dedupeKey keeps the fragment")
	}
}

func TestCookieConversion(t *testing.T) {
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	in := &domain.Cookie{Domain: "example.com", Name: "sid", Value: "v", Path: "/p", Secure: true, HTTPOnly: true, SameSite: "lax", ExpiresAt: &exp}

	p := toCDP(in)
	if p.Domain != ".example.com" || p.SameSite != network.CookieSameSiteLax || float64(p.Expires) != float64(exp.Unix()) || !p.Secure || !p.HTTPOnly || p.PartitionKey != nil {
		t.Errorf("domain cookie -> %+v", p)
	}
	host := *in
	host.HostOnly = true
	host.PartitionKey = "https://top.example"
	host.ExpiresAt = nil
	p = toCDP(&host)
	if p.Domain != "example.com" || p.Expires != 0 || p.PartitionKey == nil || p.PartitionKey.TopLevelSite != "https://top.example" {
		t.Errorf("host cookie -> %+v", p)
	}

	back := fromCDP(&network.Cookie{Name: "a", Value: "b", Domain: ".Example.com", Path: "/", Expires: 1893553445.5, SameSite: network.CookieSameSiteStrict, Secure: true})
	if back.Domain != "example.com" || back.HostOnly || back.SameSite != "strict" || back.ExpiresAt == nil || back.ExpiresAt.UnixMilli() != 1893553445500 || !back.Secure {
		t.Errorf("domain cookie <- %+v", back)
	}
	back = fromCDP(&network.Cookie{
		Name: "a", Domain: "host.example", Path: "/", Expires: -1, Session: true,
		PartitionKey: &network.CookiePartitionKey{TopLevelSite: "https://top.example"},
	})
	if !back.HostOnly || back.ExpiresAt != nil || back.PartitionKey != "https://top.example" {
		t.Errorf("session host cookie <- %+v", back)
	}
	if fromCDP(&network.Cookie{Name: "a", Domain: "x", PartitionKeyOpaque: true}) != nil {
		t.Error("an opaquely partitioned cookie was converted")
	}
	if got := fromCDP(&network.Cookie{Name: "a", Domain: "x", Path: "/", Expires: 1e18}); got.ExpiresAt != nil {
		t.Errorf("an absurd expiry was kept: %v", got.ExpiresAt)
	}
}
