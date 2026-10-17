package ftp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/settings"
)

var epoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func newEngine(t *testing.T, opts ...Option) *Engine {
	t.Helper()
	noSleep := func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	all := append([]Option{WithRetrySleeper(noSleep), WithCheckpointInterval(time.Millisecond)}, opts...)
	return New(engine.Env{}, all...)
}

func testSettings(attempts int, mutate ...func(*settings.Settings)) settings.Settings {
	s := settings.Defaults("")
	s.Proxy = settings.Some(settings.Proxy{Mode: settings.ProxyNone})
	s.Retry.MaxAttempts = settings.Some(attempts)
	s.TLSVerify = settings.Some(false)
	for _, m := range mutate {
		m(&s)
	}
	return s
}

type recorder struct {
	mu sync.Mutex
	cp engine.Checkpoint
	n  int
}

func (r *recorder) Checkpoint(_ context.Context, cp engine.Checkpoint) error {
	r.mu.Lock()
	r.cp, r.n = cp, r.n+1
	r.mu.Unlock()
	return nil
}

func (r *recorder) last() engine.Checkpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cp
}

func spec(dir, rawURL, name string, s settings.Settings, cp engine.Checkpointer) engine.Spec {
	return engine.Spec{TaskID: "t1", Source: domain.Source{CurrentURL: rawURL}, Path: filepath.Join(dir, name), Settings: s, Checkpointer: cp}
}

// resumeSpec continues a download from the checkpoint of an earlier run.
func resumeSpec(sp engine.Spec, cp engine.Checkpoint) engine.Spec {
	sp.Segments, sp.EngineState = cp.Segments, cp.EngineState
	if cp.Source != nil {
		sp.Source = *cp.Source
	}
	return sp
}

