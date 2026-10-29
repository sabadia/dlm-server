package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/events"
)

type staticSource struct {
	mu      sync.Mutex
	targets []Target
	version uint64
	loads   int
}

func (s *staticSource) Targets(context.Context) ([]Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	return append([]Target(nil), s.targets...), nil
}

func (s *staticSource) Version() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

func (s *staticSource) set(ts ...Target) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targets = ts
	s.version++
}

type received struct {
	header  http.Header
	body    []byte
	payload Payload
}

func recorder(t *testing.T, status func(n int) int) (*httptest.Server, <-chan received, *atomic.Int32) {
	t.Helper()
	ch := make(chan received, 32)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		body, _ := io.ReadAll(r.Body)
		var p Payload
		_ = json.Unmarshal(body, &p)
		ch <- received{header: r.Header.Clone(), body: body, payload: p}
		w.WriteHeader(status(n))
	}))
	t.Cleanup(srv.Close)
	return srv, ch, &calls
}

func ok(int) int { return http.StatusNoContent }

func start(t *testing.T, src Source, opts Options) *events.Bus {
	t.Helper()
	bus := events.New(events.Options{})
	t.Cleanup(bus.Close)
	if opts.Backoff == nil {
		opts.Backoff = func(int) time.Duration { return time.Millisecond }
	}
	d := New(bus, src, opts)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	<-d.Ready()
	return bus
}

func next(t *testing.T, ch <-chan received) received {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery within 5s")
		return received{}
	}
}

func TestDeliversSignedEventsToSubscribedTargetsOnly(t *testing.T) {
	srv, ch, _ := recorder(t, ok)
	other, otherCh, otherCalls := recorder(t, ok)
	src := &staticSource{}
	src.set(
		Target{ID: "w1", URL: srv.URL, Events: []string{"task.status", "queue.completed"}, Secret: []byte("s3cret")},
		Target{ID: "w2", URL: other.URL, Events: []string{"task.deleted"}},
	)
	bus := start(t, src, Options{})

	bus.Publish(events.Event{Type: events.TypeTaskCreated, TaskID: "t0"}) // nobody wants it
	bus.Publish(events.Event{Type: events.TypeTaskStatus, TaskID: "t1", Data: map[string]string{"to": "completed"}})
	r := next(t, ch)
	if r.payload.Type != events.TypeTaskStatus || r.payload.TaskID != "t1" || r.payload.ID == 0 {
		t.Errorf("payload = %+v", r.payload)
	}
	if got := r.header.Get(HeaderEvent); got != "task.status" {
		t.Errorf("%s = %q", HeaderEvent, got)
	}
	if got, want := r.header.Get(HeaderSignature), Sign([]byte("s3cret"), r.body); got != want || got[:7] != "sha256=" {
		t.Errorf("%s = %q, want %q", HeaderSignature, got, want)
	}
	if r.header.Get(HeaderDelivery) == "" || r.header.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", r.header)
	}
	if otherCalls.Load() != 0 {
		t.Error("an unsubscribed webhook received an event")
	}
	bus.Publish(events.Event{Type: events.TypeTaskDeleted, TaskID: "t1"})
	if r := next(t, otherCh); r.payload.Type != events.TypeTaskDeleted || r.header.Get(HeaderSignature) != "" {
		t.Errorf("w2 got %+v (unsigned expected)", r)
	}
}

func TestProgressEventsAreNeverDelivered(t *testing.T) {
	srv, _, calls := recorder(t, ok)
	src := &staticSource{}
	src.set(Target{ID: "w", URL: srv.URL, Events: []string{"task.progress", "task.status"}})
	bus := start(t, src, Options{})
	bus.Publish(events.Event{Type: events.TypeTaskProgress, TaskID: "t"})
	bus.Publish(events.Event{Type: events.TypeTaskStatus, TaskID: "t"})
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Errorf("deliveries = %d, want only the status event", n)
	}
}

func TestRetriesTransientFailuresButNotRefusals(t *testing.T) {
	// 503, 429, then success: three attempts.
	srv, ch, calls := recorder(t, func(n int) int {
		switch n {
		case 1:
			return http.StatusServiceUnavailable
		case 2:
			return http.StatusTooManyRequests
		}
		return http.StatusOK
	})
	src := &staticSource{}
	src.set(Target{ID: "w", URL: srv.URL, Events: []string{"task.status"}})
	bus := start(t, src, Options{})
	bus.Publish(events.Event{Type: events.TypeTaskStatus, TaskID: "t"})
	for range 3 {
		next(t, ch)
	}
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 3 {
		t.Errorf("attempts = %d, want 3", n)
	}

	// A 404 is final: one attempt.
	gone, goneCh, goneCalls := recorder(t, func(int) int { return http.StatusNotFound })
	src.set(Target{ID: "g", URL: gone.URL, Events: []string{"task.status"}})
	bus.Publish(events.Event{Type: events.TypeTaskStatus, TaskID: "t2"})
	next(t, goneCh)
	time.Sleep(100 * time.Millisecond)
	if n := goneCalls.Load(); n != 1 {
		t.Errorf("attempts after 404 = %d, want 1", n)
	}
}

func TestGivesUpAfterTheAttemptLimit(t *testing.T) {
	srv, ch, calls := recorder(t, func(int) int { return http.StatusBadGateway })
	src := &staticSource{}
	src.set(Target{ID: "w", URL: srv.URL, Events: []string{"task.status"}})
	bus := start(t, src, Options{Attempts: 3})
	bus.Publish(events.Event{Type: events.TypeTaskStatus})
	for range 3 {
		next(t, ch)
	}
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 3 {
		t.Errorf("attempts = %d, want 3", n)
	}
}

