package hls

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"net/url"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
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
#EXT-X-KEY:METHOD=AES-128,URI="keys/k1.key",IV=0x01
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
	if m == nil || !m.closed || m.target != 6 || m.seq != 100 || len(m.segments) != 4 {
		t.Fatalf("media = %+v", m)
	}
	s := m.segments
	if s[0].seq != 100 || s[0].ref.url != "https://cdn.example/path/live/main.mp4" || *s[0].ref.rng != (byteRange{0, 1000}) {
		t.Errorf("segment 0 = %+v", s[0])
	}
	if *s[1].ref.rng != (byteRange{1000, 2000}) {
		t.Errorf("an omitted offset must continue the previous range, got %+v", s[1].ref.rng)
	}
	if s[0].key == nil || s[0].key.url != "https://cdn.example/path/live/keys/k1.key" || s[0].key.method != "AES-128" ||
		!bytes.Equal(s[0].key.iv, append(make([]byte, 15), 1)) || s[1].key == nil {
		t.Errorf("keys = %+v / %+v", s[0].key, s[1].key)
	}
	if s[2].key != nil || s[3].key != nil {
		t.Errorf("METHOD=NONE must clear the key: %+v %+v", s[2].key, s[3].key)
	}
	if s[0].init == nil || s[0].init.url != "https://cdn.example/path/live/init.mp4" || s[2].init.url != s[0].init.url {
		t.Errorf("init = %+v / %+v", s[0].init, s[2].init)
	}
	if s[2].ref.url != "https://cdn.example/abs/s3.m4s" || s[3].ref.url != "https://other.example/s4.m4s?x=1" {
		t.Errorf("urls = %q %q", s[2].ref.url, s[3].ref.url)
	}
	if s[3].init == nil || s[3].init.url != "https://cdn.example/path/live/init2.mp4" || *s[3].init.rng != (byteRange{10, 720}) || !s[3].disc {
		t.Errorf("segment 3 = %+v init %+v", s[3], s[3].init)
	}
	if got := m.duration(); got != 21.5 {
		t.Errorf("duration = %v", got)
	}
}

func TestParseLivePlaylist(t *testing.T) {
	p := mustParse(t, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:7\n#EXTINF:4,\na.ts\n#EXTINF:4,\nb.ts\n", "https://x/live.m3u8")
	if p.media.closed || len(p.media.segments) != 2 || p.media.segments[1].seq != 8 {
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
	if p, err := parse([]byte(drm), base); err != nil || p.media.segments[0].key != nil {
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
	k := &keyInfo{}
	iv := ivFor(k, 258)
	if len(iv) != 16 || iv[14] != 1 || iv[15] != 2 || iv[0] != 0 {
		t.Errorf("sequence IV = %x", iv)
	}
	explicit := &keyInfo{iv: bytes.Repeat([]byte{7}, 16)}
	if !bytes.Equal(ivFor(explicit, 5), explicit.iv) {
		t.Error("an explicit IV was not used")
	}
}

func encrypt(t *testing.T, plain, key, iv []byte) []byte {
	t.Helper()
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(bytes.Clone(plain), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out
}

func TestDecryptAES128(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 16)
	iv := ivFor(&keyInfo{}, 9)
	for _, n := range []int{0, 1, 15, 16, 17, 1000} {
		plain := bytes.Repeat([]byte{0xAB}, n)
		got, err := decryptAES128(encrypt(t, plain, key, iv), key, iv)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("round trip of %d bytes: %v", n, err)
		}
	}
	enc := encrypt(t, []byte("secret segment"), key, iv)
	if _, err := decryptAES128(enc, bytes.Repeat([]byte{4}, 16), iv); err == nil {
		t.Error("a wrong key was accepted")
	}
	for name, tc := range map[string]struct{ data, key []byte }{
		"short key": {enc, key[:8]}, "empty": {nil, key}, "not a block multiple": {append(bytes.Clone(enc), 0), key},
	} {
		if _, err := decryptAES128(tc.data, tc.key, iv); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