func run(t *testing.T, e *Engine, sp engine.Spec) (engine.Download, error) {
	t.Helper()
	dl, err := e.NewDownload(sp)
	if err != nil {
		t.Fatalf("NewDownload: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return dl, dl.Run(ctx)
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

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hasCmd(cmds []string, prefix string) bool {
	for _, c := range cmds {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestParseTarget(t *testing.T) {
	for raw, want := range map[string]target{
		"ftp://host/a/b.bin":             {host: "host", addr: "host:21", user: "anonymous", pass: "anonymous@", path: "a/b.bin"},
		"ftp://u:p%40ss@host:2121/x.zip": {host: "host", addr: "host:2121", user: "u", pass: "p@ss", path: "x.zip"},
		"ftp://u@host/x":                 {host: "host", addr: "host:21", user: "u", pass: "", path: "x"},
		"ftps://host/x":                  {host: "host", addr: "host:990", user: "anonymous", pass: "anonymous@", path: "x", mode: tlsImplicit},
		"ftpes://host/x":                 {host: "host", addr: "host:21", user: "anonymous", pass: "anonymous@", path: "x", mode: tlsExplicit},
		"ftp://host//etc/motd":           {host: "host", addr: "host:21", user: "anonymous", pass: "anonymous@", path: "/etc/motd"},
		"ftp://host/%2Fetc/motd":         {host: "host", addr: "host:21", user: "anonymous", pass: "anonymous@", path: "/etc/motd"},
		"ftp://[::1]:2121/a b":           {host: "::1", addr: "[::1]:2121", user: "anonymous", pass: "anonymous@", path: "a b"},
		"FTP://Host/Mixed%20Case.TXT":    {host: "Host", addr: "Host:21", user: "anonymous", pass: "anonymous@", path: "Mixed Case.TXT"},
		"ftp://anonymous@host/x":         {host: "host", addr: "host:21", user: "anonymous", pass: "anonymous@", path: "x"},
		"ftp://host/%0d%0aDELE%20x":      {},
		"http://host/x":                  {},
		"ftp:///x":                       {},
		"ftp://host:notaport/x":          {},
		"ftp://%zz/x":                    {},
		"ftp://host/dir/":                {host: "host", addr: "host:21", user: "anonymous", pass: "anonymous@", path: "dir/"},
	} {
		got, err := parseTarget(raw)
		if want.host == "" {
			if err == nil || !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("parseTarget(%q) = %+v, %v; want ErrInvalidArgument", raw, got, err)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("parseTarget(%q) = %+v, %v; want %+v", raw, got, err, want)
		}
	}
}

func TestMatchAndRedaction(t *testing.T) {
	e := newEngine(t)
	for u, want := range map[string]bool{
		"ftp://h/a": true, "FTPS://h/a": true, "ftpes://h/a": true, "http://h/a": false, "sftp://h/a": false, "ftp:///a": false, "": false,
	} {
		if got := e.Match(domain.Source{CurrentURL: u}); got != want {
			t.Errorf("Match(%q) = %v, want %v", u, got, want)
		}
	}
	tg, _ := parseTarget("ftp://user:secret@h:21/a/b")
	if r := tg.redacted(); strings.Contains(r, "secret") || strings.Contains(r, "user") || r != "ftp://h:21/a/b" {
		t.Errorf("redacted = %q", r)
	}
}

func TestResolve(t *testing.T) {
	srv := newServer(t, serverOpts{})
	data := payload(5000)
	srv.put("dir/My File.bin", data, epoch)
	e := newEngine(t)
	p, err := e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: "ftp://" + srv.addr() + "/dir/My%20File.bin"}, Settings: testSettings(0)})
	if err != nil {
		t.Fatal(err)
	}
	if p.Engine != EngineName || p.Suggested != "My File.bin" || p.Resource.Size != 5000 || !p.Resource.RangeSupported || len(p.Resource.Files) != 1 {
		t.Errorf("probe = %+v", p)
	}
	fp := p.Source.Fingerprint
	if fp.Size != 5000 || fp.LastModified == nil || !fp.LastModified.Equal(epoch) || fp.Filename != "My File.bin" {
		t.Errorf("fingerprint = %+v", fp)
	}
}

func TestResolveErrors(t *testing.T) {
	srv := newServer(t, serverOpts{user: "bob", pass: "pw"})
	srv.put("f", []byte("x"), epoch)
	e := newEngine(t)
	resolve := func(u string) error {
		_, err := e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: u}, Settings: testSettings(0)})
		return err
	}
	base := "ftp://" + srv.addr()
	wantKind(t, resolve("ftp://anonymous@"+srv.addr()+"/f"), engine.KindLinkExpired) // 530
	wantKind(t, resolve("ftp://bob:wrong@"+srv.addr()+"/f"), engine.KindLinkExpired) // 530
	wantKind(t, resolve("ftp://bob:pw@"+srv.addr()+"/missing"), engine.KindLinkExpired)
	wantKind(t, resolve("ftp://bob:pw@"+srv.addr()+"/dir/"), engine.KindFatal)
	wantKind(t, resolve(base+"/"), engine.KindFatal)
	wantKind(t, resolve("http://x/f"), engine.KindFatal)
	if err := resolve("ftp://bob:pw@" + srv.addr() + "/f"); err != nil {
		t.Errorf("valid credentials: %v", err)
	}
	var ee *engine.Error
	if err := resolve("ftp://bob:wrong@" + srv.addr() + "/f"); !errors.As(err, &ee) || ee.StatusCode != 530 || strings.Contains(err.Error(), "wrong") {
		t.Errorf("login error = %v (must carry 530 and never the password)", err)
	}
	// Nothing listens: a refused connection is transient.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	_ = ln.Close()
	wantKind(t, resolve("ftp://"+dead+"/f"), engine.KindTransient)
}

func TestDownload(t *testing.T) {
	for _, size := range []int{0, 1, 4096, 300_000} {
		srv := newServer(t, serverOpts{})
		data := payload(size)
		srv.put("f.bin", data, epoch)
		dir := t.TempDir()
		rec := &recorder{}
		dl, err := run(t, newEngine(t), spec(dir, "ftp://"+srv.addr()+"/f.bin", "out.bin", testSettings(0), rec))
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if got := readFile(t, filepath.Join(dir, "out.bin")); !bytes.Equal(got, data) {
			t.Fatalf("size %d: content differs (%d bytes)", size, len(got))
		}
		if _, err := os.Stat(filepath.Join(dir, "out.bin"+PartSuffix)); !os.IsNotExist(err) {
			t.Errorf("size %d: .part left behind: %v", size, err)
		}
		if p := dl.Progress(); p.Downloaded != int64(size) || p.Size != int64(size) || p.Connections != 0 {
			t.Errorf("size %d: progress = %+v", size, p)
		}
		if size > 0 && rec.last().Source == nil {
			t.Errorf("size %d: the checkpoint did not carry the fingerprint", size)
		}
	}
}

