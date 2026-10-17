package bt

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/ratelimit"
	"github.com/sabadia/dlm-server/internal/settings"
)

var multi = content{"a.bin": payload(300_000, 1), "sub/b.bin": payload(150_000, 2), "sub/deep/c.txt": []byte("hello torrent")}

func TestMatch(t *testing.T) {
	e := New(engine.Env{}, Config{})
	for u, want := range map[string]bool{
		"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567": true,
		"MAGNET:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567": true,
		"data:application/x-bittorrent;base64,AAAA":                    true,
		"https://example.com/files/a.torrent":                          true,
		"http://example.com/a.TORRENT?token=1":                         true,
		"https://example.com/a.mp4":                                    false,
		"ftp://example.com/a.torrent":                                  false,
		"data:text/plain;base64,AAAA":                                  false,
		"":                                                             false,
	} {
		if got := e.Match(domain.Source{CurrentURL: u}); got != want {
			t.Errorf("Match(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestResolveDataURL(t *testing.T) {
	sw := newSwarm(t, "My Show", multi, false)
	e := newEngine(t, testConfig())
	p, err := e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: sw.dataURL()}, Settings: testSettings()})
	if err != nil {
		t.Fatal(err)
	}
	if p.Engine != EngineName || p.Suggested != "My Show" || p.Resource.Name != "My Show" {
		t.Errorf("probe = %+v", p)
	}
	var total int64
	byName := map[string]domain.File{}
	for _, f := range p.Resource.Files {
		total += f.Size
		byName[f.Path+"/"+f.Name] = f
		if !f.Selected {
			t.Errorf("file %q is not selected by default", f.Name)
		}
	}
	if p.Resource.Size != total || total != int64(300_000+150_000+len("hello torrent")) || len(p.Resource.Files) != 3 {
		t.Errorf("resource = %+v", p.Resource)
	}
	if f, ok := byName["sub/deep/c.txt"]; !ok || f.Size != int64(len("hello torrent")) {
		t.Errorf("files = %+v", p.Resource.Files)
	}
	if fp := p.Source.Fingerprint; fp.Size != total || fp.Filename != "My Show" {
		t.Errorf("fingerprint = %+v", fp)
	}

	single := newSwarm(t, "one.bin", content{"": payload(70_000, 3)}, false)
	p, err = e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: single.dataURL()}, Settings: testSettings()})
	if err != nil || len(p.Resource.Files) != 1 || p.Resource.Files[0].Name != "one.bin" || p.Resource.Size != 70_000 {
		t.Errorf("single-file probe = %+v, %v", p, err)
	}
}

func TestResolveFromAnHTTPURL(t *testing.T) {
	sw := newSwarm(t, "web", content{"": payload(70_000, 4)}, false)
	var buf bytes.Buffer
	if err := sw.mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/x/web.torrent" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)
	e := newEngine(t, testConfig())
	resolve := func(path string) error {
		_, err := e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: srv.URL + path}, Settings: testSettings()})
		return err
	}
	if err := resolve("/x/web.torrent"); err != nil {
		t.Fatal(err)
	}
	wantKind(t, resolve("/x/gone.torrent"), engine.KindLinkExpired)
}

func TestResolveRejectsBadSources(t *testing.T) {
	e := newEngine(t, testConfig())
	for name, u := range map[string]string{
		"bad magnet":       "magnet:?dn=nothing",
		"not base64":       "data:application/x-bittorrent;base64,@@@@",
		"no base64 marker": "data:application/x-bittorrent,abc",
		"not a torrent":    "data:application/x-bittorrent;base64," + base64.StdEncoding.EncodeToString([]byte("hello")),
		"empty torrent":    "data:application/x-bittorrent;base64,ZGU=", // "de"
		"v2-only magnet":   "magnet:?xt=urn:btmh:1220" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		_, err := e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: u}, Settings: testSettings()})
		wantKind(t, err, engine.KindFatal)
		if name != "v2-only magnet" && !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
}

func TestResolveMagnetFetchesMetadata(t *testing.T) {
	sw := newSwarm(t, "from-magnet", multi, false)
	e := newEngine(t, testConfig())
	p, err := e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: sw.magnet()}, Settings: testSettings()})
	if err != nil {
		t.Fatal(err)
	}
	if p.Resource.Name != "from-magnet" || len(p.Resource.Files) != 3 {
		t.Errorf("probe = %+v", p.Resource)
	}
	// Nothing is left in the client after a resolve.
	e.mu.Lock()
	n := len(e.active)
	e.mu.Unlock()
	if n != 0 {
		t.Errorf("%d torrents left in the client after the resolve", n)
	}
}

