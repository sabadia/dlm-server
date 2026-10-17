package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// sseHandler serves scripted SSE responses: the n-th connection gets
// scripts[n] (the last one repeats) and its request is recorded.
type sseHandler struct {
	scripts []func(w http.ResponseWriter, r *http.Request)

	mu   sync.Mutex
	reqs []*http.Request
}

func (h *sseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	n := len(h.reqs)
	h.reqs = append(h.reqs, r.Clone(r.Context()))
	h.mu.Unlock()
	h.scripts[min(n, len(h.scripts)-1)](w, r)
}

func (h *sseHandler) requests() []*http.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*http.Request(nil), h.reqs...)
}

func sseScript(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, body)
	}
}

func eventFrame(seq int, typ, task string) string {
	return fmt.Sprintf("id: b-%d\nevent: %s\ndata: {\"id\":%d,\"type\":%q,\"time\":\"2026-01-02T03:04:05Z\",\"taskId\":%q,\"data\":{\"n\":%d}}\n\n",
		seq, typ, seq, typ, task, seq)
}

func newEventsClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "tok", WithTimeout(time.Millisecond)) // the stream must ignore the request timeout
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEventsReconnectsWithLastEventID(t *testing.T) {
	h := &sseHandler{scripts: []func(http.ResponseWriter, *http.Request){
		sseScript(": ping\n\n" + eventFrame(1, "task.created", "t1") + eventFrame(2, "task.status", "t1")),
		func(w http.ResponseWriter, _ *http.Request) { // transient failure
			http.Error(w, "busy", http.StatusServiceUnavailable)
		},
		sseScript("event: reset\ndata: {\"reason\":\"gap\"}\n\n" + eventFrame(5, "task.deleted", "t1")),
	}}
	c := newEventsClient(t, h)

	var retries []error
	opts := EventsOptions{
		Types: []string{"task.created", " task.status ", ""}, TaskID: "t1", QueueID: "q1",
		LastEventID: "b-0", Reconnect: true, RetryDelay: time.Millisecond,
		OnRetry: func(err error, _ time.Duration) { retries = append(retries, err) },
	}
	var got []Event
	for ev, err := range c.Events(t.Context(), opts) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got = append(got, ev)
		if len(got) == 4 {
			break
		}
	}

	if len(got) != 4 {
		t.Fatalf("got %d events", len(got))
	}
	if got[0].ID != 1 || got[0].EventID != "b-1" || got[0].Type != "task.created" || got[0].TaskID != "t1" ||
		string(got[0].Data) != `{"n":1}` || !got[0].Time.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("event 0 = %+v", got[0])
	}
	if got[2].Type != EventTypeReset || got[2].ResetReason() != "gap" || got[2].ID != 0 {
		t.Errorf("event 2 = %+v, want reset gap", got[2])
	}
	if got[3].EventID != "b-5" || got[3].ResetReason() != "" {
		t.Errorf("event 3 = %+v", got[3])
	}

	reqs := h.requests()
	if len(reqs) != 3 {
		t.Fatalf("connections = %d, want 3", len(reqs))
	}
	wantLast := []string{"b-0", "b-2", "b-2"}
	for i, r := range reqs {
		if got := r.Header.Get("Last-Event-ID"); got != wantLast[i] {
			t.Errorf("connection %d Last-Event-ID = %q, want %q", i, got, wantLast[i])
		}
		if r.URL.Path != "/v1/events" || r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("connection %d request = %s %v", i, r.URL, r.Header)
		}
		q := r.URL.Query()
		if q.Get("types") != "task.created,task.status" || q.Get("taskId") != "t1" || q.Get("queueId") != "q1" {
			t.Errorf("connection %d query = %s", i, r.URL.RawQuery)
		}
	}
	if len(retries) != 2 || !errors.Is(retries[0], ErrStreamClosed) || !hasStatus(retries[1], http.StatusServiceUnavailable) {
		t.Errorf("retries = %v, want [stream closed, 503]", retries)
	}
}

func TestEventsTerminalErrors(t *testing.T) {
	tests := []struct {
		name      string
		reconnect bool
		script    func(http.ResponseWriter, *http.Request)
		check     func(error) bool
	}{
		{"unauthorized is terminal even with reconnect", true, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401}`))
		}, IsUnauthorized},
		{"wrong content type", true, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}, func(err error) bool { var pe *protocolError; return errors.As(err, &pe) }},
		{
			"malformed event data", true, sseScript("event: task.status\ndata: {not json\n\n"),
			func(err error) bool { var pe *protocolError; return errors.As(err, &pe) },
		},
		{"5xx without reconnect", false, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "x", http.StatusInternalServerError)
		}, func(err error) bool { return hasStatus(err, http.StatusInternalServerError) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &sseHandler{scripts: []func(http.ResponseWriter, *http.Request){tt.script}}
			c := newEventsClient(t, h)
			var errs []error
			for _, err := range c.Events(t.Context(), EventsOptions{Reconnect: tt.reconnect, RetryDelay: time.Millisecond}) {
				errs = append(errs, err)
			}
			if len(errs) != 1 || !tt.check(errs[0]) {
				t.Fatalf("yielded errors %v", errs)
			}
			if n := len(h.requests()); n != 1 {
				t.Fatalf("connections = %d, want 1 (no retry)", n)
			}
		})
	}
}

func TestEventsCleanEndWithoutReconnect(t *testing.T) {
	h := &sseHandler{scripts: []func(http.ResponseWriter, *http.Request){sseScript(eventFrame(1, "task.created", "a"))}}
	c := newEventsClient(t, h)
	n := 0
	for _, err := range c.Events(t.Context(), EventsOptions{}) {
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("events = %d, want 1", n)
	}
}

func TestEventsContextCancelEndsIteration(t *testing.T) {
	started := make(chan struct{})
	h := &sseHandler{scripts: []func(http.ResponseWriter, *http.Request){
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_ = http.NewResponseController(w).Flush()
			close(started)
			<-r.Context().Done()
		},
	}}
	c := newEventsClient(t, h)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan []error, 1)
	go func() {
		var errs []error
		for _, err := range c.Events(ctx, EventsOptions{Reconnect: true}) {
			errs = append(errs, err)
		}
		done <- errs
	}()
	<-started
	cancel()
	select {
	case errs := <-done:
		if len(errs) != 0 {
			t.Fatalf("yielded %v after cancel, want nothing", errs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("iteration did not end after ctx cancel")
	}
}

func TestEventsDaemonUnreachableRetries(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // nothing listens any more
	c, err := New(base, "tok")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var retries []error
	opts := EventsOptions{Reconnect: true, RetryDelay: time.Millisecond, OnRetry: func(err error, _ time.Duration) {
		retries = append(retries, err)
		if len(retries) == 2 {
			cancel()
		}
	}}
	for _, err := range c.Events(ctx, opts) {
		t.Fatalf("yielded %v, want retries until cancel", err)
	}
	if len(retries) != 2 || !errors.Is(retries[0], ErrDaemonUnreachable) {
		t.Fatalf("retries = %v, want ErrDaemonUnreachable", retries)
	}
}
