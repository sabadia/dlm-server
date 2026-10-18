package ed2k

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/monkeyWie/goed2k"
	"github.com/monkeyWie/goed2k/protocol"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/settings"
)

// linkOf builds the link of data under name, optionally naming a source.
func linkOf(name string, data []byte, hash protocol.Hash, sources ...string) string {
	l := "ed2k://|file|" + strings.ReplaceAll(name, " ", "%20") + "|" + strconv.Itoa(len(data)) + "|" + strings.ToUpper(hash.String()) + "|/"
	if len(sources) > 0 {
		l += "|sources," + strings.Join(sources, ",") + "|/"
	}
	return l
}

// seeder is an in-process ed2k client sharing a complete file.
type seeder struct {
	cl   *goed2k.Client
	port int
	hash protocol.Hash
	data []byte
}

// newSeeder shares data (at most one piece long). The upload queue of the
// protocol hands out one block per slot turn, so test files are small.
func newSeeder(t *testing.T, data []byte) *seeder {
	t.Helper()
	skipUnderRace(t)
	hash, err := protocol.HashFromData(data)
	if err != nil {
		t.Fatal(err)
	}
	dir := tempDir(t)
	path := filepath.Join(dir, "seed.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	s := goed2k.NewSettings()
	s.ListenPort = 0
	s.UDPPort = 0
	s.UploadSlots = 10
	cl := goed2k.NewClient(s)
	if err := cl.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	pieces := protocol.NewBitField(1)
	pieces.SetBit(0)
	atp := goed2k.AddTransferParams{Hash: hash, CreateTime: time.Now().UnixMilli(), Size: int64(len(data)), FilePath: path, ResumeData: &protocol.TransferResumeData{Pieces: pieces}}
	h, err := cl.AddTransfer(atp)
	if err != nil || !h.IsFinished() {
		t.Fatalf("seed: %v finished=%v", err, h.IsFinished())
	}
	return &seeder{cl: cl, port: cl.Session().GetListenPort(), hash: hash, data: data}
}

func (s *seeder) link(name string) string {
	return linkOf(name, s.data, s.hash, "127.0.0.1:"+strconv.Itoa(s.port))
}

func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ed2k-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for range 50 {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Logf("could not remove %s", dir)
	})
	return dir
}

func randomData(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func newEngine(t *testing.T, opts ...Option) *Engine {
	t.Helper()
	all := append([]Option{WithPollInterval(50 * time.Millisecond), WithCheckpointInterval(50 * time.Millisecond)}, opts...)
	e := New(engine.Env{}, Config{}, all...)
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func spec(dir, src, name string, cp engine.Checkpointer) engine.Spec {
	s := settings.Defaults("")
	return engine.Spec{TaskID: "t1", Source: domain.Source{CurrentURL: src}, Path: filepath.Join(dir, name), Settings: s, Checkpointer: cp}
}

type recorder struct {
	cp engine.Checkpoint
	st []byte
}

func (r *recorder) Checkpoint(_ context.Context, cp engine.Checkpoint) error {
	r.cp = cp
	if cp.EngineState != nil {
		r.st = cp.EngineState
	}
	return nil
}

func wantKind(t *testing.T, err error, kind engine.Kind) {
	t.Helper()
	if err == nil || engine.Classify(err) != kind {
		t.Fatalf("err = %v (kind %v), want kind %v", err, engine.Classify(err), kind)
	}
}

func TestMatchAndParse(t *testing.T) {
	e := New(engine.Env{}, Config{})
	good := "ed2k://|file|My%20File.iso|1234567|0123456789ABCDEF0123456789ABCDEF|/"
	for u, want := range map[string]bool{
		good: true, strings.ToUpper(good): true, "ed2k://|server|1.2.3.4|4661|/": false, "http://x/a.iso": false, "": false,
	} {
		if got := e.Match(domain.Source{CurrentURL: u}); got != want {
			t.Errorf("Match(%q) = %v, want %v", u, got, want)
		}
	}
	l, err := parseLink(good + "|sources,1.2.3.4:4662,bad,5.6.7.8:99999,[::1]:4662,9.9.9.9:4662|/")
	if err != nil {
		t.Fatal(err)
	}
	if l.name != "My File.iso" || l.size != 1234567 || strings.ToUpper(l.hash.String()) != "0123456789ABCDEF0123456789ABCDEF" {
		t.Errorf("link = %+v", l)
	}
	if len(l.sources) != 2 || l.sources[0].String() != "1.2.3.4:4662" || l.sources[1].String() != "9.9.9.9:4662" {
		t.Errorf("sources = %v", l.sources)
	}
	if l, err := parseLink("ed2k://|file|a|5|0123456789abcdef0123456789abcdef|h=XYZ|/"); err != nil || l.size != 5 {
		t.Errorf("lower-case hash and options: %v", err)
	}
	for name, bad := range map[string]string{
		"no end":        "ed2k://|file|a|5|0123456789ABCDEF0123456789ABCDEF|",
		"short hash":    "ed2k://|file|a|5|0123|/",
		"bad hash":      "ed2k://|file|a|5|ZZZZ456789ABCDEF0123456789ABCDEF|/",
		"zero size":     "ed2k://|file|a|0|0123456789ABCDEF0123456789ABCDEF|/",
		"bad size":      "ed2k://|file|a|big|0123456789ABCDEF0123456789ABCDEF|/",
		"bad escape":    "ed2k://|file|a%zz|5|0123456789ABCDEF0123456789ABCDEF|/",
		"too few parts": "ed2k://|file|a|5|",
	} {
		if _, err := parseLink(bad); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
}

func TestResolveNeedsNoNetwork(t *testing.T) {
	e := newEngine(t)
	p, err := e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: "ed2k://|file|A%20b%3Ac.iso|42|0123456789ABCDEF0123456789ABCDEF|/"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Engine != EngineName || p.Suggested != "A b_c.iso" || p.Resource.Size != 42 || len(p.Resource.Files) != 1 {
		t.Errorf("probe = %+v", p)
	}
	if fp := p.Source.Fingerprint; fp.Size != 42 || fp.Filename != "A b_c.iso" {
		t.Errorf("fingerprint = %+v", fp)
	}
	e.mu.Lock()
	started := e.client != nil
	e.mu.Unlock()
	if started {
		t.Error("Resolve started the client")
	}
	_, err = e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: "ed2k://|file|broken|/"}})
	wantKind(t, err, engine.KindFatal)
}

