package hls

import (
	"bytes"
	"errors"
	"net/url"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine/segmented"
)

func mustParse(t *testing.T, src, base string) *playlist {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	p, err := parse([]byte(src), u)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return p
}

func TestParseMediaPlaylist(t *testing.T) {
	p := mustParse(t, `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:100
#EXT-X-MAP:URI="init.mp4"
#EXT-X-KEY:METHOD=AES-128,URI="keys/k1.Key",IV=0x01
#EXTINF:6.0,
#EXT-X-BYTERANGE:1000@0
main.mp4
#EXTINF:6.0,
#EXT-X-BYTERANGE:2000
main.mp4
#EXT-X-KEY:METHOD=NONE
#EXTINF:5.5,
/abs/s3.m4s
#EXT-X-DISCONTINUITY
#EXT-X-MAP:URI="init2.mp4",BYTERANGE="720@10"
#EXTINF:4,
https://other.example/s4.m4s?x=1
#EXT-X-ENDLIST
`, "https://cdn.example/path/live/index.m3u8?token=abc")
	m := p.media
	if m == nil || !m.Closed || m.Target != 6 || len(m.Segments) != 4 {
		t.Fatalf("media = %+v", m)
	}
	s := m.Segments
	if s[0].Seq != 100 || s[0].Ref.URL != "https://cdn.example/path/live/main.mp4" || *s[0].Ref.Range != (segmented.ByteRange{Offset: 0, Length: 1000}) {
		t.Errorf("segment 0 = %+v", s[0])
	}
	if *s[1].Ref.Range != (segmented.ByteRange{Offset: 1000, Length: 2000}) {
		t.Errorf("an omitted offset must continue the previous range, got %+v", s[1].Ref.Range)
	}
	if s[0].Key == nil || s[0].Key.URL != "https://cdn.example/path/live/keys/k1.Key" || s[0].Key.Method != "AES-128" ||
		!bytes.Equal(s[0].Key.IV, append(make([]byte, 15), 1)) || s[1].Key == nil {
		t.Errorf("keys = %+v / %+v", s[0].Key, s[1].Key)
	}
	if s[2].Key != nil || s[3].Key != nil {
		t.Errorf("METHOD=NONE must clear the key: %+v %+v", s[2].Key, s[3].Key)
	}
	if s[0].Init == nil || s[0].Init.URL != "https://cdn.example/path/live/init.mp4" || s[2].Init.URL != s[0].Init.URL {
		t.Errorf("init = %+v / %+v", s[0].Init, s[2].Init)
	}
	if s[2].Ref.URL != "https://cdn.example/abs/s3.m4s" || s[3].Ref.URL != "https://other.example/s4.m4s?x=1" {
		t.Errorf("urls = %q %q", s[2].Ref.URL, s[3].Ref.URL)
	}
	if s[3].Init == nil || s[3].Init.URL != "https://cdn.example/path/live/init2.mp4" || *s[3].Init.Range != (segmented.ByteRange{Offset: 10, Length: 720}) || !s[3].Disc {
		t.Errorf("segment 3 = %+v init %+v", s[3], s[3].Init)
	}
	if got := m.Duration(); got != 21.5 {
		t.Errorf("duration = %v", got)
	}
}

