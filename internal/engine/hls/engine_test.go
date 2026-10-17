package hls

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/ffmpeg"
	"github.com/sabadia/dlm-server/internal/netclient"
	"github.com/sabadia/dlm-server/internal/settings"
)

// ---- fakes ----

type fakeFF struct{ err error }

func (f fakeFF) Locate(context.Context) (ffmpeg.Binary, error) {
	if f.err != nil {
		return ffmpeg.Binary{}, f.err
	}
	return ffmpeg.Binary{Path: "/fake/ffmpeg", Version: "1", Source: "path"}, nil
}

// fakeExec records ffmpeg invocations and "muxes" by concatenating the inputs.
type fakeExec struct {
	mu    sync.Mutex
	calls [][]string
	fail  error
}

func (f *fakeExec) run(_ context.Context, bin string, args []string) error {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{bin}, args...))
	f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	var out bytes.Buffer
	for i, a := range args {
		if a == "-i" {
			b, err := os.ReadFile(args[i+1])
			if err != nil {
				return err
			}
			out.Write(b)
		}
	}
	return os.WriteFile(args[len(args)-1], out.Bytes(), 0o644)
}

func (f *fakeExec) last() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

// stream is an origin with named resources and request counting.
type stream struct {
	t    *testing.T
	srv  *httptest.Server
	mu   sync.Mutex
	res  map[string]func(w http.ResponseWriter, r *http.Request)
	hits map[string]int
}

func newStream(t *testing.T) *stream {
	t.Helper()
	s := &stream{t: t, res: map[string]func(http.ResponseWriter, *http.Request){}, hits: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		h := s.res[r.URL.Path]
		s.mu.Unlock()
		if h == nil {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stream) handle(path string, h func(http.ResponseWriter, *http.Request)) {
	s.mu.Lock()
	s.res[path] = h
	s.mu.Unlock()
}

func (s *stream) bytes(path string, data []byte) {
	s.handle(path, func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	})
}

func (s *stream) text(path, body string) {
	s.handle(path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = fmt.Fprint(w, body)
	})
}

func (s *stream) hitCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func (s *stream) url(path string) string { return s.srv.URL + path }

// tokenOf reads a string stored in an atomic.Value.
func tokenOf(v *atomic.Value) string {
	s, _ := v.Load().(string)
	return s
}

func seg(i int) []byte { return bytes.Repeat([]byte{byte('a' + i%26)}, 300+i) }

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// vod serves n TS-like segments /seg0.ts… and the playlist /index.m3u8.
func (s *stream) vod(n int) []byte {
	var pl strings.Builder
	pl.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
	var all [][]byte
	for i := range n {
		fmt.Fprintf(&pl, "#EXTINF:4.0,\nseg%d.ts\n", i)
		s.bytes(fmt.Sprintf("/seg%d.ts", i), seg(i))
		all = append(all, seg(i))
	}
	pl.WriteString("#EXT-X-ENDLIST\n")
	s.text("/index.m3u8", pl.String())
	return concat(all...)
}

type env struct {
	e   *Engine
	ex  *fakeExec
	dir string
}

func newEnv(t *testing.T, ff FFmpeg, opts ...Option) *env {
	t.Helper()
	clients := netclient.New(netclient.Options{})
	t.Cleanup(func() { _ = clients.Close() })
	ex := &fakeExec{}
	noSleep := func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	all := append([]Option{WithExec(ex.run), WithRetrySleeper(noSleep), WithCheckpointInterval(10 * time.Millisecond)}, opts...)
	return &env{e: New(engine.Env{Clients: clients}, ff, all...), ex: ex, dir: t.TempDir()}
}

func (v *env) settings() settings.Settings {
	s := settings.Defaults(v.dir)
	s.Proxy = settings.Some(settings.Proxy{Mode: settings.ProxyNone})
	s.Retry.MaxAttempts = settings.Some(2)
	s.MaxConnections = settings.Some(3)
	return s
}

type recorder struct {
	mu  sync.Mutex
	cps []engine.Checkpoint
}