func TestDownloadUsesCredentialsAndThePath(t *testing.T) {
	srv := newServer(t, serverOpts{user: "bob", pass: "p@ss"})
	srv.put("/abs/file", []byte("hello"), epoch)
	dir := t.TempDir()
	if _, err := run(t, newEngine(t), spec(dir, "ftp://bob:p%40ss@"+srv.addr()+"//abs/file", "o", testSettings(0), nil)); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, filepath.Join(dir, "o"))) != "hello" {
		t.Error("wrong content")
	}
	if !hasCmd(srv.commands(), "RETR /abs/file") || !hasCmd(srv.commands(), "TYPE I") {
		t.Errorf("commands = %v", srv.commands())
	}
}

func TestRetryResumesWithREST(t *testing.T) {
	srv := newServer(t, serverOpts{dropAfter: 120_000})
	data := payload(400_000)
	srv.put("f", data, epoch)
	dir := t.TempDir()
	_, err := run(t, newEngine(t), spec(dir, "ftp://"+srv.addr()+"/f", "o", testSettings(3), nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "o")); !bytes.Equal(got, data) {
		t.Fatal("content differs after the resumed transfer")
	}
	// The second RETR continued at an offset: bytes the client had received
	// are not asked for again. (What the server wrote before the reset can
	// exceed that on some platforms, so only the offset is checked.)
	var offset int
	for _, c := range srv.commands() {
		if n, ok := strings.CutPrefix(c, "REST "); ok {
			offset, _ = strconv.Atoi(n) //nolint:errcheck // the server logs what the client sent
		}
	}
	// (After a reset Windows may deliver none of the bytes already sent, so
	// the second RETR may start at 0 there.)
	if offset < 0 || offset > 120_000 || (offset == 0 && runtime.GOOS != "windows") {
		t.Errorf("REST offset = %d, want within (0, 120000]; commands = %v", offset, srv.commands())
	}
}

func TestResumeAcrossRuns(t *testing.T) {
	srv := newServer(t, serverOpts{dropAfter: 150_000})
	data := payload(500_000)
	srv.put("f", data, epoch)
	dir := t.TempDir()
	u := "ftp://" + srv.addr() + "/f"
	rec := &recorder{}
	e := newEngine(t)
	sp := spec(dir, u, "o", testSettings(0), rec)
	_, err := run(t, e, sp)
	wantKind(t, err, engine.KindTransient)
	cp := rec.last()
	if cp.Downloaded <= 0 || cp.Downloaded >= int64(len(data)) || len(cp.Segments) != 1 || cp.Segments[0].Done != cp.Downloaded {
		t.Fatalf("checkpoint = %+v", cp)
	}
	if _, err := os.Stat(filepath.Join(dir, "o")); !os.IsNotExist(err) {
		t.Fatal("the final file exists before the download finished")
	}

	srv.resetCounters()
	dl, err := run(t, e, resumeSpec(sp, cp))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "o")); !bytes.Equal(got, data) {
		t.Fatal("content differs after resuming")
	}
	if sent := srv.bytesSent(); sent != len(data)-int(cp.Downloaded) {
		t.Errorf("second run fetched %d bytes, want %d", sent, len(data)-int(cp.Downloaded))
	}
	if p := dl.Progress(); p.Downloaded != int64(len(data)) {
		t.Errorf("progress = %+v", p)
	}
}

