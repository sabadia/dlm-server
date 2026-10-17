package dash

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/ffmpeg"
	"github.com/sabadia/dlm-server/internal/netclient"
	"github.com/sabadia/dlm-server/internal/settings"
)

func engineEnv() engine.Env { return engine.Env{} }

func srcOf(u string) domain.Source { return domain.Source{CurrentURL: u} }

type fakeFF struct{}

func (fakeFF) Locate(context.Context) (ffmpeg.Binary, error) {
	return ffmpeg.Binary{Path: "/fake/ffmpeg", Version: "1", Source: "path"}, nil
}

// fakeExec records ffmpeg invocations and "muxes" by concatenating the inputs.
type fakeExec struct {
	mu    sync.Mutex
	calls [][]string
}

func (f *fakeExec) run(_ context.Context, bin string, args []string) error {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{bin}, args...))
	f.mu.Unlock()
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

type origin struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

func newOrigin(t *testing.T, files map[string]string, mpdPath, mpd string) *origin {
	t.Helper()
	o := &origin{hits: map[string]int{}}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.hits[r.URL.Path]++
		o.mu.Unlock()
		if r.URL.Path == mpdPath {
			w.Header().Set("Content-Type", "application/dash+xml")
			_, _ = fmt.Fprint(w, mpd)
			return
		}
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(body))
	}))
	t.Cleanup(o.srv.Close)
	return o
}

func newEngine(t *testing.T) (*Engine, *fakeExec, string) {
	t.Helper()
	clients := netclient.New(netclient.Options{})
	t.Cleanup(func() { _ = clients.Close() })
	ex := &fakeExec{}
	noSleep := func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	e := New(engine.Env{Clients: clients}, fakeFF{}, WithExec(ex.run), WithRetrySleeper(noSleep), WithCheckpointInterval(10*time.Millisecond))
	return e, ex, t.TempDir()
}

func spec(dir, u, name string) engine.Spec {
	s := settings.Defaults(dir)
	s.Proxy = settings.Some(settings.Proxy{Mode: settings.ProxyNone})
	s.Retry.MaxAttempts = settings.Some(2)
	return engine.Spec{TaskID: "t1", Source: domain.Source{CurrentURL: u}, Path: filepath.Join(dir, name), Settings: s}
}

const demoMPD = `<MPD type="static" mediaPresentationDuration="PT4S"><Period>
 <AdaptationSet mimeType="video/mp4"><SegmentTemplate timescale="1" duration="2" initialization="v/init.mp4" media="v/$Number$.m4s"/><Representation id="v" bandwidth="100"/></AdaptationSet>
 <AdaptationSet mimeType="audio/mp4"><SegmentTemplate timescale="1" duration="2" initialization="a/init.mp4" media="a/$Number$.m4s"/><Representation id="a" bandwidth="10"/></AdaptationSet>
</Period></MPD>`

var demoFiles = map[string]string{
	"/v/init.mp4": "VI", "/v/1.m4s": "V1", "/v/2.m4s": "V2",
	"/a/init.mp4": "AI", "/a/1.m4s": "A1", "/a/2.m4s": "A2",
}

func TestDownloadMuxesVideoAndAudio(t *testing.T) {
	o := newOrigin(t, demoFiles, "/show.mpd", demoMPD)
	e, ex, dir := newEngine(t)
	dl, err := e.NewDownload(spec(dir, o.srv.URL+"/show.mpd", "out.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if err := dl.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	// The fake muxer concatenates the inputs: video track then audio track,
	// each with its init section once.
	if string(got) != "VIV1V2AIA1A2" {
		t.Errorf("output = %q", got)
	}
	if len(ex.calls) != 1 || strings.Count(strings.Join(ex.calls[0], " "), " -i ") != 2 {
		t.Errorf("ffmpeg calls = %v", ex.calls)
	}
	if o.hits["/v/init.mp4"] != 1 {
		t.Errorf("init fetched %d times, want 1", o.hits["/v/init.mp4"])
	}
}

func TestResolve(t *testing.T) {
	o := newOrigin(t, demoFiles, "/Big Show.mpd", demoMPD)
	e, _, dir := newEngine(t)
	s := spec(dir, o.srv.URL+"/Big%20Show.mpd", "x")
	p, err := e.Resolve(context.Background(), engine.Request{Source: s.Source, Settings: s.Settings})
	if err != nil {
		t.Fatal(err)
	}
	if p.Engine != EngineName || p.Suggested != "Big Show.mp4" || len(p.Resource.Files) != 1 {
		t.Errorf("probe = %+v", p)
	}
}

func TestUnsupportedAndExpired(t *testing.T) {
	e, _, dir := newEngine(t)
	for name, c := range map[string]struct {
		body string
		want engine.Kind
	}{
		"dynamic": {`<MPD type="dynamic"><Period/></MPD>`, engine.KindFatal},
		"html":    {`<!DOCTYPE html><html>login</html>`, engine.KindLinkExpired},
		"empty":   {`<MPD type="static"><Period/></MPD>`, engine.KindFatal},
	} {
		o := newOrigin(t, nil, "/m.mpd", c.body)
		s := spec(dir, o.srv.URL+"/m.mpd", "x")
		_, err := e.Resolve(context.Background(), engine.Request{Source: s.Source, Settings: s.Settings})
		if err == nil || engine.Classify(err) != c.want {
			t.Errorf("%s: err = %v (kind %v), want kind %v", name, err, engine.Classify(err), c.want)
		}
	}
}

func TestBadSpecs(t *testing.T) {
	e, _, dir := newEngine(t)
	s := spec(dir, "https://x/a.mp4", "x")
	if _, err := e.NewDownload(s); err == nil {
		t.Error("want an error for a non-MPD URL")
	}
	s = spec(dir, "https://x/a.mpd", "x")
	s.Path = "relative.mp4"
	if _, err := e.NewDownload(s); err == nil {
		t.Error("want an error for a relative path")
	}
}