func TestDownloadFromASourceInTheLink(t *testing.T) {
	data := randomData(150_000)
	sd := newSeeder(t, data)
	e := newEngine(t)
	dir := tempDir(t)
	rec := &recorder{}
	dl, err := e.NewDownload(spec(dir, sd.link("got it.bin"), "got it.bin", rec))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	if err := dl.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "got it.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("content differs (%d bytes, %v)", len(got), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "got it.bin"+PartSuffix)); !os.IsNotExist(err) {
		t.Errorf(".part left behind: %v", err)
	}
	if p := dl.Progress(); p.Downloaded != int64(len(data)) || p.Size != int64(len(data)) {
		t.Errorf("progress = %+v", p)
	}
	if rec.cp.Size != int64(len(data)) || rec.cp.Downloaded != int64(len(data)) {
		t.Errorf("checkpoint = %+v", rec.cp)
	}
}

func TestPauseKeepsThePartAndResumeData(t *testing.T) {
	data := randomData(150_000)
	sd := newSeeder(t, data)
	e := newEngine(t)
	dir := tempDir(t)
	rec := &recorder{}
	sp := spec(dir, sd.link("p.bin"), "p.bin", rec)
	dl, err := e.NewDownload(sp)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // paused before anything arrives
	if err := dl.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "p.bin"+PartSuffix)); err != nil {
		t.Errorf(".part missing after the pause: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "p.bin")); !os.IsNotExist(err) {
		t.Error("the final file exists after a pause")
	}
	st, ok := decodeState(rec.st)
	if !ok || st.Resume == nil {
		t.Fatalf("no resume data was saved: %q", rec.st)
	}
	sp.EngineState = rec.st
	dl2, err := e.NewDownload(sp)
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel2()
	if err := dl2.Run(ctx2); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "p.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("content differs after resuming (%v)", err)
	}
}