func TestChangedFile(t *testing.T) {
	for name, policy := range map[string]settings.MismatchPolicy{"fail": settings.MismatchFail, "restart": settings.MismatchRestart} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t, serverOpts{dropAfter: 100_000})
			old := payload(300_000)
			srv.put("f", old, epoch)
			dir := t.TempDir()
			rec := &recorder{}
			e := newEngine(t)
			sp := spec(dir, "ftp://"+srv.addr()+"/f", "o", testSettings(0, func(s *settings.Settings) {
				s.Refresh.OnMismatch = settings.Some(policy)
			}), rec)
			_, err := run(t, e, sp)
			wantKind(t, err, engine.KindTransient)
			cp := rec.last()

			fresh := append(append([]byte(nil), payload(300_000)[1:]...), 'x') // same size, other bytes
			srv.put("f", fresh, epoch.Add(time.Hour))
			srv.resetCounters()
			_, err = run(t, e, resumeSpec(sp, cp))
			if policy == settings.MismatchFail {
				wantKind(t, err, engine.KindLinkExpired)
				if !errors.Is(err, ErrContentChanged) {
					t.Errorf("err = %v, want ErrContentChanged", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(dir, "o")); !bytes.Equal(got, fresh) {
				t.Error("restart did not download the new file")
			}
			if srv.bytesSent() != len(fresh) {
				t.Errorf("restart fetched %d bytes, want the whole %d", srv.bytesSent(), len(fresh))
			}
		})
	}
}

func TestServerWithoutREST(t *testing.T) {
	srv := newServer(t, serverOpts{dropAfter: 100_000})
	data := payload(300_000)
	srv.put("f", data, epoch)
	dir := t.TempDir()
	rec := &recorder{}
	e := newEngine(t)
	sp := spec(dir, "ftp://"+srv.addr()+"/f", "o", testSettings(0), rec)
	_, err := run(t, e, sp)
	wantKind(t, err, engine.KindTransient)
	cp := rec.last()
	srv.set(func(o *serverOpts) { o.noREST = true })
	srv.resetCounters()
	if _, err := run(t, e, resumeSpec(sp, cp)); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "o")); !bytes.Equal(got, data) {
		t.Fatal("content differs after restarting without REST")
	}
	if srv.bytesSent() != len(data) {
		t.Errorf("fetched %d bytes, want a full %d", srv.bytesSent(), len(data))
	}
}

func TestServerWithoutSizeOrMDTM(t *testing.T) {
	srv := newServer(t, serverOpts{noSize: true, noMDTM: true})
	data := payload(200_000)
	srv.put("f", data, epoch)
	dir := t.TempDir()
	dl, err := run(t, newEngine(t), spec(dir, "ftp://"+srv.addr()+"/f", "o", testSettings(0), nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "o")); !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	if p := dl.Progress(); p.Size != int64(len(data)) || p.Downloaded != int64(len(data)) {
		t.Errorf("progress = %+v", p)
	}
	// Without a size an abrupt end cannot be told from a complete file by
	// length; the missing 226 gives it away.
	srv.set(func(o *serverOpts) { o.dropAfter = 50_000 })
	dir2 := t.TempDir()
	_, err = run(t, newEngine(t), spec(dir2, "ftp://"+srv.addr()+"/f", "o", testSettings(0), nil))
	wantKind(t, err, engine.KindTransient)
	if _, err := os.Stat(filepath.Join(dir2, "o")); !os.IsNotExist(err) {
		t.Error("a truncated download was published")
	}
}

func TestTLS(t *testing.T) {
	for name, c := range map[string]struct {
		opts   serverOpts
		scheme string
	}{
		"implicit": {serverOpts{implicit: true}, "ftps"},
		"explicit": {serverOpts{explicit: true}, "ftpes"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t, c.opts)
			data := payload(150_000)
			srv.put("f", data, epoch)
			u := c.scheme + "://" + srv.addr() + "/f"
			dir := t.TempDir()
			if _, err := run(t, newEngine(t), spec(dir, u, "o", testSettings(0), nil)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readFile(t, filepath.Join(dir, "o")), data) {
				t.Fatal("content differs")
			}
			// With verification on, the self-signed certificate is refused.
			strict := testSettings(0, func(s *settings.Settings) { s.TLSVerify = settings.Some(true) })
			_, err := newEngine(t).Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: u}, Settings: strict})
			wantKind(t, err, engine.KindFatal)
		})
	}
}

func TestSilentServerTimesOut(t *testing.T) {
	srv := newServer(t, serverOpts{implicit: true})
	srv.put("f", []byte("x"), epoch)
	start := time.Now()
	_, err := newEngine(t, WithControlTimeout(200*time.Millisecond)).Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: "ftp://" + srv.addr() + "/f"}, Settings: testSettings(0)})
	wantKind(t, err, engine.KindTransient) // a server that never greets is a timeout
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v to give up on a silent server", time.Since(start))
	}
}