func (r *recorder) Checkpoint(_ context.Context, cp engine.Checkpoint) error {
	r.mu.Lock()
	r.cps = append(r.cps, cp)
	r.mu.Unlock()
	return nil
}

func (r *recorder) last() engine.Checkpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cps) == 0 {
		return engine.Checkpoint{}
	}
	return r.cps[len(r.cps)-1]
}

func (v *env) spec(rawURL, name string, state []byte, cp engine.Checkpointer, mutate ...func(*engine.Spec)) engine.Spec {
	sp := engine.Spec{TaskID: "t1", Source: domain.Source{CurrentURL: rawURL}, Path: filepath.Join(v.dir, name), Settings: v.settings(), EngineState: state, Checkpointer: cp}
	for _, m := range mutate {
		m(&sp)
	}
	return sp
}

func (v *env) run(ctx context.Context, sp engine.Spec) (engine.Download, error) {
	dl, err := v.e.NewDownload(sp)
	if err != nil {
		return nil, err
	}
	return dl, dl.Run(ctx)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func wantKind(t *testing.T, err error, kind engine.Kind) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a %v error, got nil", kind)
	}
	if got := engine.Classify(err); got != kind {
		t.Fatalf("Classify(%v) = %v, want %v", err, got, kind)
	}
}

// ---- tests ----

func TestMatchAndSuggestName(t *testing.T) {
	e := New(engine.Env{}, nil)
	for u, want := range map[string]bool{
		"https://x/a/index.m3u8": true, "http://x/A.M3U8?token=1": true, "https://x/a.mp4": false, "ftp://x/a.m3u8": false, "dlm-browser://r/0": false, "": false,
	} {
		if got := e.Match(domain.Source{CurrentURL: u}); got != want {
			t.Errorf("Match(%q) = %v, want %v", u, got, want)
		}
	}
	for u, want := range map[string]string{
		"https://x/videos/my-show-ep1/index.m3u8": "my-show-ep1.mp4",
		"https://x/live/stream.m3u8":              "x.mp4", // all generic: falls to the host
		"https://cdn.example/a/Big%20Movie.m3u8":  "Big Movie.mp4",
		"https://cdn.example/master.m3u8":         "cdn.example.mp4",
		"https://x/a/b/c/playlist.m3u8?x=1":       "c.mp4",
		"https://x/we:ird*/index.m3u8":            "we_ird_.mp4",
	} {
		if got := suggestName(u); got != want {
			t.Errorf("suggestName(%q) = %q, want %q", u, got, want)
		}
	}
}

func TestDownloadTSAsTSWithoutFFmpeg(t *testing.T) {
	o := newStream(t)
	want := o.vod(5)
	v := newEnv(t, nil)
	rec := &recorder{}
	dl, err := v.run(t.Context(), v.spec(o.url("/index.m3u8"), "out.ts", nil, rec))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(v.dir, "out.ts")); !bytes.Equal(got, want) {
		t.Errorf("output is %d bytes, want %d concatenated segments", len(got), len(want))
	}
	if len(v.ex.calls) != 0 {
		t.Errorf("ffmpeg ran for a plain TS concatenation: %v", v.ex.calls)
	}
	for _, leftover := range []string{"out.ts.part", "out.ts.hls"} {
		if _, err := os.Stat(filepath.Join(v.dir, leftover)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind: %v", leftover, err)
		}
	}
	if p := dl.Progress(); p.Downloaded != int64(len(want)) || p.Size != p.Downloaded {
		t.Errorf("progress = %+v, want %d done and the size settled", p, len(want))
	}
	if info, err := os.Stat(filepath.Join(v.dir, "out.ts")); err != nil || info.Size() != dl.Progress().Downloaded {
		t.Errorf("the final progress must be the size of the result: %v", err)
	}
	if cp := rec.last(); cp.Downloaded != int64(len(want)) || cp.Size != cp.Downloaded || len(cp.EngineState) == 0 {
		t.Errorf("last checkpoint = %+v", cp)
	}
}