func TestDestinationExistsAndBadSpecs(t *testing.T) {
	dir := tempDir(t)
	link := "ed2k://|file|x.bin|5|0123456789ABCDEF0123456789ABCDEF|/"
	if err := os.WriteFile(filepath.Join(dir, "x.bin"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newEngine(t)
	dl, err := e.NewDownload(spec(dir, link, "x.bin", nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := dl.Run(t.Context()); !errors.Is(err, ErrDestinationExists) {
		t.Errorf("err = %v, want ErrDestinationExists", err)
	}
	if string(mustRead(t, filepath.Join(dir, "x.bin"))) != "mine" {
		t.Error("an existing file was overwritten")
	}
	rel := spec(dir, link, "x", nil)
	rel.Path = "relative"
	for name, sp := range map[string]engine.Spec{
		"relative path": rel,
		"http url":      spec(dir, "http://x/a", "x", nil),
		"bad link":      spec(dir, "ed2k://|file|x|/", "x", nil),
	} {
		if _, err := e.NewDownload(sp); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDiskFullPrecheckAndDuplicates(t *testing.T) {
	dir := tempDir(t)
	link := "ed2k://|file|x.bin|1000000|0123456789ABCDEF0123456789ABCDEF|/"
	e := newEngine(t, WithFreeSpace(func(string) (uint64, error) { return 10, nil }))
	dl, err := e.NewDownload(spec(dir, link, "x.bin", nil))
	if err != nil {
		t.Fatal(err)
	}
	err = dl.Run(t.Context())
	wantKind(t, err, engine.KindFatal)
	var ee *engine.Error
	if !errors.As(err, &ee) || ee.Code != domain.ErrorCodeDiskFull {
		t.Errorf("err = %v, want disk_full", err)
	}
	if !e.claim("h") || e.claim("h") {
		t.Error("claim must succeed once and then refuse")
	}
	e.unclaim("h")
	if !e.claim("h") {
		t.Error("a released hash can be claimed again")
	}
}

// skipUnderRace skips tests that run a transfer: goed2k has data races of its
// own (its tick goroutine changes transfers that status calls read
// unsynchronised), which the race detector reports. They run without -race.
func skipUnderRace(t *testing.T) {
	t.Helper()
	if raceDetector {
		t.Skip("goed2k is not race-free; this test runs without -race")
	}
}

func TestCloseStopsDownloads(t *testing.T) {
	skipUnderRace(t)
	link := "ed2k://|file|x.bin|1000000|0123456789ABCDEF0123456789ABCDEF|/" // nobody has it
	e := newEngine(t)
	dl, err := e.NewDownload(spec(tempDir(t), link, "x.bin", nil))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	go func() { done <- dl.Run(ctx) }()
	time.Sleep(500 * time.Millisecond)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	cancel() // the library gives no signal when it is closed; the run ends with its context
	if err := <-done; err == nil {
		t.Error("Run succeeded although nobody has the file")
	}
	_, err = e.NewDownload(spec(tempDir(t), link, "y.bin", nil))
	if err != nil {
		t.Fatal(err)
	}
	dl2, _ := e.NewDownload(spec(tempDir(t), link, "y.bin", nil))
	wantKind(t, dl2.Run(t.Context()), engine.KindFatal) // a closed engine refuses new work
}

func TestStateRoundTrip(t *testing.T) {
	rd := &protocol.TransferResumeData{Pieces: protocol.NewBitField(2)}
	rd.Pieces.SetBit(1)
	ep, _ := endpointOf("1.2.3.4:4662")
	rd.Peers = append(rd.Peers, ep)
	blob, err := json.Marshal(state{V: stateVersion, Resume: rd})
	if err != nil {
		t.Fatal(err)
	}
	st, ok := decodeState(blob)
	if !ok || st.Resume == nil || !st.Resume.Pieces.GetBit(1) || st.Resume.Pieces.GetBit(0) || len(st.Resume.Peers) != 1 || st.Resume.Peers[0].String() != "1.2.3.4:4662" {
		t.Fatalf("state = %+v (decoded %v)", st, ok)
	}
	for _, bad := range [][]byte{nil, []byte("{"), []byte(`{"v":9}`)} {
		if _, ok := decodeState(bad); ok {
			t.Errorf("decodeState(%q) accepted", bad)
		}
	}
	// The links' sources join the saved peers without duplicates.
	l, _ := parseLink("ed2k://|file|a|5|0123456789ABCDEF0123456789ABCDEF|/|sources,1.2.3.4:4662,5.6.7.8:4662|/")
	d := &Download{l: l}
	merged := d.resumeData(st.Resume)
	if len(merged.Peers) != 2 {
		t.Errorf("merged peers = %v", merged.Peers)
	}
	if got := (&Download{l: &link{}}).resumeData(nil); got != nil {
		t.Errorf("nothing to resume: %+v", got)
	}
}
