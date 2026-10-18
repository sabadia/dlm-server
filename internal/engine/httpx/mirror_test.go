package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func mirrorServer(t *testing.T, fs *fileServer) string {
	t.Helper()
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	return srv.URL + "/files/file.bin"
}

func slowFile(data []byte) *fileServer {
	return &fileServer{data: data, etag: `"m"`, chunk: 32 << 10, delay: time.Millisecond}
}

func TestMirrorsShareTheDownload(t *testing.T) {
	data := testData(6<<20, 7)
	primary := slowFile(data)
	primary.etag = `"v1"`
	m1, m2 := slowFile(data), slowFile(data)
	h := newHarness(t, primary)
	spec := h.specFromProbe(h.resolve(), &recorder{})
	spec.Source.Mirrors = []string{mirrorServer(t, m1), mirrorServer(t, m2)}
	if _, err := h.run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	h.checkFile()
	for name, m := range map[string]*fileServer{"mirror 1": m1, "mirror 2": m2} {
		reqs := m.requests()
		if len(reqs) < 2 {
			t.Errorf("%s got %d requests, want its probe and ranged requests", name, len(reqs))
		}
		for _, r := range reqs {
			if r.ifRange != "" {
				t.Errorf("%s got If-Range %q: the primary's validator means nothing there", name, r.ifRange)
			}
		}
		if m.served.Load() == 0 {
			t.Errorf("%s served no bytes", name)
		}
	}
}

func TestUnusableMirrorsAreIgnored(t *testing.T) {
	data := testData(2<<20, 8)
	h := newHarness(t, &fileServer{data: data, etag: `"v1"`, chunk: 32 << 10})
	wrongSize := &fileServer{data: testData(1<<20, 9)}
	noRange := &fileServer{data: data, noRange: true}
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL + "/files/file.bin"
	dead.Close()
	spec := h.specFromProbe(h.resolve(), &recorder{})
	spec.Source.Mirrors = []string{mirrorServer(t, wrongSize), mirrorServer(t, noRange), deadURL}
	if _, err := h.run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	h.checkFile()
	if n := len(wrongSize.requests()); n != 1 {
		t.Errorf("the wrong-size mirror got %d requests, want only the probe", n)
	}
	if n := len(noRange.requests()); n != 1 {
		t.Errorf("the mirror without ranges got %d requests, want only the probe", n)
	}
}

func TestMirrorThatFailsLaterIsDropped(t *testing.T) {
	data := testData(6<<20, 10)
	primary := slowFile(data)
	bad := slowFile(data)
	var mu sync.Mutex
	probed := false
	bad.hook = func(w http.ResponseWriter, _ *http.Request, _ int, _ reqInfo) bool {
		mu.Lock()
		first := !probed
		probed = true
		mu.Unlock()
		if first {
			return false // survives the probe
		}
		w.WriteHeader(http.StatusInternalServerError)
		return true
	}
	h := newHarness(t, primary)
	spec := h.specFromProbe(h.resolve(), &recorder{})
	spec.Source.Mirrors = []string{mirrorServer(t, bad)}
	if _, err := h.run(context.Background(), spec); err != nil {
		t.Fatalf("a failing mirror must not fail the download: %v", err)
	}
	h.checkFile()
	if n := len(bad.requests()); n > 1+mirrorStrikes+1 {
		t.Errorf("the failing mirror got %d requests; it should be dropped after %d failures", n, mirrorStrikes)
	}
}

func TestCapturedHeadersStayOnTheirHost(t *testing.T) {
	data := testData(2<<20, 11)
	var mu sync.Mutex
	var seen []string
	mirror := slowFile(data)
	mirror.hook = func(_ http.ResponseWriter, r *http.Request, _ int, _ reqInfo) bool {
		mu.Lock()
		seen = append(seen, r.Header.Get("X-Captured"))
		mu.Unlock()
		return false
	}
	h := newHarness(t, slowFile(data))
	spec := h.specFromProbe(h.resolve(), &recorder{})
	spec.Source.Headers = map[string]string{"X-Captured": "secret"}
	spec.Source.Mirrors = []string{mirrorServer(t, mirror)}
	if _, err := h.run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the mirror was never requested")
	}
	for _, v := range seen {
		if v != "" {
			t.Errorf("a captured header was sent to another host: %q", v)
		}
	}
}

func TestMirrorSetRotationAndStrikes(t *testing.T) {
	var nilSet *mirrorSet
	if u, m := nilSet.pick("p"); u != "p" || m {
		t.Errorf("nil set pick = %q, %v", u, m)
	}
	nilSet.fail("x") // must not panic
	s := newMirrorSet([]string{"a", "b"})
	counts := map[string]int{}
	for range 30 {
		u, mirror := s.pick("p")
		counts[u]++
		if mirror != (u != "p") {
			t.Fatalf("pick = %q, mirror %v", u, mirror)
		}
	}
	if counts["p"] != 10 || counts["a"] != 10 || counts["b"] != 10 {
		t.Errorf("rotation = %v, want even", counts)
	}
	for range mirrorStrikes {
		s.fail("a")
	}
	counts = map[string]int{}
	for range 20 {
		u, _ := s.pick("p")
		counts[u]++
	}
	if counts["a"] != 0 || counts["p"] != 10 || counts["b"] != 10 {
		t.Errorf("after strikes = %v: a must be out, the primary never", counts)
	}
}