func TestDownloadMergesWithFFmpeg(t *testing.T) {
	o := newStream(t)
	want := o.vod(4)
	v := newEnv(t, fakeFF{})
	if _, err := v.run(t.Context(), v.spec(o.url("/index.m3u8"), "movie.mp4", nil, nil)); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(v.dir, "movie.mp4")); !bytes.Equal(got, want) {
		t.Errorf("output is %d bytes, want %d", len(got), len(want))
	}
	args := v.ex.last()
	if len(args) == 0 || args[0] != "/fake/ffmpeg" {
		t.Fatalf("ffmpeg calls = %v", v.ex.calls)
	}
	joined := strings.Join(args, " ")
	for _, frag := range []string{"-c copy", "-f mp4", "-movflags +faststart", "-map 0:v? -map 0:a?"} {
		if !strings.Contains(joined, frag) {
			t.Errorf("ffmpeg arguments lack %q: %s", frag, joined)
		}
	}
	if strings.Count(joined, " -i ") != 1 || strings.Contains(joined, "-map 1:a?") || !strings.HasSuffix(joined, "movie.mp4.part") {
		t.Errorf("ffmpeg arguments = %s", joined)
	}
}

func TestMissingFFmpegFailsBeforeDownloading(t *testing.T) {
	o := newStream(t)
	o.vod(3)
	for name, ff := range map[string]FFmpeg{"none configured": nil, "not found": fakeFF{err: ffmpeg.ErrNotFound}} {
		t.Run(name, func(t *testing.T) {
			v := newEnv(t, ff)
			before := o.hitCount("/seg0.ts")
			_, err := v.run(t.Context(), v.spec(o.url("/index.m3u8"), "movie.mp4", nil, nil))
			if !errors.Is(err, ErrFFmpeg) {
				t.Fatalf("err = %v, want ErrFFmpeg", err)
			}
			wantKind(t, err, engine.KindFatal)
			if o.hitCount("/seg0.ts") != before {
				t.Error("segments were downloaded although ffmpeg is missing")
			}
		})
	}
}

func TestMasterVariantAndSeparateAudio(t *testing.T) {
	o := newStream(t)
	o.text("/master.m3u8", `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="en",DEFAULT=YES,URI="audio.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=640x360,AUDIO="a"
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,AUDIO="a"
high.m3u8
`)
	media := func(path, prefix string, n int) []byte {
		var pl strings.Builder
		pl.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
		var all [][]byte
		for i := range n {
			fmt.Fprintf(&pl, "#EXTINF:4,\n%s%d.ts\n", prefix, i)
			data := bytes.Repeat([]byte(prefix[:1]), 100+i)
			o.bytes(fmt.Sprintf("/%s%d.ts", prefix, i), data)
			all = append(all, data)
		}
		pl.WriteString("#EXT-X-ENDLIST\n")
		o.text(path, pl.String())
		return concat(all...)
	}
	media("/low.m3u8", "L", 2)
	high := media("/high.m3u8", "H", 3)
	audio := media("/audio.m3u8", "A", 3)

	v := newEnv(t, fakeFF{})
	sp := v.spec(o.url("/master.m3u8"), "m.mp4", nil, nil, func(s *engine.Spec) { s.Source.MediaHint = "720p" })
	if _, err := v.run(t.Context(), sp); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(v.dir, "m.mp4")); !bytes.Equal(got, concat(high, audio)) {
		t.Errorf("output is %d bytes, want the 720p video followed by the audio (%d)", len(got), len(high)+len(audio))
	}
	joined := strings.Join(v.ex.last(), " ")
	if strings.Count(joined, " -i ") != 2 || !strings.Contains(joined, "-map 1:a?") {
		t.Errorf("ffmpeg arguments = %s", joined)
	}
	if o.hitCount("/L0.ts") != 0 {
		t.Error("the low variant was downloaded")
	}
}

