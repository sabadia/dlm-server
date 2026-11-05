package segmented

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/settings"
)

// A site that allows only a few connections is not asked for more, however
// many workers and downloads there are.
func TestFetchKeepsToTheHostCap(t *testing.T) {
	const limit = 4
	var inFlight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		_, _ = w.Write([]byte("segment"))
	}))
	t.Cleanup(srv.Close)

	c := Client{HTTP: srv.Client(), Settings: settings.Settings{MaxConnectionsPerHost: settings.Some(limit)}}
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := fetch(t.Context(), c, srv.URL+"/seg.ts", nil, 1<<10, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := peak.Load(); got > limit {
		t.Errorf("the server saw %d requests at once, the cap is %d", got, limit)
	}
}