func TestParseLivePlaylist(t *testing.T) {
	p := mustParse(t, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:7\n#EXTINF:4,\na.ts\n#EXTINF:4,\nb.ts\n", "https://x/live.m3u8")
	if p.media.Closed || len(p.media.Segments) != 2 || p.media.Segments[1].Seq != 8 {
		t.Errorf("live playlist = %+v", p.media)
	}
}

func TestParseRejects(t *testing.T) {
	base, _ := url.Parse("https://x/a.m3u8")
	for name, src := range map[string]string{"empty": "", "html": "<html>login</html>", "no header": "a.ts\nb.ts\n"} {
		if _, err := parse([]byte(src), base); !errors.Is(err, errNotPlaylist) {
			t.Errorf("%s: err = %v, want errNotPlaylist", name, err)
		}
	}
	sample := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"k\"\n#EXTINF:4,\na.ts\n#EXT-X-ENDLIST\n"
	if _, err := parse([]byte(sample), base); !errors.Is(err, errSampleAES) {
		t.Errorf("SAMPLE-AES: err = %v, want errSampleAES", err)
	}
	// A key format other than identity (a DRM system) is ignored, not mistaken for AES-128.
	drm := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://k\",KEYFORMAT=\"com.apple.streamingkeydelivery\"\n#EXTINF:4,\na.ts\n#EXT-X-ENDLIST\n"
	if p, err := parse([]byte(drm), base); err != nil || p.media.Segments[0].Key != nil {
		t.Errorf("a DRM key format must be skipped: %v", err)
	}
}

const masterSrc = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="audio/en.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="French",LANGUAGE="fr",URI="audio/fr.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.4d401e",AUDIO="aud",NAME="low"
v360/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2800000,RESOLUTION=1280x720,CODECS="avc1.4d401f",AUDIO="aud"
v720/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080,CODECS="avc1.640028"
v1080/index.m3u8
#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=100000,URI="iframe.m3u8"
`

func TestParseMasterPlaylist(t *testing.T) {
	m := mustParse(t, masterSrc, "https://cdn.example/hls/master.m3u8").master
	if m == nil || len(m.variants) != 3 {
		t.Fatalf("master = %+v", m)
	}
	v := m.variants[1]
	if v.url != "https://cdn.example/hls/v720/index.m3u8" || v.bandwidth != 2800000 || v.width != 1280 || v.height != 720 || v.audio != "aud" {
		t.Errorf("variant = %+v", v)
	}
	if len(m.audio) != 2 || m.audio[0].url != "https://cdn.example/hls/audio/en.m3u8" || !m.audio[0].isDef {
		t.Errorf("audio = %+v", m.audio)
	}
	if a, ok := pickAudio(m, m.variants[1]); !ok || a.name != "English" {
		t.Errorf("pickAudio = %+v, %v; want the default rendition", a, ok)
	}
	if _, ok := pickAudio(m, m.variants[2]); ok {
		t.Error("a variant without an audio group got a separate audio rendition")
	}
}

func TestSelectVariant(t *testing.T) {
	m := mustParse(t, masterSrc, "https://cdn.example/hls/master.m3u8").master
	tests := []struct {
		hint, want string
	}{
		{"", "v1080"},
		{"best", "v1080"},
		{"worst", "v360"},
		{"720", "v720"},
		{"720p", "v720"},
		{"1080p", "v1080"},
		{"800", "v720"},  // nothing at or below 800 would be 360? no: 360 <= 800, the tallest below is 720
		{"480p", "v360"}, // the tallest not above 480
		{"240p", "v360"}, // all above: the lowest of them
		{"4000p", "v1080"},
		{"v720/", "v720"}, // substring of the URL
		{`v(360|720)/`, "v360"},
		{"low", "v360"},
	}
	for _, tt := range tests {
		t.Run(tt.hint, func(t *testing.T) {
			v, err := selectVariant(m.variants, tt.hint)
			if err != nil {
				t.Fatal(err)
			}
			if !contains(v.url, "/"+tt.want+"/") {
				t.Errorf("selectVariant(%q) = %s, want %s", tt.hint, v.url, tt.want)
			}
		})
	}
	if _, err := selectVariant(m.variants, "nothing-like-this"); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("unknown hint: %v, want ErrInvalidArgument", err)
	}
	if _, err := selectVariant(nil, ""); err == nil {
		t.Error("an empty master was accepted")
	}
	// Without resolutions a height hint cannot match.
	if _, err := selectVariant([]variant{{url: "a", bandwidth: 1}}, "720p"); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("height hint without resolutions: %v", err)
	}
}

func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }

func TestIV(t *testing.T) {
	if iv, err := parseIV("0x000102030405060708090a0b0c0d0e0f"); err != nil || iv[15] != 0x0f || iv[1] != 1 {
		t.Errorf("parseIV = %x, %v", iv, err)
	}
	for _, bad := range []string{"", "0xzz", "0x" + string(bytes.Repeat([]byte("1"), 33))} {
		if _, err := parseIV(bad); err == nil {
			t.Errorf("parseIV(%q) accepted", bad)
		}
	}
}

func TestFormatsListTheStreamsAndTheirIDsSelectThem(t *testing.T) {
	vs := []variant{
		{url: "https://cdn.example.com/720-lo.m3u8", bandwidth: 1_200_000, width: 1280, height: 720, codecs: "avc1.4d401f"},
		{url: "https://cdn.example.com/1080.m3u8", bandwidth: 5_000_000, width: 1920, height: 1080},
		{url: "https://cdn.example.com/720-hi.m3u8", bandwidth: 2_500_000, width: 1280, height: 720},
		{url: "https://cdn.example.com/audio.m3u8", bandwidth: 64_000},
	}
	got := formats(vs)
	if len(got) != 4 {
		t.Fatalf("formats = %+v", got)
	}
	// Best first; a height is the id when it is unique, else the label carries the bitrate.
	if got[0].ID != "1080p" || got[0].Label != "1080p" || got[0].Height != 1080 || got[0].Width != 1920 {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Label != "720p, 2.5 Mbit/s" || got[2].Label != "720p, 1.2 Mbit/s" || got[3].Label != "64 kbit/s" {
		t.Errorf("labels = %q %q %q", got[1].Label, got[2].Label, got[3].Label)
	}
	// Every id picks the stream it names.
	byID := map[string]string{
		got[0].ID: "https://cdn.example.com/1080.m3u8",
		got[1].ID: "https://cdn.example.com/720-hi.m3u8",
		got[2].ID: "https://cdn.example.com/720-lo.m3u8",
		got[3].ID: "https://cdn.example.com/audio.m3u8",
	}
	for id, want := range byID {
		v, err := selectVariant(vs, id)
		if err != nil || v.url != want {
			t.Errorf("selectVariant(%q) = %q, %v; want %q", id, v.url, err, want)
		}
	}
}