func TestResolveMagnetWithoutPeersTimesOut(t *testing.T) {
	cfg := testConfig()
	cfg.MetadataTimeout = 300 * time.Millisecond
	e := newEngine(t, cfg)
	start := time.Now()
	_, err := e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}, Settings: testSettings()})
	wantKind(t, err, engine.KindTransient)
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	// And a cancelled resolve returns the cancellation.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cfg.MetadataTimeout = time.Minute
	_, err = newEngine(t, cfg).Resolve(ctx, engine.Request{Source: domain.Source{CurrentURL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}, Settings: testSettings()})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestDownloadSingleFile(t *testing.T) {
	data := payload(500_000, 5)
	sw := newSwarm(t, "single.bin", content{"": data}, false)
	e := newEngine(t, testConfig())
	dir := tempDir(t)
	rec := &recorder{}
	dl, err := runWithin(t, e, spec(dir, sw.dataURL(), "single.bin", testSettings(), rec), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "single.bin")); !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	if _, err := os.Stat(filepath.Join(dir, "single.bin.part")); !os.IsNotExist(err) {
		t.Errorf(".part left behind: %v", err)
	}
	if p := dl.Progress(); p.Downloaded != int64(len(data)) || p.Size != int64(len(data)) {
		t.Errorf("progress = %+v", p)
	}
	cp, state := rec.last()
	if cp.Downloaded != int64(len(data)) || cp.Size != int64(len(data)) {
		t.Errorf("checkpoint = %+v", cp)
	}
	if st, ok := decodeState(state); !ok || !st.Complete || len(st.Metainfo) == 0 {
		t.Errorf("engine state = %+v (decoded %v)", st, ok)
	}
}

func TestDownloadUnderAnotherName(t *testing.T) {
	data := payload(100_000, 6)
	sw := newSwarm(t, "torrent-name.bin", content{"": data}, false)
	dir := tempDir(t)
	if _, err := runWithin(t, newEngine(t, testConfig()), spec(dir, sw.dataURL(), "my name.bin", testSettings(), nil), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, filepath.Join(dir, "my name.bin")), data) {
		t.Fatal("content differs")
	}
	if _, err := os.Stat(filepath.Join(dir, "torrent-name.bin")); !os.IsNotExist(err) {
		t.Error("the file was written under the torrent's own name")
	}
}

func TestDownloadMultiFileFromAMagnetWithASelection(t *testing.T) {
	sw := newSwarm(t, "pack", multi, false)
	e := newEngine(t, testConfig())
	p, err := e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: sw.magnet()}, Settings: testSettings()})
	if err != nil {
		t.Fatal(err)
	}
	res := p.Resource
	for i := range res.Files {
		res.Files[i].Selected = res.Files[i].Name != "a.bin" // skip the biggest
	}
	dir := tempDir(t)
	sp := spec(dir, sw.magnet(), "pack", testSettings(), nil)
	sp.Resource = &res
	sp.Probe = p
	dl, err := runWithin(t, e, sp, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "pack", "sub", "b.bin")); !bytes.Equal(got, multi["sub/b.bin"]) {
		t.Error("sub/b.bin differs")
	}
	if got := readFile(t, filepath.Join(dir, "pack", "sub", "deep", "c.txt")); string(got) != "hello torrent" {
		t.Error("sub/deep/c.txt differs")
	}
	if _, err := os.Stat(filepath.Join(dir, "pack", "a.bin")); !os.IsNotExist(err) {
		t.Errorf("the unselected file exists: %v", err)
	}
	// (A piece shared with a selected neighbour may leave a .part stub of the
	// unselected file; it is never promoted to the file itself.)
	want := int64(150_000 + len("hello torrent"))
	if pr := dl.Progress(); pr.Size != want || pr.Downloaded != want {
		t.Errorf("progress = %+v, want %d", pr, want)
	}
}

func TestNoFileSelected(t *testing.T) {
	sw := newSwarm(t, "pack", multi, false)
	e := newEngine(t, testConfig())
	res := domain.Resource{Name: "pack", Files: []domain.File{{Index: 0}, {Index: 1}, {Index: 2}}}
	sp := spec(tempDir(t), sw.dataURL(), "pack", testSettings(), nil)
	sp.Resource = &res
	_, err := runWithin(t, e, sp, 10*time.Second)
	wantKind(t, err, engine.KindFatal)
}

