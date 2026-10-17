package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
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
	"github.com/sabadia/dlm-server/internal/ratelimit"
	"github.com/sabadia/dlm-server/internal/settings"
)

// testData returns n deterministic pseudo-random bytes.
func testData(n int, seed uint64) []byte {
	var s [32]byte
	s[0] = byte(seed)
	r := rand.NewChaCha8(s)
	b := make([]byte, n)
	_, _ = r.Read(b)
	return b
}

type reqInfo struct {
	start, end int64 // end inclusive; -1 when open-ended or no range
	ranged     bool
	ifRange    string
}

// fileServer serves data with hand-written range handling so tests can
// inject faults. hook, when set, runs first and may handle the request
// (returning true); n is the 1-based request number.
type fileServer struct {
	data    []byte
	etag    string
	noRange bool // ignore Range and do not advertise ranges
	chunked bool // omit Content-Length (unknown size)
	chunk   int
	delay   time.Duration // after each chunk
	// Ranges starting at or after slowFrom (when > 0) use slowDelay.
	slowFrom  int64
	slowDelay time.Duration
	hook      func(w http.ResponseWriter, r *http.Request, n int, ri reqInfo) bool

	mu     sync.Mutex
	reqs   []reqInfo
	served atomic.Int64
	count  atomic.Int32
}

func (fs *fileServer) requests() []reqInfo {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]reqInfo(nil), fs.reqs...)
}