func TestRateLimiterIsCharged(t *testing.T) {
	srv := newServer(t, serverOpts{})
	data := payload(200_000)
	srv.put("f", data, epoch)
	dir := t.TempDir()
	var n atomic.Int64
	sp := spec(dir, "ftp://"+srv.addr()+"/f", "o", testSettings(0), nil)
	sp.Limiter = limiterFunc(func(_ context.Context, k int) error { n.Add(int64(k)); return nil })
	if _, err := run(t, newEngine(t), sp); err != nil {
		t.Fatal(err)
	}
	if n.Load() != int64(len(data)) {
		t.Errorf("limiter charged %d bytes, want %d", n.Load(), len(data))
	}
}

type limiterFunc func(ctx context.Context, n int) error

func (f limiterFunc) WaitN(ctx context.Context, n int) error { return f(ctx, n) }

func TestPauseKeepsThePartAndCheckpoint(t *testing.T) {
	srv := newServer(t, serverOpts{stall: true, stallAfter: 100_000})
	data := payload(400_000)
	srv.put("f", data, epoch)
	dir := t.TempDir()
	rec := &recorder{}
	e := newEngine(t)
	sp := spec(dir, "ftp://"+srv.addr()+"/f", "o", testSettings(0), rec)
	dl, err := e.NewDownload(sp)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- dl.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for dl.Progress().Downloaded < 100_000 {
		if time.Now().After(deadline) {
			t.Fatalf("no progress: %+v", dl.Progress())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if _, err := os.Stat(filepath.Join(dir, "o"+PartSuffix)); err != nil {
		t.Errorf(".part missing: %v", err)
	}
	if cp := rec.last(); cp.Downloaded != 100_000 || len(cp.Segments) != 1 {
		t.Errorf("final checkpoint = %+v", cp)
	}
	// And the paused download continues.
	srv.set(func(o *serverOpts) { o.stall = false })
	srv.resetCounters()
	if _, err := run(t, e, resumeSpec(sp, rec.last())); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, filepath.Join(dir, "o")), data) {
		t.Fatal("content differs after pause and resume")
	}
	if srv.bytesSent() != len(data)-100_000 {
		t.Errorf("resume fetched %d bytes, want %d", srv.bytesSent(), len(data)-100_000)
	}
}

func TestStalledTransferIsTransient(t *testing.T) {
	srv := newServer(t, serverOpts{stall: true, stallAfter: 1000})
	srv.put("f", payload(50_000), epoch)
	dir := t.TempDir()
	start := time.Now()
	_, err := run(t, newEngine(t, WithStallTimeout(150*time.Millisecond)), spec(dir, "ftp://"+srv.addr()+"/f", "o", testSettings(0), nil))
	wantKind(t, err, engine.KindTransient)
	if time.Since(start) > 5*time.Second {
		t.Errorf("stall took %v to detect", time.Since(start))
	}
}