func TestDestinationExists(t *testing.T) {
	sw := newSwarm(t, "single.bin", content{"": payload(70_000, 7)}, false)
	e := newEngine(t, testConfig())
	dir := tempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "single.bin"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runWithin(t, e, spec(dir, sw.dataURL(), "single.bin", testSettings(), nil), 10*time.Second)
	if !errors.Is(err, ErrDestinationExists) {
		t.Errorf("err = %v, want ErrDestinationExists", err)
	}
	if string(readFile(t, filepath.Join(dir, "single.bin"))) != "mine" {
		t.Error("an existing file was overwritten")
	}
	// A multi-file torrent cannot go where a plain file is.
	msw := newSwarm(t, "pack", multi, false)
	if err := os.WriteFile(filepath.Join(dir, "pack"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = runWithin(t, e, spec(dir, msw.dataURL(), "pack", testSettings(), nil), 10*time.Second)
	if !errors.Is(err, ErrDestinationExists) {
		t.Errorf("multi: err = %v, want ErrDestinationExists", err)
	}
}

func TestDiskFullPrecheck(t *testing.T) {
	sw := newSwarm(t, "single.bin", content{"": payload(70_000, 8)}, false)
	e := newEngine(t, testConfig(), WithFreeSpace(func(string) (uint64, error) { return 100, nil }))
	_, err := runWithin(t, e, spec(tempDir(t), sw.dataURL(), "single.bin", testSettings(), nil), 10*time.Second)
	wantKind(t, err, engine.KindFatal)
	var ee *engine.Error
	if !errors.As(err, &ee) || ee.Code != domain.ErrorCodeDiskFull {
		t.Errorf("err = %v, want disk_full", err)
	}
}

func TestOddFileNamesAreMadeValid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test seeder cannot create files with these names on Windows")
	}
	dir := tempDir(t)
	odd := newSwarm(t, "pack", content{"ok.txt": []byte("fine"), "we:ird*.txt": []byte("odd")}, false)
	if _, err := runWithin(t, newEngine(t, testConfig()), spec(dir, odd.dataURL(), "pack", testSettings(), nil), 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, filepath.Join(dir, "pack", "ok.txt"))) != "fine" || string(readFile(t, filepath.Join(dir, "pack", "we_ird_.txt"))) != "odd" {
		t.Error("files are not where expected")
	}
}

func TestSafeComponent(t *testing.T) {
	for in, want := range map[string]string{"plain.txt": "plain.txt", "we:ird*.txt": "we_ird_.txt", "a/b": "b", "..": "download", ".": "download", "": "download"} {
		if got := safeComponent(in); got != want {
			t.Errorf("safeComponent(%q) = %q, want %q", in, got, want)
		}
	}
	if got := safeComponent("CON" + " "); got == "" || got == "CON"+" " {
		t.Errorf("safeComponent(CON ) = %q", got)
	}
}

func TestSecondDownloadOfTheSameTorrentIsRefused(t *testing.T) {
	sw := newSwarm(t, "single.bin", content{"": payload(2_000_000, 9)}, false)
	e := newEngine(t, testConfig())
	// A limit keeps the first download busy.
	slow := ratelimit.NewBucket(100_000)
	sp := spec(tempDir(t), sw.dataURL(), "single.bin", testSettings(), nil)
	sp.Limiter = slow
	first, err := e.NewDownload(sp)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- first.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for first.Progress().Size <= 0 || e.activeCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first download did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, err = runWithin(t, e, spec(tempDir(t), sw.dataURL(), "other.bin", testSettings(), nil), 10*time.Second)
	wantKind(t, err, engine.KindFatal)
	cancel()
	if err := <-done; err != nil {
		t.Logf("the cancelled run ended with: %v", err)
	}
}

func (e *Engine) activeCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.active)
}