func parseRangeHeader(v string, size int64) (reqInfo, bool) {
	spec, ok := strings.CutPrefix(v, "bytes=")
	if !ok {
		return reqInfo{}, false
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return reqInfo{}, false
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil {
		return reqInfo{}, false
	}
	end := size - 1
	if b != "" {
		if end, err = strconv.ParseInt(b, 10, 64); err != nil {
			return reqInfo{}, false
		}
	}
	return reqInfo{start: start, end: min(end, size-1), ranged: true}, true
}

func (fs *fileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := int(fs.count.Add(1))
	size := int64(len(fs.data))
	ri := reqInfo{start: 0, end: -1}
	if h := r.Header.Get("Range"); h != "" && !fs.noRange {
		if p, ok := parseRangeHeader(h, size); ok {
			ri = p
		}
	}
	ri.ifRange = r.Header.Get("If-Range")
	fs.mu.Lock()
	fs.reqs = append(fs.reqs, ri)
	fs.mu.Unlock()
	if fs.hook != nil && fs.hook(w, r, n, ri) {
		return
	}
	if fs.etag != "" {
		w.Header().Set("ETag", fs.etag)
	}
	if ri.ifRange != "" && ri.ifRange != fs.etag {
		ri.ranged = false // validator failed: full response
	}
	body := fs.data
	if ri.ranged {
		if ri.start >= size {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		body = fs.data[ri.start : ri.end+1]
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(ri.start, 10)+"-"+strconv.FormatInt(ri.end, 10)+"/"+strconv.FormatInt(size, 10))
	}
	if !fs.noRange {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	if !fs.chunked {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if ri.ranged {
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	delay := fs.delay
	if fs.slowFrom > 0 && ri.start >= fs.slowFrom {
		delay = fs.slowDelay
	}
	fs.write(w, r, body, delay)
}

func (fs *fileServer) write(w http.ResponseWriter, r *http.Request, body []byte, delay time.Duration) {
	chunk := fs.chunk
	if chunk <= 0 {
		chunk = 32 << 10
	}
	fl, _ := w.(http.Flusher)
	for len(body) > 0 {
		c := min(chunk, len(body))
		m, err := w.Write(body[:c])
		fs.served.Add(int64(m))
		if err != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
		body = body[c:]
		if delay > 0 && len(body) > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
		}
	}
}

type fixedClients struct{ c *http.Client }

func (f fixedClients) Client(settings.Settings) (*http.Client, error) { return f.c, nil }

type recorder struct {
	mu  sync.Mutex
	cps []engine.Checkpoint
}

func (r *recorder) Checkpoint(_ context.Context, cp engine.Checkpoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cps = append(r.cps, cp)
	return nil
}

func (r *recorder) last(t *testing.T) engine.Checkpoint {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cps) == 0 {
		t.Fatal("no checkpoint written")
	}
	return r.cps[len(r.cps)-1]
}

type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return ctx.Err()
}

type harness struct {
	t    *testing.T
	fs   *fileServer
	srv  *httptest.Server
	eng  *Engine
	sl   *sleeps
	dir  string
	set  settings.Settings
	path string
}

func newHarness(t *testing.T, fs *fileServer, opts ...Option) *harness {
	t.Helper()
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	h := &harness{t: t, fs: fs, srv: srv, sl: &sleeps{}, dir: t.TempDir()}
	h.path = filepath.Join(h.dir, "sub", "file.bin")
	h.set = settings.Defaults(h.dir)
	h.set.MaxConnections = settings.Some(4)
	h.set.Retry.MaxAttempts = settings.Some(3)
	base := []Option{
		WithMinSplitSize(64 << 10),
		WithSlowStartInterval(10 * time.Millisecond),
		WithCheckpointInterval(20 * time.Millisecond),
		WithStallTimeout(5 * time.Second),
		WithRetrySleeper(h.sl.sleep),
		WithFreeSpace(func(string) (uint64, error) { return 1 << 40, nil }),
	}
	h.eng = New(engine.Env{Clients: fixedClients{srv.Client()}}, append(base, opts...)...)
	return h
}

func (h *harness) url() string { return h.srv.URL + "/files/file.bin" }

func (h *harness) resolve() *engine.Probe {
	h.t.Helper()
	p, err := h.eng.Resolve(context.Background(), engine.Request{Source: domain.Source{CurrentURL: h.url()}, Settings: h.set})
	if err != nil {
		h.t.Fatalf("resolve: %v", err)
	}
	return p
}

// specFromProbe builds a spec that consumes the probe.
func (h *harness) specFromProbe(p *engine.Probe, cp engine.Checkpointer) engine.Spec {
	res := p.Resource
	return engine.Spec{TaskID: "t1", Source: p.Source, Resource: &res, Path: h.path, Settings: h.set, Probe: p, Checkpointer: cp}
}

// specNoProbe builds a spec without a probe attachment.
func (h *harness) specNoProbe(cp engine.Checkpointer) engine.Spec {
	size := int64(len(h.fs.data))
	src := domain.Source{CurrentURL: h.url(), Fingerprint: domain.Fingerprint{Size: size, ETag: h.fs.etag}}
	res := domain.Resource{Name: "file.bin", Size: size, RangeSupported: !h.fs.noRange}
	return engine.Spec{TaskID: "t1", Source: src, Resource: &res, Path: h.path, Settings: h.set, Checkpointer: cp}
}

func (h *harness) run(ctx context.Context, spec engine.Spec) (*Download, error) {
	h.t.Helper()
	dl, err := h.eng.NewDownload(spec)
	if err != nil {
		h.t.Fatalf("NewDownload: %v", err)
	}
	d, ok := dl.(*Download)
	if !ok {
		h.t.Fatalf("NewDownload returned %T", dl)
	}
	return d, d.Run(ctx)
}

func (h *harness) checkFile() {
	h.t.Helper()
	got, err := os.ReadFile(h.path)
	if err != nil {
		h.t.Fatalf("read result: %v", err)
	}
	if sha256.Sum256(got) != sha256.Sum256(h.fs.data) {
		h.t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(h.fs.data))
	}
	if _, err := os.Stat(h.path + PartSuffix); !errors.Is(err, os.ErrNotExist) {
		h.t.Errorf(".part file still present: %v", err)
	}
}

func wantKind(t *testing.T, err error, kind engine.Kind) {
	t.Helper()
	if err == nil {
		t.Fatalf("Run succeeded, want %v error", kind)
	}
	if got := engine.Classify(err); got != kind {
		t.Fatalf("Classify(%v) = %v, want %v", err, got, kind)
	}
}

