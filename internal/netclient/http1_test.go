package netclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sabadia/dlm-server/internal/settings"
)

// fetchParallel makes n overlapping requests with a client for s and returns the
// protocol seen and the number of TCP connections the server accepted.
func fetchParallel(t *testing.T, s settings.Settings, n int) (proto string, conns int) {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]bool{}
	release := make(chan struct{})
	var arrived sync.WaitGroup
	arrived.Add(n)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.RemoteAddr] = true
		mu.Unlock()
		arrived.Done()
		<-release // hold every request open until all n are in flight
		_, _ = io.WriteString(w, "ok")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	f := New(Options{Getenv: noEnv, SystemProxy: noOSProxy})
	t.Cleanup(func() { _ = f.Close() })
	s.TLSVerify = settings.Some(false)
	s.Proxy = settings.Some(settings.Proxy{Mode: settings.ProxyNone})
	c, err := f.Client(s)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	protos := make(chan string, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.Get(srv.URL)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			protos <- resp.Proto
		}()
	}
	arrived.Wait()
	close(release)
	wg.Wait()
	close(protos)
	mu.Lock()
	defer mu.Unlock()
	return <-protos, len(seen)
}

func TestParallelDownloadsUseOneConnectionEach(t *testing.T) {
	proto, conns := fetchParallel(t, settings.Settings{MaxConnections: settings.Some(8)}, 8)
	if proto != "HTTP/1.1" || conns != 8 {
		t.Errorf("8 connections allowed: %s over %d TCP connections, want HTTP/1.1 over 8", proto, conns)
	}
}

func TestSingleConnectionMayUseHTTP2(t *testing.T) {
	proto, conns := fetchParallel(t, settings.Settings{MaxConnections: settings.Some(1)}, 4)
	if proto != "HTTP/2.0" || conns != 1 {
		t.Errorf("one connection allowed: %s over %d TCP connections, want HTTP/2.0 over 1", proto, conns)
	}
}