func TestExtraTrackersExceptForPrivateTorrents(t *testing.T) {
	for name, private := range map[string]bool{"public": false, "private": true} {
		t.Run(name, func(t *testing.T) {
			sw := newSwarm(t, "single.bin", content{"": payload(70_000, 10)}, private)
			var hits atomic.Int32
			extra := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
			t.Cleanup(extra.Close)
			s := testSettings(withSeeding(0, 1500*time.Millisecond, false), func(s *settings.Settings) {
				s.Torrent.Trackers = settings.Some([]string{extra.URL + "/announce"})
			})
			if _, err := runWithin(t, newEngine(t, testConfig()), spec(tempDir(t), sw.dataURL(), "single.bin", s, nil), 30*time.Second); err != nil {
				t.Fatal(err)
			}
			if got := hits.Load() > 0; got == private {
				t.Errorf("the extra tracker was asked: %v, for a %s torrent", got, name)
			}
		})
	}
}

func TestCloseStopsDownloads(t *testing.T) {
	sw := newSwarm(t, "single.bin", content{"": payload(2_000_000, 11)}, false)
	e := newEngine(t, testConfig())
	sp := spec(tempDir(t), sw.dataURL(), "single.bin", testSettings(), nil)
	sp.Limiter = ratelimit.NewBucket(100_000)
	dl, err := e.NewDownload(sp)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- dl.Run(t.Context()) }()
	deadline := time.Now().Add(10 * time.Second)
	for e.activeCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the download did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := e.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run succeeded although the engine was closed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after Close")
	}
	// A closed engine refuses new work.
	_, err = e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: sw.magnet()}, Settings: testSettings()})
	wantKind(t, err, engine.KindFatal)
}

func TestBadSpecs(t *testing.T) {
	e := newEngine(t, testConfig())
	s := spec(tempDir(t), "https://x/a.mp4", "x", testSettings(), nil)
	if _, err := e.NewDownload(s); err == nil {
		t.Error("want an error for a non-torrent URL")
	}
	s = spec(tempDir(t), "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "x", testSettings(), nil)
	s.Path = "relative"
	if _, err := e.NewDownload(s); err == nil {
		t.Error("want an error for a relative path")
	}
	sw := newSwarm(t, "single.bin", content{"": payload(10_000, 12)}, false)
	dl, err := e.NewDownload(spec(tempDir(t), sw.dataURL(), "single.bin", testSettings(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := dl.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := dl.Run(t.Context()); err == nil {
		t.Error("a second Run must fail")
	}
}

func TestMetainfoRoundTripsThroughTheEngineState(t *testing.T) {
	sw := newSwarm(t, "single.bin", content{"": payload(10_000, 13)}, false)
	var buf bytes.Buffer
	if err := sw.mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	b, _ := jsonOf(state{V: stateVersion, Metainfo: buf.Bytes(), Complete: true, Uploaded: 5, SeedMillis: 6})
	st, ok := decodeState(b)
	if !ok || st.Uploaded != 5 || st.SeedMillis != 6 || !st.Complete {
		t.Fatalf("state = %+v, %v", st, ok)
	}
	if mi, err := metainfo.Load(bytes.NewReader(st.Metainfo)); err != nil || mi.HashInfoBytes() != sw.mi.HashInfoBytes() {
		t.Errorf("metainfo did not survive: %v", err)
	}
	for _, bad := range [][]byte{nil, []byte("{"), []byte(`{"v":99}`)} {
		if _, ok := decodeState(bad); ok {
			t.Errorf("decodeState(%q) accepted", bad)
		}
	}
}

func TestPromoteAndCopyFile(t *testing.T) {
	dir := tempDir(t)
	final := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(final+".part", []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := promote(final); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, final)) != "data" {
		t.Error("the part file was not promoted")
	}
	if _, err := os.Stat(final + ".part"); !os.IsNotExist(err) {
		t.Errorf(".part left: %v", err)
	}
	if err := promote(final); err != nil { // already there: nothing to do
		t.Fatal(err)
	}
	if err := promote(filepath.Join(dir, "missing.bin")); err == nil {
		t.Error("promoting a file that does not exist must fail")
	}
	// The fallback copies, and never overwrites.
	if err := os.WriteFile(filepath.Join(dir, "src"), []byte("copy me"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := copyFile(filepath.Join(dir, "src"), dst); err != nil || string(readFile(t, dst)) != "copy me" {
		t.Fatalf("copyFile: %v", err)
	}
	if err := copyFile(filepath.Join(dir, "src"), dst); err == nil {
		t.Error("copyFile overwrote an existing file")
	}
	if string(readFile(t, dst)) != "copy me" {
		t.Error("the existing file was damaged")
	}
}