func TestRangedMultiConnectionWorkStealing(t *testing.T) {
	before := runtime.NumGoroutine()
	// Ranges in the second half are served slowly, so the connections on
	// the first half finish early and must steal from them.
	fs := &fileServer{
		data: testData(8<<20, 1), etag: `"v1"`, chunk: 32 << 10, delay: time.Millisecond,
		slowFrom: 4 << 20, slowDelay: 8 * time.Millisecond,
	}
	h := newHarness(t, fs)
	rec := &recorder{}
	d, err := h.run(context.Background(), h.specFromProbe(h.resolve(), rec))
	if err != nil {
		t.Fatal(err)
	}
	h.checkFile()

	reqs := fs.requests()
	atZero := 0
	ranged := 0
	for _, r := range reqs {
		if r.start == 0 {
			atZero++
		}
		if r.ranged && r.start > 0 {
			ranged++
			if r.ifRange != `"v1"` {
				t.Errorf("range request without If-Range: %+v", r)
			}
		}
	}
	if atZero != 1 {
		t.Errorf("%d requests started at offset 0, want 1 (the probe)", atZero)
	}
	if ranged < 1 {
		t.Errorf("no extra connections were used: %+v", reqs)
	}
	cp := rec.last(t)
	if cp.Downloaded != int64(len(fs.data)) || cp.Size != int64(len(fs.data)) {
		t.Errorf("final checkpoint = %d/%d", cp.Downloaded, cp.Size)
	}
	if len(cp.Segments) <= h.set.MaxConnections.Value() {
		t.Errorf("%d segments for %d connections: no work stealing: %+v", len(cp.Segments), h.set.MaxConnections.Value(), cp.Segments)
	}
	if p := d.Progress(); p.Connections != 0 || p.Downloaded != int64(len(fs.data)) {
		t.Errorf("progress after run = %+v", p)
	}
	h.srv.Close()
	h.srv.Client().CloseIdleConnections()
	checkGoroutines(t, before)
}