func TestPicksUpChangedTargetsOnlyWhenTheVersionChanges(t *testing.T) {
	srv, ch, _ := recorder(t, ok)
	src := &staticSource{}
	bus := start(t, src, Options{})
	bus.Publish(events.Event{Type: events.TypeTaskStatus})
	bus.Publish(events.Event{Type: events.TypeTaskStatus})
	time.Sleep(50 * time.Millisecond)
	src.mu.Lock()
	loads := src.loads
	src.mu.Unlock()
	if loads != 1 {
		t.Errorf("loads = %d, want 1 for an unchanged version", loads)
	}
	src.set(Target{ID: "w", URL: srv.URL, Events: []string{"task.status"}})
	bus.Publish(events.Event{Type: events.TypeTaskStatus, TaskID: "late"})
	if r := next(t, ch); r.payload.TaskID != "late" {
		t.Errorf("payload = %+v", r.payload)
	}
}

func TestDeliverDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(target.Close)
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redir.Close)
	d := New(events.New(events.Options{}), &staticSource{}, Options{})
	err := d.Deliver(t.Context(), Target{ID: "w", URL: redir.URL}, events.Event{Type: events.TypeTaskStatus})
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("err = %v, want a permanent refusal for a 307", err)
	}
	if hits.Load() != 0 {
		t.Error("the redirect was followed")
	}
}

func TestDeliverReportsNetworkErrorsAsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	d := New(events.New(events.Options{}), &staticSource{}, Options{Timeout: time.Second})
	err := d.Deliver(t.Context(), Target{ID: "w", URL: url}, events.Event{Type: events.TypeTaskStatus})
	if err == nil || errors.Is(err, ErrPermanent) {
		t.Errorf("err = %v, want a retryable network error", err)
	}
}

func TestSignKnownVector(t *testing.T) {
	// RFC 4231 test case 2.
	got := Sign([]byte("Jefe"), []byte("what do ya want for nothing?"))
	if want := "sha256=5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"; got != want {
		t.Errorf("Sign = %s, want %s", got, want)
	}
}

func TestDeliveriesAreLoggedPerAttemptNewestFirst(t *testing.T) {
	// The endpoint fails twice with 503, then accepts.
	srv, ch, _ := recorder(t, func(n int) int {
		if n < 3 {
			return http.StatusServiceUnavailable
		}
		return http.StatusNoContent
	})
	src := &staticSource{}
	src.set(Target{ID: "w1", URL: srv.URL, Events: []string{"task.status"}})
	other := &staticSource{}
	other.set()
	d := New(nil, src, Options{Attempts: 4, Backoff: func(int) time.Duration { return time.Millisecond }})

	e := events.Event{ID: 7, Type: events.TypeTaskStatus, TaskID: "t1", Time: time.Now()}
	d.deliverWithRetry(t.Context(), job{target: Target{ID: "w1", URL: srv.URL}, event: e})
	for range 3 {
		next(t, ch)
	}

	got := d.Deliveries("w1")
	if len(got) != 3 {
		t.Fatalf("deliveries = %+v", got)
	}
	// Newest first: the success, then the two failures.
	if !got[0].OK || got[0].Attempt != 3 || got[0].Status != http.StatusNoContent || got[0].WillRetry || got[0].Error != "" {
		t.Errorf("last attempt = %+v", got[0])
	}
	for i, wantAttempt := range []int{2, 1} {
		a := got[i+1]
		if a.OK || a.Attempt != wantAttempt || a.Status != http.StatusServiceUnavailable || !a.WillRetry || a.Error == "" {
			t.Errorf("failed attempt %d = %+v", wantAttempt, a)
		}
	}
	if got[0].Event != "task.status" || got[0].EventID != 7 || got[0].ID <= got[2].ID || got[0].Time.IsZero() {
		t.Errorf("fields = %+v", got[0])
	}
	if len(d.Deliveries("nobody")) != 0 {
		t.Error("deliveries of an unknown webhook")
	}
}

func TestDeliveryLogIsBoundedAndShowsTransportErrorsAndRefusals(t *testing.T) {
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	t.Cleanup(refuse.Close)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	d := New(nil, &staticSource{}, Options{Attempts: 3, Backoff: func(int) time.Duration { return time.Millisecond }})
	e := events.Event{ID: 1, Type: events.TypeTaskStatus, Time: time.Now()}

	d.deliverWithRetry(t.Context(), job{target: Target{ID: "refused", URL: refuse.URL}, event: e})
	got := d.Deliveries("refused")
	if len(got) != 1 || got[0].OK || got[0].Status != http.StatusForbidden || got[0].WillRetry {
		t.Errorf("a refusal is final, one attempt: %+v", got)
	}
	d.deliverWithRetry(t.Context(), job{target: Target{ID: "dead", URL: deadURL}, event: e})
	got = d.Deliveries("dead")
	if len(got) != 3 || got[0].Status != 0 || got[0].WillRetry || !got[1].WillRetry || got[0].Error == "" {
		t.Errorf("transport failures: %+v", got)
	}

	for range DeliveryLogSize + 20 {
		_ = d.Deliver(t.Context(), Target{ID: "many", URL: refuse.URL}, e)
	}
	if n := len(d.Deliveries("many")); n != DeliveryLogSize {
		t.Errorf("kept %d deliveries, want %d", n, DeliveryLogSize)
	}
}