func aesEncrypt(t *testing.T, plain, key, iv []byte) []byte {
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

func TestEncryptedSegments(t *testing.T) {
	o := newStream(t)
	key := bytes.Repeat([]byte{0x42}, 16)
	o.bytes("/k.key", key)
	explicitIV := bytes.Repeat([]byte{9}, 16)
	// Segments 0-1 use the explicit IV, 2-3 the sequence-number IV, 4 is clear.
	var pl strings.Builder
	pl.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:10\n")
	pl.WriteString(`#EXT-X-KEY:METHOD=AES-128,URI="k.key",IV=0x09090909090909090909090909090909` + "\n")
	var want [][]byte
	for i := range 5 {
		seq := 10 + i
		if i == 2 {
			pl.WriteString("#EXT-X-KEY:METHOD=AES-128,URI=\"k.key\"\n")
		}
		if i == 4 {
			pl.WriteString("#EXT-X-KEY:METHOD=NONE\n")
		}
		fmt.Fprintf(&pl, "#EXTINF:4,\ne%d.ts\n", i)
		plain := seg(i)
		want = append(want, plain)
		switch {
		case i < 2:
			o.bytes(fmt.Sprintf("/e%d.ts", i), aesEncrypt(t, plain, key, explicitIV))
		case i < 4:
			o.bytes(fmt.Sprintf("/e%d.ts", i), aesEncrypt(t, plain, key, segmentedIV(uint64(seq))))
		default:
			o.bytes(fmt.Sprintf("/e%d.ts", i), plain)
		}
	}
	pl.WriteString("#EXT-X-ENDLIST\n")
	o.text("/enc.m3u8", pl.String())
	v := newEnv(t, nil)
	if _, err := v.run(t.Context(), v.spec(o.url("/enc.m3u8"), "enc.ts", nil, nil)); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(v.dir, "enc.ts")); !bytes.Equal(got, concat(want...)) {
		t.Errorf("decrypted output differs (%d bytes, want %d)", len(got), len(concat(want...)))
	}
	if n := o.hitCount("/k.key"); n != 1 {
		t.Errorf("the key was fetched %d times, want once", n)
	}

	// A key of the wrong size is refused.
	o.bytes("/k.key", []byte("short"))
	v2 := newEnv(t, nil)
	if _, err := v2.run(t.Context(), v2.spec(o.url("/enc.m3u8"), "enc.ts", nil, nil)); err == nil || engine.Classify(err) != engine.KindFatal {
		t.Errorf("wrong key size: err = %v, want a fatal error", err)
	}
}