// checkGoroutines waits for the goroutine count to return near before.
func checkGoroutines(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= before+2 {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines: before %d, after %d\n%s", before, n, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestOneTimeURLUsesProbeBody(t *testing.T) {
	fs := &fileServer{data: testData(1<<20, 2), etag: `"v1"`}
	fs.hook = func(w http.ResponseWriter, _ *http.Request, n int, _ reqInfo) bool {
		if n > 1 {
			w.WriteHeader(http.StatusForbidden)
			return true
		}
		return false
	}
	h := newHarness(t, fs)
	h.set.MaxConnections = settings.Some(1)
	if _, err := h.run(context.Background(), h.specFromProbe(h.resolve(), nil)); err != nil {
		t.Fatal(err)
	}
	h.checkFile()
	if n := fs.count.Load(); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

func TestSequentialNoRangeServer(t *testing.T) {
	fs := &fileServer{data: testData(2<<20+123, 3), noRange: true}
	h := newHarness(t, fs)
	p := h.resolve()
	if p.Resource.RangeSupported {
		t.Fatal("resource unexpectedly supports ranges")
	}
	if _, err := h.run(context.Background(), h.specFromProbe(p, nil)); err != nil {
		t.Fatal(err)
	}
	h.checkFile()
	if n := fs.count.Load(); n != 1 {
		t.Errorf("%d requests, want 1 (probe body reused)", n)
	}
}

func TestUnknownSizeChunked(t *testing.T) {
	fs := &fileServer{data: testData(1<<20+7, 4), noRange: true, chunked: true}
	h := newHarness(t, fs)
	rec := &recorder{}
	d, err := h.run(context.Background(), h.specFromProbe(h.resolve(), rec))
	if err != nil {
		t.Fatal(err)
	}
	h.checkFile()
	if p := d.Progress(); p.Size != int64(len(fs.data)) {
		t.Errorf("size = %d, want %d", p.Size, len(fs.data))
	}
	if cp := rec.last(t); cp.Size != int64(len(fs.data)) {
		t.Errorf("final checkpoint size = %d", cp.Size)
	}
}

func TestRangeIgnoredOnMismatch(t *testing.T) {
	newFS := func() *fileServer {
		// The server's validator changed: If-Range "v1" no longer matches
		// "v2", so every range request is answered with the full file.
		return &fileServer{data: testData(1<<20, 5), etag: `"v2"`}
	}
	t.Run("restart", func(t *testing.T) {
		fs := newFS()
		h := newHarness(t, fs)
		h.set.Refresh.OnMismatch = settings.Some(settings.MismatchRestart)
		rec := &recorder{}
		spec := h.specNoProbe(rec)
		spec.Source.Fingerprint.ETag = `"v1"`
		if _, err := h.run(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		h.checkFile()
		cp := rec.last(t)
		if cp.Source == nil || cp.Source.Fingerprint.ETag != `"v2"` {
			t.Errorf("checkpoint source = %+v, want new etag", cp.Source)
		}
		if n := fs.count.Load(); n != 1 {
			t.Errorf("%d requests, want 1 (the 200 response is streamed)", n)
		}
	})
	t.Run("fail", func(t *testing.T) {
		fs := newFS()
		h := newHarness(t, fs)
		h.set.Refresh.OnMismatch = settings.Some(settings.MismatchFail)
		spec := h.specNoProbe(nil)
		spec.Source.Fingerprint.ETag = `"v1"`
		_, err := h.run(context.Background(), spec)
		wantKind(t, err, engine.KindFatal)
		if !errors.Is(err, ErrRangeIgnored) {
			t.Errorf("err = %v, want ErrRangeIgnored", err)
		}
		if _, err := os.Stat(h.path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("destination exists after failure: %v", err)
		}
		if _, err := os.Stat(h.path + PartSuffix); err != nil {
			t.Errorf(".part missing after failure: %v", err)
		}
	})
}

func TestContentRangeMismatch(t *testing.T) {
	data := testData(256<<10, 6)
	n := strconv.Itoa(len(data))
	tests := []struct {
		name string
		cr   func(ri reqInfo) string
	}{
		{"wrong start", func(ri reqInfo) string {
			return "bytes " + strconv.FormatInt(ri.start+1, 10) + "-" + strconv.FormatInt(ri.end, 10) + "/" + n
		}},
		{"wrong total", func(ri reqInfo) string {
			return "bytes " + strconv.FormatInt(ri.start, 10) + "-" + strconv.FormatInt(ri.end, 10) + "/" + n + "0"
		}},
		{"beyond requested end", func(ri reqInfo) string {
			return "bytes " + strconv.FormatInt(ri.start, 10) + "-" + strconv.FormatInt(ri.end+1, 10) + "/" + n
		}},
		{"malformed", func(reqInfo) string { return "bytes garbage" }},
		{"missing", func(reqInfo) string { return "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &fileServer{data: data, etag: `"v1"`}
			fs.hook = func(w http.ResponseWriter, _ *http.Request, _ int, ri reqInfo) bool {
				if cr := tt.cr(ri); cr != "" {
					w.Header().Set("Content-Range", cr)
				}
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(data[ri.start : ri.end+1])
				return true
			}
			h := newHarness(t, fs)
			h.set.MaxConnections = settings.Some(1)
			_, err := h.run(context.Background(), h.specNoProbe(nil))
			wantKind(t, err, engine.KindFatal)
			if !errors.Is(err, ErrRangeMismatch) {
				t.Errorf("err = %v, want ErrRangeMismatch", err)
			}
		})
	}
}

func TestFlakyServerRecovers(t *testing.T) {
	fs := &fileServer{data: testData(4<<20, 7), etag: `"v1"`}
	fs.hook = func(w http.ResponseWriter, _ *http.Request, n int, ri reqInfo) bool {
		if n%2 == 0 {
			return false
		}
		// Odd requests send half their range, then drop the connection.
		body := fs.data[ri.start : ri.end+1]
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(ri.start, 10)+"-"+strconv.FormatInt(ri.end, 10)+"/"+strconv.Itoa(len(fs.data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[:len(body)/2])
		flush(w)
		panic(http.ErrAbortHandler)
	}
	h := newHarness(t, fs)
	if _, err := h.run(context.Background(), h.specNoProbe(nil)); err != nil {
		t.Fatal(err)
	}
	h.checkFile()
	h.sl.mu.Lock()
	defer h.sl.mu.Unlock()
	if len(h.sl.d) == 0 {
		t.Error("no retries happened")
	}
}

func TestRetryAfterHonoured(t *testing.T) {
	fs := &fileServer{data: testData(512<<10, 8), etag: `"v1"`}
	fs.hook = func(w http.ResponseWriter, _ *http.Request, n int, _ reqInfo) bool {
		if n == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	}
	h := newHarness(t, fs)
	if _, err := h.run(context.Background(), h.specNoProbe(nil)); err != nil {
		t.Fatal(err)
	}
	h.checkFile()
	h.sl.mu.Lock()
	defer h.sl.mu.Unlock()
	if len(h.sl.d) != 1 || h.sl.d[0] < 7*time.Second {
		t.Errorf("sleeps = %v, want one >= 7s", h.sl.d)
	}
}

func TestForbiddenIsLinkExpired(t *testing.T) {
	fs := &fileServer{data: testData(256<<10, 9), etag: `"v1"`}
	fs.hook = func(w http.ResponseWriter, _ *http.Request, _ int, _ reqInfo) bool {
		w.WriteHeader(http.StatusForbidden)
		return true
	}
	h := newHarness(t, fs)
	_, err := h.run(context.Background(), h.specNoProbe(nil))
	wantKind(t, err, engine.KindLinkExpired)
	if _, err := os.Stat(h.path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("destination exists after failure: %v", err)
	}
}

func TestStallDetection(t *testing.T) {
	fs := &fileServer{data: testData(512<<10, 10), etag: `"v1"`}
	fs.hook = func(w http.ResponseWriter, r *http.Request, n int, ri reqInfo) bool {
		if n != 1 {
			return false
		}
		body := fs.data[ri.start : ri.end+1]
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(ri.start, 10)+"-"+strconv.FormatInt(ri.end, 10)+"/"+strconv.Itoa(len(fs.data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[:1000])
		flush(w)
		<-r.Context().Done() // stop sending until the client gives up
		return true
	}
	h := newHarness(t, fs, WithStallTimeout(200*time.Millisecond))
	h.set.MaxConnections = settings.Some(1)
	start := time.Now()
	if _, err := h.run(context.Background(), h.specNoProbe(nil)); err != nil {
		t.Fatal(err)
	}
	h.checkFile()
	if el := time.Since(start); el < 200*time.Millisecond {
		t.Errorf("finished in %v, before the stall timeout", el)
	}
	if n := fs.count.Load(); n < 2 {
		t.Errorf("%d requests, want a retry after the stall", n)
	}
	reqs := fs.requests()
	if reqs[1].start != 1000 {
		t.Errorf("retry started at %d, want 1000 (resume)", reqs[1].start)
	}
}

func TestCancelThenResume(t *testing.T) {
	fs := &fileServer{data: testData(6<<20, 11), etag: `"v1"`, chunk: 16 << 10, delay: time.Millisecond}
	h := newHarness(t, fs)
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dl, err := h.eng.NewDownload(h.specFromProbe(h.resolve(), rec))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- dl.Run(ctx) }()
	for dl.Progress().Downloaded < int64(len(fs.data))/4 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	err = <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	wantKind(t, err, engine.KindCanceled)
	cp := rec.last(t)
	if cp.Downloaded <= 0 || cp.Downloaded >= int64(len(fs.data)) {
		t.Fatalf("final checkpoint downloaded = %d", cp.Downloaded)
	}
	if _, ok := validateSegments(cp.Segments, int64(len(fs.data))); !ok {
		t.Fatalf("checkpoint segments invalid: %+v", cp.Segments)
	}
	if dl.Progress().Connections != 0 {
		t.Errorf("connections after cancel = %d", dl.Progress().Connections)
	}

	// Resume from the checkpoint with a fresh Download.
	seen := len(fs.requests())
	spec := h.specNoProbe(&recorder{})
	spec.Segments = cp.Segments
	if _, err := h.run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	h.checkFile()
	for _, r := range fs.requests()[seen:] {
		for _, s := range cp.Segments {
			if s.Done == 0 {
				continue
			}
			if r.start < s.Start+s.Done && r.end >= s.Start {
				t.Errorf("resume request %d-%d re-downloads completed bytes %d-%d", r.start, r.end, s.Start, s.Start+s.Done-1)
			}
		}
	}
}

func TestRestoredSegmentsInvalid(t *testing.T) {
	tests := []struct {
		name    string
		segs    []domain.Segment
		writeFS bool
	}{
		{"gap", []domain.Segment{{Start: 0, End: 100, Done: 100}, {Start: 200, End: 256 << 10}}, true},
		{"part missing", []domain.Segment{{Start: 0, End: 256 << 10, Done: 1000}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &fileServer{data: testData(256<<10, 12), etag: `"v1"`}
			h := newHarness(t, fs)
			if tt.writeFS {
				if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(h.path+PartSuffix, bytes.Repeat([]byte{'x'}, len(fs.data)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			spec := h.specNoProbe(nil)
			spec.Segments = tt.segs
			if _, err := h.run(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			h.checkFile()
			if r := fs.requests(); len(r) == 0 || r[0].start != 0 {
				t.Errorf("download did not start over: %+v", r)
			}
		})
	}
}

func TestRateLimiter(t *testing.T) {
	fs := &fileServer{data: testData(1<<20, 13), etag: `"v1"`}
	h := newHarness(t, fs)
	spec := h.specNoProbe(nil)
	spec.Limiter = ratelimit.NewBucket(1 << 20) // 1 MiB/s, 256 KiB burst
	start := time.Now()
	if _, err := h.run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	h.checkFile()
	// At least (1 MiB - burst) / rate = 0.75s; allow generous slack.
	if el := time.Since(start); el < 500*time.Millisecond {
		t.Errorf("1 MiB at 1 MiB/s took %v", el)
	}
}

func TestDiskFullPrecheck(t *testing.T) {
	fs := &fileServer{data: testData(256<<10, 14), etag: `"v1"`}
	h := newHarness(t, fs, WithFreeSpace(func(string) (uint64, error) { return 1000, nil }))
	_, err := h.run(context.Background(), h.specNoProbe(nil))
	wantKind(t, err, engine.KindFatal)
	var ee *engine.Error
	if !errors.As(err, &ee) || ee.Code != domain.ErrorCodeDiskFull {
		t.Errorf("err = %v, want code disk_full", err)
	}
	if n := fs.count.Load(); n != 0 {
		t.Errorf("%d requests despite failed precheck", n)
	}
}

func TestDestinationExists(t *testing.T) {
	fs := &fileServer{data: testData(64<<10, 15), etag: `"v1"`}
	h := newHarness(t, fs)
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.path, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := h.run(context.Background(), h.specNoProbe(nil))
	wantKind(t, err, engine.KindFatal)
	if !errors.Is(err, ErrDestinationExists) {
		t.Errorf("err = %v, want ErrDestinationExists", err)
	}
	if b, _ := os.ReadFile(h.path); string(b) != "keep" {
		t.Errorf("existing file modified: %q", b)
	}
}

func TestRunWithoutResourceProbes(t *testing.T) {
	fs := &fileServer{data: testData(300<<10, 16), etag: `"v1"`}
	h := newHarness(t, fs)
	spec := engine.Spec{TaskID: "t", Source: domain.Source{CurrentURL: h.url()}, Path: h.path, Settings: h.set}
	if _, err := h.run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	h.checkFile()
}

func TestNewDownloadValidation(t *testing.T) {
	e := New(engine.Env{})
	ct := &closeTracker{ReadCloser: io.NopCloser(strings.NewReader(""))}
	pr := &ProbeResult{Response: &http.Response{Body: ct}}
	p := &engine.Probe{Engine: EngineName}
	p.Attach(pr)
	_, err := e.NewDownload(engine.Spec{Source: domain.Source{CurrentURL: "http://x/"}, Path: "relative/file", Probe: p})
	wantKind(t, err, engine.KindFatal)
	if !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("err = %v, want ErrInvalidArgument", err)
	}
	if !ct.closed {
		t.Error("probe response not closed on validation failure")
	}
	d, err := e.NewDownload(engine.Spec{Source: domain.Source{CurrentURL: "http://x/"}, Path: filepath.Join(t.TempDir(), "f")})
	if err != nil {
		t.Fatal(err)
	}
	if p := d.Progress(); p.Size != -1 || p.Downloaded != 0 {
		t.Errorf("progress before run = %+v", p)
	}
}

type closeTracker struct {
	io.ReadCloser
	closed bool
}

func (c *closeTracker) Close() error { c.closed = true; return c.ReadCloser.Close() }

func TestMatch(t *testing.T) {
	e := New(engine.Env{})
	for raw, want := range map[string]bool{
		"http://example.com/a":  true,
		"HTTPS://example.com/a": true,
		"ftp://example.com/a":   false,
		"magnet:?xt=urn:btih:x": false,
		"http:///nohost":        false,
		"":                      false,
	} {
		if got := e.Match(domain.Source{CurrentURL: raw}); got != want {
			t.Errorf("Match(%q) = %v, want %v", raw, got, want)
		}
	}
	if e.Name() != "http" {
		t.Errorf("Name = %q", e.Name())
	}
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
