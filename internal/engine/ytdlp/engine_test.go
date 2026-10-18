package ytdlp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/ffmpeg"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/ytdlp"
)

type fakeLocator struct{ err error }

func (f fakeLocator) Locate(context.Context) (ytdlp.Binary, error) {
	if f.err != nil {
		return ytdlp.Binary{}, f.err
	}
	exe, err := os.Executable()
	if err != nil {
		return ytdlp.Binary{}, err
	}
	return ytdlp.Binary{Path: exe, Version: "2026.08.19", Source: "configured"}, nil
}

type fakeFF struct{ err error }

func (f fakeFF) Locate(context.Context) (ffmpeg.Binary, error) {
	return ffmpeg.Binary{Path: "/fake/ffmpeg"}, f.err
}

// scenario sets up the fake yt-dlp and returns the file its invocations are
// logged to.
func scenario(t *testing.T, env map[string]string) string {
	t.Helper()
	t.Setenv("DLM_FAKE_YTDLP", "1")
	log := filepath.Join(t.TempDir(), "calls.jsonl")
	t.Setenv("FAKE_LOG", log)
	// A scenario starts clean: variables of an earlier one must not leak in.
	for _, k := range []string{"FAKE_MODE", "FAKE_ERROR", "FAKE_JSON", "FAKE_BODY", "FAKE_OUTSIDE"} {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	return log
}

type invocation struct {
	Args        []string `json:"args"`
	Cookies     string   `json:"cookies"`
	CookiesMode string   `json:"cookiesMode"`
}

func calls(t *testing.T, log string) []invocation {
	t.Helper()
	f, err := os.Open(log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []invocation
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var c invocation
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func (c invocation) has(a string) bool {
	for _, x := range c.Args {
		if x == a {
			return true
		}
	}
	return false
}

func (c invocation) opt(name string) (string, bool) {
	for _, x := range c.Args {
		if v, ok := strings.CutPrefix(x, "--"+name+"="); ok {
			return v, true
		}
	}
	return "", false
}

func newEngine(t *testing.T, loc Locator, ff FFmpeg, opts ...Option) *Engine {
	t.Helper()
	return New(engine.Env{}, loc, ff, append([]Option{WithCheckpointInterval(time.Millisecond)}, opts...)...)
}

func src(u string) domain.Source {
	return domain.Source{CurrentURL: u, ResolverKind: domain.ResolverYtdlp}
}

func testSettings(mutate ...func(*settings.Settings)) settings.Settings {
	s := settings.Defaults("")
	s.Proxy = settings.Some(settings.Proxy{Mode: settings.ProxySystem})
	s.Retry.MaxAttempts = settings.Some(0)
	for _, m := range mutate {
		m(&s)
	}
	return s
}

func wantFatal(t *testing.T, err error) {
	t.Helper()
	if err == nil || engine.Classify(err) != engine.KindFatal {
		t.Fatalf("err = %v (kind %v), want a fatal error", err, engine.Classify(err))
	}
}

type recorder struct{ cp engine.Checkpoint }

func (r *recorder) Checkpoint(_ context.Context, cp engine.Checkpoint) error { r.cp = cp; return nil }

func spec(dir, u, name string, cp engine.Checkpointer, s settings.Settings) engine.Spec {
	return engine.Spec{TaskID: "t1", Source: src(u), Path: filepath.Join(dir, name), Settings: s, Checkpointer: cp}
}

func TestMatch(t *testing.T) {
	e := New(engine.Env{}, nil, nil)
	for _, c := range []struct {
		src  domain.Source
		want bool
	}{
		{domain.Source{CurrentURL: "https://v.example/watch?v=1", ResolverKind: domain.ResolverYtdlp}, true},
		{domain.Source{CurrentURL: "http://v.example/x", ResolverKind: domain.ResolverYtdlp}, true},
		{domain.Source{CurrentURL: "https://v.example/watch?v=1"}, false},
		{domain.Source{CurrentURL: "https://v.example/a.mp4", ResolverKind: domain.ResolverBrowser}, false},
		{domain.Source{CurrentURL: "ftp://v.example/x", ResolverKind: domain.ResolverYtdlp}, false},
		{domain.Source{CurrentURL: "magnet:?xt=urn:btih:abc", ResolverKind: domain.ResolverYtdlp}, false},
		{domain.Source{ResolverKind: domain.ResolverYtdlp}, false},
	} {
		if got := e.Match(c.src); got != c.want {
			t.Errorf("Match(%+v) = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestResolve(t *testing.T) {
	log := scenario(t, nil)
	e := newEngine(t, fakeLocator{}, nil)
	s := testSettings()
	p, err := e.Resolve(t.Context(), engine.Request{Source: src("https://v.example/watch?v=abc"), Settings: s})
	if err != nil {
		t.Fatal(err)
	}
	if p.Engine != EngineName || p.Suggested != "My Video_ Part 1.mp4" || p.Resource.Size != 4000 || len(p.Resource.Files) != 1 {
		t.Errorf("probe = %+v", p)
	}
	if fp := p.Source.Fingerprint; fp.Size != 4000 || fp.Filename != "My Video_ Part 1.mp4" {
		t.Errorf("fingerprint = %+v", fp)
	}
	c := calls(t, log)[0]
	n := len(c.Args)
	if !c.has("--dump-single-json") || !c.has("--no-playlist") || !c.has("--ignore-config") || c.Args[n-2] != "--" || c.Args[n-1] != "https://v.example/watch?v=abc" {
		t.Errorf("args = %v", c.Args)
	}
}

func TestResolveInfoVariants(t *testing.T) {
	for name, c := range map[string]struct {
		json     string
		wantName string
		wantSize int64
	}{
		"single format size":  {`{"id":"i","title":"T","ext":"webm","filesize":123}`, "T.webm", 123},
		"approximate size":    {`{"id":"i","title":"T","ext":"mp4","filesize_approx":99.4}`, "T.mp4", 99},
		"unknown size":        {`{"id":"i","title":"T","ext":"mp4"}`, "T.mp4", -1},
		"merge with unknown":  {`{"title":"T","ext":"mkv","requested_formats":[{"filesize":5},{}]}`, "T.mkv", -1},
		"no title uses id":    {`{"id":"xyz","ext":"mp4"}`, "xyz.mp4", -1},
		"nothing but a video": {`{}`, "video", -1},
		"odd extension":       {`{"title":"T","ext":"a/b"}`, "T", -1},
		"upper case ext":      {`{"title":"T","ext":".MP4"}`, "T.mp4", -1},
	} {
		scenario(t, map[string]string{"FAKE_JSON": c.json})
		p, err := newEngine(t, fakeLocator{}, nil).Resolve(t.Context(), engine.Request{Source: src("https://v.example/x"), Settings: testSettings()})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.Suggested != c.wantName || p.Resource.Size != c.wantSize {
			t.Errorf("%s: name %q size %d, want %q %d", name, p.Suggested, p.Resource.Size, c.wantName, c.wantSize)
		}
	}
}

func TestResolveFailures(t *testing.T) {
	e := newEngine(t, fakeLocator{}, nil)
	req := engine.Request{Source: src("https://v.example/x"), Settings: testSettings()}
	for name, c := range map[string]struct {
		env  map[string]string
		kind engine.Kind
	}{
		"playlist":    {map[string]string{"FAKE_JSON": `{"_type":"playlist","title":"P"}`}, engine.KindFatal},
		"not json":    {map[string]string{"FAKE_JSON": `<html>`}, engine.KindFatal},
		"unsupported": {map[string]string{"FAKE_MODE": "resolve-fail", "FAKE_ERROR": "Unsupported URL: https://v.example/x"}, engine.KindFatal},
		"gone":        {map[string]string{"FAKE_MODE": "resolve-fail", "FAKE_ERROR": "HTTP Error 404: Not Found"}, engine.KindLinkExpired},
		"timeout":     {map[string]string{"FAKE_MODE": "resolve-fail", "FAKE_ERROR": "Unable to download webpage: timed out"}, engine.KindTransient},
	} {
		scenario(t, c.env)
		_, err := e.Resolve(t.Context(), req)
		if err == nil || engine.Classify(err) != c.kind {
			t.Errorf("%s: err = %v (kind %v), want %v", name, err, engine.Classify(err), c.kind)
		}
	}
	// Option injection and a missing yt-dlp.
	scenario(t, nil)
	_, err := e.Resolve(t.Context(), engine.Request{Source: src("--exec=touch /tmp/pwned"), Settings: testSettings()})
	wantFatal(t, err)
	_, err = newEngine(t, fakeLocator{err: ytdlp.ErrNotFound}, nil).Resolve(t.Context(), req)
	wantFatal(t, err)
	if !errors.Is(err, ytdlp.ErrNotFound) {
		t.Errorf("err = %v, want ytdlp.ErrNotFound", err)
	}
	_, err = New(engine.Env{}, nil, nil).Resolve(t.Context(), req)
	wantFatal(t, err)
}

func TestArgumentsFollowTheSourceAndSettings(t *testing.T) {
	log := scenario(t, nil)
	cookieFor := func(_ context.Context, u *url.URL) ([]*domain.Cookie, error) {
		return []*domain.Cookie{{Name: "sid", Value: "s3cret", Domain: u.Hostname(), Path: "/", HostOnly: true}}, nil
	}
	e := newEngine(t, fakeLocator{}, nil, WithCookies(cookieFor), WithTempDir(t.TempDir()))
	source := domain.Source{
		CurrentURL: "https://v.example/watch", PageURL: "https://v.example/page", ResolverKind: domain.ResolverYtdlp,
		MediaHint: "bv*[height<=720]+ba/b", Headers: map[string]string{"X-A": "1"}, UserAgent: "Agent/1",
	}
	s := testSettings(func(s *settings.Settings) {
		s.Proxy = settings.Some(settings.Proxy{Mode: settings.ProxyCustom, URL: "socks5://p.example:1080"})
		s.TLSVerify = settings.Some(false)
		s.Retry.MaxAttempts = settings.Some(7)
		s.SpeedLimit = settings.Some[int64](250_000)
		s.Headers = settings.Some(map[string]string{"X-B": "2"})
	})
	if _, err := e.Resolve(t.Context(), engine.Request{Source: source, Settings: s}); err != nil {
		t.Fatal(err)
	}
	c := calls(t, log)[0]
	for opt, want := range map[string]string{
		"format": "bv*[height<=720]+ba/b", "proxy": "socks5://p.example:1080", "user-agent": "Agent/1",
		"referer": "https://v.example/page", "retries": "7", "fragment-retries": "7", "limit-rate": "250000",
	} {
		if got, ok := c.opt(opt); !ok || got != want {
			t.Errorf("--%s = %q (set %v), want %q", opt, got, ok, want)
		}
	}
	headers := map[string]bool{}
	for _, a := range c.Args {
		if h, ok := strings.CutPrefix(a, "--add-header="); ok {
			headers[h] = true
		}
	}
	if !c.has("--no-check-certificates") || !headers["X-A:1"] || !headers["X-B:2"] {
		t.Errorf("args = %v", c.Args)
	}
	// The cookies reached yt-dlp in a private file with the cookie in it, and
	// the file is gone afterwards.
	if !strings.Contains(c.Cookies, "s3cret") || !strings.Contains(c.Cookies, "# Netscape HTTP Cookie File") {
		t.Errorf("cookies file = %q", c.Cookies)
	}
	if runtime.GOOS != "windows" && c.CookiesMode != "-rw-------" {
		t.Errorf("cookies file mode = %s", c.CookiesMode)
	}
	if path, _ := c.opt("cookies"); path != "" {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the cookies file %s outlived the call: %v", path, err)
		}
	}

	// No proxy at all is said explicitly; the system proxy is left to yt-dlp.
	for mode, want := range map[settings.ProxyMode]string{settings.ProxyNone: "set", settings.ProxySystem: "unset"} {
		_ = os.Remove(log)
		s2 := testSettings(func(s *settings.Settings) { s.Proxy = settings.Some(settings.Proxy{Mode: mode}) })
		if _, err := e.Resolve(t.Context(), engine.Request{Source: domain.Source{CurrentURL: "https://v.example/w", ResolverKind: domain.ResolverYtdlp}, Settings: s2}); err != nil {
			t.Fatal(err)
		}
		v, ok := calls(t, log)[0].opt("proxy")
		if got := map[bool]string{true: "set", false: "unset"}[ok]; got != want || (ok && v != "") {
			t.Errorf("proxy mode %s: --proxy %q (set %v), want %s and empty", mode, v, ok, want)
		}
	}
}

func TestDownload(t *testing.T) {
	body := strings.Repeat("media!", 700)
	log := scenario(t, map[string]string{"FAKE_BODY": body})
	e := newEngine(t, fakeLocator{}, fakeFF{})
	dir := t.TempDir()
	rec := &recorder{}
	dl, err := e.NewDownload(spec(dir, "https://v.example/watch", "My Video.mp4", rec, testSettings()))
	if err != nil {
		t.Fatal(err)
	}
	if err := dl.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "My Video.mp4"))
	if err != nil || string(got) != body {
		t.Fatalf("file: %v (%d bytes)", err, len(got))
	}
	if _, err := os.Stat(filepath.Join(dir, "My Video.mp4"+WorkSuffix)); !os.IsNotExist(err) {
		t.Errorf("the work directory was left behind: %v", err)
	}
	if p := dl.Progress(); p.Downloaded != int64(len(body)) || p.Size != int64(len(body)) || p.Connections != 0 {
		t.Errorf("progress = %+v", p)
	}
	if rec.cp.Downloaded != int64(len(body)) || rec.cp.Size != int64(len(body)) {
		t.Errorf("checkpoint = %+v", rec.cp)
	}
	c := calls(t, log)[0]
	out, _ := c.opt("output")
	tmpl, _ := c.opt("progress-template")
	pr, _ := c.opt("print")
	if filepath.Dir(out) != filepath.Join(dir, "My Video.mp4"+WorkSuffix) || !strings.HasSuffix(out, "dl.%(ext)s") ||
		!strings.HasPrefix(tmpl, "download:DLM|") || pr != "after_move:DLMFILE|%(filepath)s" {
		t.Errorf("output %q template %q print %q", out, tmpl, pr)
	}
	if ff, _ := c.opt("ffmpeg-location"); ff != "/fake/ffmpeg" {
		t.Errorf("--ffmpeg-location = %q", ff)
	}
	for _, a := range []string{"--no-playlist", "--newline", "--continue", "--no-simulate"} {
		if !c.has(a) {
			t.Errorf("missing %s in %v", a, c.Args)
		}
	}
	if c.Args[len(c.Args)-2] != "--" || c.Args[len(c.Args)-1] != "https://v.example/watch" {
		t.Errorf("the URL must follow --: %v", c.Args)
	}
}

func TestMergedStreamsAddUp(t *testing.T) {
	scenario(t, map[string]string{"FAKE_MODE": "merge", "FAKE_BODY": "merged"})
	e := newEngine(t, fakeLocator{}, nil)
	sp := spec(t.TempDir(), "https://v.example/w", "v.mkv", nil, testSettings())
	sp.Resource = &domain.Resource{Name: "v.mkv", Size: 1200}
	dl, err := e.NewDownload(sp)
	if err != nil {
		t.Fatal(err)
	}
	var seen []engine.Progress
	done := make(chan error, 1)
	go func() { done <- dl.Run(t.Context()) }()
	for len(done) == 0 {
		seen = append(seen, dl.Progress())
		time.Sleep(time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// At the end the real size of the file is reported.
	if p := dl.Progress(); p.Downloaded != int64(len("merged")) || p.Size != int64(len("merged")) {
		t.Errorf("progress = %+v", p)
	}
	_ = seen
}

func TestProgressLines(t *testing.T) {
	d := newDownload(New(engine.Env{}, nil, nil), engine.Spec{Path: filepath.Join(t.TempDir(), "x")})
	d.progressLine("downloading|500|1000|NA|v.f1.mp4")
	if p := d.Progress(); p.Downloaded != 500 || p.Size != 1000 {
		t.Errorf("after the first stream: %+v", p)
	}
	d.progressLine("finished|1000|1000|NA|v.f1.mp4")
	d.progressLine("downloading|200|NA|400.5|a|b.m4a") // the file name may hold "|"
	if p := d.Progress(); p.Downloaded != 1200 || p.Size != 1400 {
		t.Errorf("with the second stream: %+v", p)
	}
	d.progressLine("finished|400|NA|NA|a|b.m4a")
	if p := d.Progress(); p.Downloaded != 1400 || p.Size != 1400 {
		t.Errorf("finished: %+v", p)
	}
	before := d.Progress()
	for _, junk := range []string{"", "x", "a|b|c", "downloading|NA|NA|NA|f"} {
		d.progressLine(junk)
	}
	if d.Progress() != before {
		t.Errorf("junk changed the progress: %+v", d.Progress())
	}
	for in, want := range map[string]int64{"12": 12, "12.9": 12, "NA": 0, "": 0, "-3": 0, "abc": 0} {
		if got := parseCount(in); got != want {
			t.Errorf("parseCount(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestFailuresAreClassified(t *testing.T) {
	e := newEngine(t, fakeLocator{}, nil)
	for msg, kind := range map[string]engine.Kind{
		"Unsupported URL: https://v.example/x":                     engine.KindFatal,
		"Private video. Sign in if you've been granted access":     engine.KindFatal,
		"[youtube] x: Video unavailable":                           engine.KindFatal,
		"unable to download video data: HTTP Error 403: Forbidden": engine.KindLinkExpired,
		"HTTP Error 404: Not Found":                                engine.KindLinkExpired,
		"This video has been removed by the uploader":              engine.KindLinkExpired,
		"HTTP Error 429: Too Many Requests":                        engine.KindTransient,
		"HTTP Error 503: Service Unavailable":                      engine.KindTransient,
		"Connection reset by peer":                                 engine.KindTransient,
		"<urlopen error timed out>":                                engine.KindTransient,
		"unable to write data: [Errno 28] No space left on device": engine.KindFatal,
	} {
		scenario(t, map[string]string{"FAKE_MODE": "fail", "FAKE_ERROR": msg})
		dl, err := e.NewDownload(spec(t.TempDir(), "https://v.example/x", "x.mp4", nil, testSettings()))
		if err != nil {
			t.Fatal(err)
		}
		err = dl.Run(t.Context())
		if err == nil || engine.Classify(err) != kind {
			t.Errorf("%q: err = %v (kind %v), want %v", msg, err, engine.Classify(err), kind)
		}
	}
	scenario(t, map[string]string{"FAKE_MODE": "fail", "FAKE_ERROR": "[Errno 28] No space left on device"})
	dl, _ := e.NewDownload(spec(t.TempDir(), "https://v.example/x", "x.mp4", nil, testSettings()))
	var ee *engine.Error
	if err := dl.Run(t.Context()); !errors.As(err, &ee) || ee.Code != domain.ErrorCodeDiskFull {
		t.Errorf("err = %v, want disk_full", err)
	}
}

func TestPauseKeepsPartialFilesAndResumes(t *testing.T) {
	scenario(t, map[string]string{"FAKE_MODE": "slow"})
	e := newEngine(t, fakeLocator{}, nil)
	dir := t.TempDir()
	sp := spec(dir, "https://v.example/x", "x.mp4", &recorder{}, testSettings())
	dl, err := e.NewDownload(sp)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- dl.Run(ctx) }()
	deadline := time.Now().Add(15 * time.Second)
	for dl.Progress().Downloaded == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no progress")
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after the cancellation")
	}
	if time.Since(start) > 15*time.Second {
		t.Errorf("stopping took %v", time.Since(start))
	}
	work := filepath.Join(dir, "x.mp4"+WorkSuffix)
	if _, err := os.Stat(filepath.Join(work, "dl.mp4.part")); err != nil {
		t.Errorf("the partial file is gone after a pause: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.mp4")); !os.IsNotExist(err) {
		t.Error("the final file exists after a pause")
	}
	// The next run continues in the same work directory.
	t.Setenv("FAKE_MODE", "ok")
	t.Setenv("FAKE_BODY", "complete")
	dl2, _ := e.NewDownload(sp)
	if err := dl2.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "x.mp4")); string(b) != "complete" {
		t.Errorf("file = %q", b)
	}
}

func TestOddOutcomes(t *testing.T) {
	e := newEngine(t, fakeLocator{}, nil)
	outside := filepath.Join(t.TempDir(), "elsewhere.mp4")
	for name, c := range map[string]map[string]string{
		"file outside the work directory": {"FAKE_MODE": "outside", "FAKE_BODY": "x", "FAKE_OUTSIDE": outside},
		"no file reported":                {"FAKE_MODE": "nofile", "FAKE_BODY": "x"},
	} {
		scenario(t, c)
		dl, _ := e.NewDownload(spec(t.TempDir(), "https://v.example/x", "x.mp4", nil, testSettings()))
		wantFatal(t, dl.Run(t.Context()))
		if name == "file outside the work directory" {
			if _, err := os.Stat(outside); err == nil {
				if err := os.Remove(outside); err != nil {
					t.Log(err)
				}
			}
		}
	}
}

func TestDestinationAndBadSpecs(t *testing.T) {
	scenario(t, map[string]string{"FAKE_BODY": "x"})
	e := newEngine(t, fakeLocator{}, nil)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.mp4"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	dl, err := e.NewDownload(spec(dir, "https://v.example/x", "x.mp4", nil, testSettings()))
	if err != nil {
		t.Fatal(err)
	}
	if err := dl.Run(t.Context()); !errors.Is(err, ErrDestinationExists) {
		t.Errorf("err = %v, want ErrDestinationExists", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "x.mp4")); string(b) != "mine" {
		t.Error("an existing file was overwritten")
	}
	rel := spec(dir, "https://v.example/x", "x", nil, testSettings())
	rel.Path = "relative"
	notYtdlp := spec(dir, "https://v.example/x", "x", nil, testSettings())
	notYtdlp.Source.ResolverKind = domain.ResolverDirect
	for name, sp := range map[string]engine.Spec{"relative path": rel, "other resolver": notYtdlp} {
		if _, err := e.NewDownload(sp); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := dl.Run(t.Context()); err == nil {
		t.Error("a second Run must fail")
	}
}

func TestDiskFullPrecheck(t *testing.T) {
	scenario(t, nil)
	e := newEngine(t, fakeLocator{}, nil, WithFreeSpace(func(string) (uint64, error) { return 10, nil }))
	sp := spec(t.TempDir(), "https://v.example/x", "x.mp4", nil, testSettings())
	sp.Resource = &domain.Resource{Name: "x.mp4", Size: 1_000_000}
	dl, _ := e.NewDownload(sp)
	err := dl.Run(t.Context())
	wantFatal(t, err)
	var ee *engine.Error
	if !errors.As(err, &ee) || ee.Code != domain.ErrorCodeDiskFull {
		t.Errorf("err = %v, want disk_full", err)
	}
}

func TestMissingFFmpegIsTolerated(t *testing.T) {
	log := scenario(t, map[string]string{"FAKE_BODY": "ok"})
	e := newEngine(t, fakeLocator{}, fakeFF{err: ffmpeg.ErrNotFound})
	dl, _ := e.NewDownload(spec(t.TempDir(), "https://v.example/x", "x.mp4", nil, testSettings()))
	if err := dl.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := calls(t, log)[0].opt("ffmpeg-location"); ok {
		t.Error("--ffmpeg-location was passed although no ffmpeg was found")
	}
}

func TestBinaryMissingIsFatal(t *testing.T) {
	scenario(t, nil)
	e := newEngine(t, fakeLocator{err: ytdlp.ErrNotFound}, nil)
	dl, _ := e.NewDownload(spec(t.TempDir(), "https://v.example/x", "x.mp4", nil, testSettings()))
	err := dl.Run(t.Context())
	wantFatal(t, err)
	if !errors.Is(err, ytdlp.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}