func TestFMP4WithInitSectionAndByteRanges(t *testing.T) {
	o := newStream(t)
	initData := []byte("INITINITINIT")
	frags := [][]byte{[]byte("frag-zero"), []byte("frag-one!!"), []byte("frag-two")}
	// One file holds the init section and all fragments, addressed by byte ranges.
	file := concat(initData, frags[0], frags[1], frags[2])
	var rangesSeen []string
	var mu sync.Mutex
	o.handle("/media.mp4", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		rangesSeen = append(rangesSeen, r.Header.Get("Range"))
		mu.Unlock()
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(file))
	})
	off := len(initData)
	o.text("/fmp4.m3u8", fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-MAP:URI="media.mp4",BYTERANGE="%d@0"
#EXTINF:4,
#EXT-X-BYTERANGE:%d@%d
media.mp4
#EXTINF:4,
#EXT-X-BYTERANGE:%d
media.mp4
#EXTINF:4,
#EXT-X-BYTERANGE:%d
media.mp4
#EXT-X-ENDLIST
`, len(initData), len(frags[0]), off, len(frags[1]), len(frags[2])))
	v := newEnv(t, fakeFF{})
	if _, err := v.run(t.Context(), v.spec(o.url("/fmp4.m3u8"), "f.mp4", nil, nil)); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(v.dir, "f.mp4")); !bytes.Equal(got, file) {
		t.Errorf("output = %q, want the init section once, then the fragments in order (%q)", got, file)
	}
	slices.Sort(rangesSeen)
	want := []string{
		fmt.Sprintf("bytes=0-%d", len(initData)-1),
		fmt.Sprintf("bytes=%d-%d", off, off+len(frags[0])-1),
		fmt.Sprintf("bytes=%d-%d", off+len(frags[0]), off+len(frags[0])+len(frags[1])-1),
		fmt.Sprintf("bytes=%d-%d", off+len(frags[0])+len(frags[1]), off+len(file)-off-1),
	}
	slices.Sort(want)
	if !slices.Equal(rangesSeen, want) {
		t.Errorf("ranges requested = %v, want %v", rangesSeen, want)
	}
}

func TestResumeFetchesOnlyWhatIsMissing(t *testing.T) {
	o := newStream(t)
	want := o.vod(8)
	v := newEnv(t, nil, WithCheckpointInterval(5*time.Millisecond))
	rec := &recorder{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Pause the first run once a few segments were served.
	var served atomic.Int32
	for i := range 8 {
		path := fmt.Sprintf("/seg%d.ts", i)
		o.handle(path, func(w http.ResponseWriter, r *http.Request) {
			if served.Add(1) == 4 {
				cancel()
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(seg(i)))
		})
	}
	sp := v.spec(o.url("/index.m3u8"), "r.ts", nil, rec)
	sp.Settings.MaxConnections = settings.Some(1) // sequential, so "4 served" means "3 stored"
	_, err := v.run(ctx, sp)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first run = %v, want context.Canceled", err)
	}
	cp := rec.last()
	if cp.Downloaded == 0 || len(cp.EngineState) == 0 {
		t.Fatalf("no resume state after the pause: %+v", cp)
	}
	if _, err := os.Stat(filepath.Join(v.dir, "r.ts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an unfinished download produced the destination file")
	}
	hitsAfterFirst := map[int]int{}
	for i := range 8 {
		hitsAfterFirst[i] = o.hitCount(fmt.Sprintf("/seg%d.ts", i))
	}

	// Resume with a plain handler: only the missing segments are requested.
	for i := range 8 {
		o.bytes(fmt.Sprintf("/seg%d.ts", i), seg(i))
	}
	sp2 := v.spec(o.url("/index.m3u8"), "r.ts", cp.EngineState, nil)
	if _, err := v.run(t.Context(), sp2); err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if got := readFile(t, filepath.Join(v.dir, "r.ts")); !bytes.Equal(got, want) {
		t.Errorf("resumed output is %d bytes, want %d", len(got), len(want))
	}
	refetched := 0
	for i := range 8 {
		if hitsAfterFirst[i] > 0 && o.hitCount(fmt.Sprintf("/seg%d.ts", i)) > hitsAfterFirst[i] && i < 3 {
			refetched++
		}
	}
	if refetched > 0 {
		t.Errorf("%d finished segments were fetched again", refetched)
	}
}

func TestResumeWithAChangedStream(t *testing.T) {
	o := newStream(t)
	o.vod(4)
	v := newEnv(t, nil)
	rec := &recorder{}
	if _, err := v.run(t.Context(), v.spec(o.url("/index.m3u8"), "c.ts", nil, rec)); err != nil {
		t.Fatal(err)
	}
	state := rec.last().EngineState
	// The "same" link now serves a different, longer stream.
	want := o.vod(6)
	if err := os.Remove(filepath.Join(v.dir, "c.ts")); err != nil {
		t.Fatal(err)
	}
	_, err := v.run(t.Context(), v.spec(o.url("/index.m3u8"), "c.ts", state, nil))
	if !errors.Is(err, ErrContentChanged) {
		t.Fatalf("err = %v, want ErrContentChanged", err)
	}
	wantKind(t, err, engine.KindLinkExpired)

	// With onMismatch=restart the new stream is downloaded from scratch.
	sp := v.spec(o.url("/index.m3u8"), "c.ts", state, nil, func(s *engine.Spec) {
		s.Settings.Refresh.OnMismatch = settings.Some(settings.MismatchRestart)
	})
	if _, err := v.run(t.Context(), sp); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(v.dir, "c.ts")); !bytes.Equal(got, want) {
		t.Errorf("restarted output is %d bytes, want %d", len(got), len(want))
	}
}

func TestSegmentURLsAreRefreshedFromTheplaylist(t *testing.T) {
	o := newStream(t)
	var token atomic.Value
	token.Store("t1")
	segs := [][]byte{seg(0), seg(1), seg(2)}
	for i := range segs {
		o.handle(fmt.Sprintf("/s%d.ts", i), func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("tok") != tokenOf(&token) {
				http.Error(w, "expired", http.StatusForbidden)
				return
			}
			_, _ = w.Write(segs[i])
		})
	}
	o.handle("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		tok := tokenOf(&token)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\ns0.ts?tok=%[1]s\n#EXTINF:4,\ns1.ts?tok=%[1]s\n#EXTINF:4,\ns2.ts?tok=%[1]s\n#EXT-X-ENDLIST\n", tok)
	})
	v := newEnv(t, nil)
	sp := v.spec(o.url("/index.m3u8"), "tok.ts", nil, nil)
	sp.Settings.MaxConnections = settings.Some(1)
	// The tokens rotate once the playlist was read: the segment URLs in the
	// engine's copy are stale, a reload yields the new ones.
	dl, err := v.e.NewDownload(sp)
	if err != nil {
		t.Fatal(err)
	}
	o.handle("/hook", func(http.ResponseWriter, *http.Request) {})
	go func() {
		// Rotate right after the first playlist fetch.
		for o.hitCount("/index.m3u8") == 0 {
			time.Sleep(time.Millisecond)
		}
		token.Store("t2")
	}()
	if err := dl.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := readFile(t, filepath.Join(v.dir, "tok.ts")); !bytes.Equal(got, concat(segs...)) {
		t.Errorf("output differs: %d bytes", len(got))
	}
	if o.hitCount("/index.m3u8") < 2 {
		t.Error("the playlist was not reloaded after a segment link expired")
	}
}

func TestExpiredLinksAreReported(t *testing.T) {
	t.Run("segments refused and the playlist has no fresh ones", func(t *testing.T) {
		o := newStream(t)
		o.vod(3)
		o.handle("/seg1.ts", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "gone", http.StatusForbidden) })
		v := newEnv(t, nil)
		_, err := v.run(t.Context(), v.spec(o.url("/index.m3u8"), "x.ts", nil, nil))
		wantKind(t, err, engine.KindLinkExpired)
		if _, statErr := os.Stat(filepath.Join(v.dir, "x.ts")); !errors.Is(statErr, os.ErrNotExist) {
			t.Error("a failed download produced the destination file")
		}
	})
	t.Run("playlist refused", func(t *testing.T) {
		o := newStream(t)
		o.handle("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusForbidden) })
		v := newEnv(t, nil)
		_, err := v.run(t.Context(), v.spec(o.url("/index.m3u8"), "x.ts", nil, nil))
		wantKind(t, err, engine.KindLinkExpired)
	})
	t.Run("login page instead of a playlist", func(t *testing.T) {
		o := newStream(t)
		o.handle("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>please log in</html>"))
		})
		v := newEnv(t, nil)
		_, err := v.run(t.Context(), v.spec(o.url("/index.m3u8"), "x.ts", nil, nil))
		wantKind(t, err, engine.KindLinkExpired)
	})
	t.Run("server errors are transient and retried", func(t *testing.T) {
		o := newStream(t)
		o.vod(2)
		var fails atomic.Int32
		o.handle("/seg0.ts", func(w http.ResponseWriter, _ *http.Request) {
			if fails.Add(1) == 1 {
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write(seg(0))
		})
		v := newEnv(t, nil)
		if _, err := v.run(t.Context(), v.spec(o.url("/index.m3u8"), "x.ts", nil, nil)); err != nil {
			t.Fatalf("a transient failure was not retried: %v", err)
		}
	})
}

func TestUnsupportedAndInvalidPlaylists(t *testing.T) {
	o := newStream(t)
	o.text("/sample.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"k\"\n#EXTINF:4,\na.ts\n#EXT-X-ENDLIST\n")
	o.text("/empty.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-ENDLIST\n")
	o.text("/junk.m3u8", "this is not a playlist")
	v := newEnv(t, nil)
	for path, want := range map[string]error{"/sample.m3u8": errSampleAES, "/empty.m3u8": ErrNoSegments} {
		_, err := v.run(t.Context(), v.spec(o.url(path), "x.ts", nil, nil))
		if !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", path, err, want)
		}
		wantKind(t, err, engine.KindFatal)
	}
	if _, err := v.run(t.Context(), v.spec(o.url("/junk.m3u8"), "x.ts", nil, nil)); !errors.Is(err, errNotPlaylist) {
		t.Errorf("junk: err = %v", err)
	}
}

func TestDestinationExistsAndBadSpecs(t *testing.T) {
	o := newStream(t)
	o.vod(2)
	v := newEnv(t, nil)
	dest := filepath.Join(v.dir, "x.ts")
	if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := v.run(t.Context(), v.spec(o.url("/index.m3u8"), "x.ts", nil, nil)); !errors.Is(err, ErrDestinationExists) {
		t.Errorf("err = %v, want ErrDestinationExists", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "old" {
		t.Errorf("the existing file was overwritten: %q", got)
	}
	if _, err := v.e.NewDownload(engine.Spec{Source: domain.Source{CurrentURL: o.url("/index.m3u8")}, Path: "relative.ts"}); err == nil {
		t.Error("a relative path was accepted")
	}
	if _, err := v.e.NewDownload(engine.Spec{Source: domain.Source{CurrentURL: "https://x/a.mp4"}, Path: filepath.Join(v.dir, "a")}); err == nil {
		t.Error("a non-HLS source was accepted")
	}
	dl, _ := v.e.NewDownload(v.spec(o.url("/index.m3u8"), "y.ts", nil, nil))
	if err := dl.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := dl.Run(t.Context()); err == nil {
		t.Error("a second Run succeeded")
	}
}

func TestResolve(t *testing.T) {
	o := newStream(t)
	o.vod(3)
	o.text("/master.m3u8", "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000,RESOLUTION=640x360\nindex.m3u8\n")
	v := newEnv(t, nil)
	probe, err := v.e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: o.url("/master.m3u8")}, Settings: v.settings()})
	if err != nil {
		t.Fatal(err)
	}
	if probe.Engine != EngineName || probe.Suggested == "" || !strings.HasSuffix(probe.Suggested, ".mp4") || probe.Resource.Size != -1 || len(probe.Resource.Files) != 1 {
		t.Errorf("probe = %+v", probe)
	}
	if _, err := v.e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: o.url("/master.m3u8"), MediaHint: "nope"}, Settings: v.settings()}); err == nil {
		t.Error("an unmatched media hint was accepted")
	}
	if _, err := v.e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: o.url("/missing.m3u8")}, Settings: v.settings()}); err == nil {
		t.Error("a missing playlist resolved")
	}
}

func TestLiveStreamIsRecordedUntilItEnds(t *testing.T) {
	o := newStream(t)
	var polls atomic.Int32
	segData := func(i int) []byte { return seg(i + 40) }
	for i := range 6 {
		o.bytes(fmt.Sprintf("/l%d.ts", i), segData(i))
	}
	// The playlist reveals two more segments per poll; the third poll ends it.
	o.handle("/live.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		n := min(int(polls.Add(1))*2, 6)
		var pl strings.Builder
		pl.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
		for i := range n {
			fmt.Fprintf(&pl, "#EXTINF:1,\nl%d.ts\n", i)
		}
		if n == 6 {
			pl.WriteString("#EXT-X-ENDLIST\n")
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = fmt.Fprint(w, pl.String())
	})
	v := newEnv(t, nil, WithLiveWait(func(time.Duration) time.Duration { return time.Millisecond }))
	dl, err := v.run(t.Context(), v.spec(o.url("/live.m3u8"), "live.ts", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	var want [][]byte
	for i := range 6 {
		want = append(want, segData(i))
	}
	if got := readFile(t, filepath.Join(v.dir, "live.ts")); !bytes.Equal(got, concat(want...)) {
		t.Errorf("recorded %d bytes, want all six segments (%d)", len(got), len(concat(want...)))
	}
	if p := dl.Progress(); p.Size != p.Downloaded {
		t.Errorf("progress = %+v", p)
	}
}

func TestLiveStreamThatStallsIsFinished(t *testing.T) {
	o := newStream(t)
	for i := range 2 {
		o.bytes(fmt.Sprintf("/l%d.ts", i), seg(i))
	}
	o.text("/live.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nl0.ts\n#EXTINF:1,\nl1.ts\n")
	v := newEnv(t, nil, WithLiveWait(func(time.Duration) time.Duration { return 5 * time.Millisecond }))
	var sizes []int64
	rec := &recorder{}
	if _, err := v.run(t.Context(), v.spec(o.url("/live.m3u8"), "stall.ts", nil, rec)); err != nil {
		t.Fatal(err)
	}
	_ = sizes
	if got := readFile(t, filepath.Join(v.dir, "stall.ts")); !bytes.Equal(got, concat(seg(0), seg(1))) {
		t.Errorf("recorded %d bytes", len(got))
	}
	if prog := rec.last(); prog.Size != prog.Downloaded {
		t.Errorf("final checkpoint = %+v, want the size settled", prog)
	}
}

func TestLiveProgressHasNoSize(t *testing.T) {
	o := newStream(t)
	o.bytes("/l0.ts", seg(0))
	o.text("/live.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nl0.ts\n")
	v := newEnv(t, nil, WithLiveWait(func(time.Duration) time.Duration { return 20 * time.Millisecond }))
	dl, err := v.e.NewDownload(v.spec(o.url("/live.m3u8"), "p.ts", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_ = dl.Run(ctx)
	if p := dl.Progress(); p.Size != -1 && p.Size != p.Downloaded {
		t.Errorf("a live stream's size is unknown, got %+v", p)
	}
}

func TestVODProgressEstimatesTheSize(t *testing.T) {
	o := newStream(t)
	o.vod(4)
	gate := make(chan struct{})
	var once sync.Once
	var served atomic.Int32
	for i := range 4 {
		o.handle(fmt.Sprintf("/seg%d.ts", i), func(w http.ResponseWriter, _ *http.Request) {
			if served.Add(1) == 3 {
				once.Do(func() { close(gate) })
				<-time.After(300 * time.Millisecond)
			}
			_, _ = w.Write(seg(i))
		})
	}
	v := newEnv(t, nil)
	sp := v.spec(o.url("/index.m3u8"), "e.ts", nil, nil)
	sp.Settings.MaxConnections = settings.Some(1)
	dl, _ := v.e.NewDownload(sp)
	done := make(chan error, 1)
	go func() { done <- dl.Run(t.Context()) }()
	<-gate
	p := dl.Progress()
	if p.Downloaded == 0 || p.Size <= p.Downloaded-1 {
		t.Errorf("mid-download progress = %+v, want an estimate at least as large as what is done", p)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFFmpegFailureIsFatalAndLeavesNoOutput(t *testing.T) {
	o := newStream(t)
	o.vod(2)
	v := newEnv(t, fakeFF{})
	v.ex.fail = errors.New("ffmpeg: Invalid data found")
	_, err := v.run(t.Context(), v.spec(o.url("/index.m3u8"), "bad.mp4", nil, nil))
	wantKind(t, err, engine.KindFatal)
	if !strings.Contains(err.Error(), "Invalid data") {
		t.Errorf("err = %v, want ffmpeg's complaint", err)
	}
	for _, name := range []string{"bad.mp4", "bad.mp4.part"} {
		if _, statErr := os.Stat(filepath.Join(v.dir, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s exists after a failed merge", name)
		}
	}
	// The downloaded segments are kept, so a retry only merges again.
	if _, statErr := os.Stat(filepath.Join(v.dir, "bad.mp4.hls")); statErr != nil {
		t.Errorf("the segments were removed: %v", statErr)
	}
}

// segmentedIV is the IV HLS derives from a segment's sequence number.
func segmentedIV(seq uint64) []byte {
	iv := make([]byte, 16)
	binary.BigEndian.PutUint64(iv[8:], seq)
	return iv
}