func TestDestinationExistsAndBadSpecs(t *testing.T) {
	srv := newServer(t, serverOpts{})
	srv.put("f", []byte("x"), epoch)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "o"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, newEngine(t), spec(dir, "ftp://"+srv.addr()+"/f", "o", testSettings(0), nil))
	if !errors.Is(err, ErrDestinationExists) {
		t.Errorf("err = %v, want ErrDestinationExists", err)
	}
	if string(readFile(t, filepath.Join(dir, "o"))) != "mine" {
		t.Error("an existing file was overwritten")
	}
	e := newEngine(t)
	rel := spec(dir, "ftp://h/f", "o", testSettings(0), nil)
	rel.Path = "relative"
	for name, sp := range map[string]engine.Spec{
		"relative path": rel,
		"http url":      spec(dir, "http://h/f", "o", testSettings(0), nil),
		"directory":     spec(dir, "ftp://h/dir/", "o", testSettings(0), nil),
	} {
		if _, err := e.NewDownload(sp); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	dl, err := e.NewDownload(spec(dir, "ftp://"+srv.addr()+"/f", "second", testSettings(0), nil))
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

func TestMissingFileDuringDownloadIsLinkExpired(t *testing.T) {
	srv := newServer(t, serverOpts{})
	dir := t.TempDir()
	_, err := run(t, newEngine(t), spec(dir, "ftp://"+srv.addr()+"/gone", "o", testSettings(0), nil))
	wantKind(t, err, engine.KindLinkExpired)
	if _, err := os.Stat(filepath.Join(dir, "o"+PartSuffix)); !os.IsNotExist(err) {
		t.Error("a .part file was created for a missing file")
	}
}

func TestDiskFullPrecheck(t *testing.T) {
	srv := newServer(t, serverOpts{})
	srv.put("f", payload(10_000), epoch)
	dir := t.TempDir()
	e := newEngine(t, WithFreeSpace(func(string) (uint64, error) { return 100, nil }))
	_, err := run(t, e, spec(dir, "ftp://"+srv.addr()+"/f", "o", testSettings(0), nil))
	wantKind(t, err, engine.KindFatal)
	var ee *engine.Error
	if !errors.As(err, &ee) || ee.Code != domain.ErrorCodeDiskFull {
		t.Errorf("err = %v, want disk_full", err)
	}
}

// ---- proxies ----

func TestHTTPProxyIsRefusedNotBypassed(t *testing.T) {
	srv := newServer(t, serverOpts{})
	srv.put("f", []byte("x"), epoch)
	s := testSettings(0, func(s *settings.Settings) {
		s.Proxy = settings.Some(settings.Proxy{Mode: settings.ProxyCustom, URL: "http://127.0.0.1:1"})
	})
	_, err := newEngine(t).Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: "ftp://" + srv.addr() + "/f"}, Settings: s})
	wantKind(t, err, engine.KindFatal)
	if srv.commands() != nil {
		t.Error("the server was contacted directly despite the proxy setting")
	}
}

// socks5 is a minimal CONNECT-only SOCKS5 proxy that counts connections.
type socks5 struct {
	ln    net.Listener
	conns atomic.Int32
}

func newSocks5(t *testing.T) *socks5 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &socks5{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.handle(c)
		}
	}()
	return p
}

func (p *socks5) handle(c net.Conn) {
	defer c.Close() //nolint:errcheck // test proxy
	buf := make([]byte, 262)
	if _, err := io.ReadFull(c, buf[:2]); err != nil || buf[0] != 5 {
		return
	}
	if _, err := io.ReadFull(c, buf[:int(buf[1])]); err != nil {
		return
	}
	_, _ = c.Write([]byte{5, 0}) // no authentication
	if _, err := io.ReadFull(c, buf[:4]); err != nil || buf[1] != 1 {
		return
	}
	var host string
	switch buf[3] {
	case 1:
		if _, err := io.ReadFull(c, buf[:4]); err != nil {
			return
		}
		host = net.IP(buf[:4]).String()
	case 3:
		if _, err := io.ReadFull(c, buf[:1]); err != nil {
			return
		}
		n := int(buf[0])
		if _, err := io.ReadFull(c, buf[:n]); err != nil {
			return
		}
		host = string(buf[:n])
	default:
		return
	}
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return
	}
	port := int(buf[0])<<8 | int(buf[1])
	up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close() //nolint:errcheck // test proxy
	p.conns.Add(1)
	_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

func TestSocks5ProxyCarriesControlAndData(t *testing.T) {
	for name, opts := range map[string]serverOpts{"plain": {}, "implicit": {implicit: true}, "explicit": {explicit: true}} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t, opts)
			data := payload(120_000)
			srv.put("f", data, epoch)
			px := newSocks5(t)
			scheme := map[string]string{"plain": "ftp", "implicit": "ftps", "explicit": "ftpes"}[name]
			s := testSettings(0, func(s *settings.Settings) {
				s.Proxy = settings.Some(settings.Proxy{Mode: settings.ProxyCustom, URL: "socks5://" + px.ln.Addr().String()})
			})
			dir := t.TempDir()
			if _, err := run(t, newEngine(t), spec(dir, scheme+"://"+srv.addr()+"/f", "o", s, nil)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readFile(t, filepath.Join(dir, "o")), data) {
				t.Fatal("content differs")
			}
			// One control and at least one data connection went through it.
			if px.conns.Load() < 2 {
				t.Errorf("proxy carried %d connections, want >= 2", px.conns.Load())
			}
		})
	}
}
