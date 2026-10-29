package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/events"
)

const (
	testBootID   = "boot1"
	frameTimeout = 5 * time.Second
)

// sseFrame is one raw SSE message as seen on the wire.
type sseFrame struct {
	id, event, data string
	hasID           bool
	comment         string
}

func newEventsServer(t *testing.T, bus *events.Bus, heartbeat time.Duration) *Server {
	t.Helper()
	cfg := config.Default()
	s, err := New(&cfg, Deps{Token: testToken, Bus: bus, BootID: testBootID, EventsHeartbeat: heartbeat})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// startHTTP serves h; open streams are cut when the test ends so Close does
// not wait for them.
func startHTTP(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	return ts
}

func newBus(t *testing.T, opts events.Options) *events.Bus {
	t.Helper()
	b := events.New(opts)
	t.Cleanup(b.Close)
	return b
}

// stream is an open SSE connection whose frames are read by a goroutine.
type stream struct {
	resp   *http.Response
	frames chan sseFrame
	done   chan struct{} // closed when the reader goroutine ends (stream closed)
}

// openStream connects to /v1/events. When it returns, the server has
// subscribed to the bus (headers are flushed after subscribing).
func openStream(ctx context.Context, t *testing.T, base, query string, header map[string]string) *stream {
	t.Helper()
	return openStreamAt(ctx, t, base, "/v1/events", query, header)
}

// openStreamAt connects to the SSE endpoint at path.
func openStreamAt(ctx context.Context, t *testing.T, base, path, query string, header map[string]string) *stream {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path+query, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/events: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("status = %d; body: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	st := &stream{resp: resp, frames: make(chan sseFrame, 1024), done: make(chan struct{})}
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(st.done)
		readFrames(resp.Body, st.frames)
	})
	t.Cleanup(func() {
		_ = resp.Body.Close()
		wg.Wait()
	})
	return st
}

func readFrames(r io.Reader, out chan<- sseFrame) {
	sc := bufio.NewScanner(r)
	var f sseFrame
	empty := true
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if !empty {
				out <- f
			}
			f, empty = sseFrame{}, true
			continue
		}
		empty = false
		field, value, _ := strings.Cut(line, ": ")
		switch field {
		case "":
			f.comment = value
		case "id":
			f.id, f.hasID = value, true
		case "event":
			f.event = value
		case "data":
			f.data = value
		}
	}
}

// next returns the next non-comment frame.
func (st *stream) next(t *testing.T) sseFrame {
	t.Helper()
	for {
		f := st.nextAny(t)
		if f.comment == "" {
			return f
		}
	}
}

func (st *stream) nextAny(t *testing.T) sseFrame {
	t.Helper()
	select {
	case f := <-st.frames:
		return f
	case <-st.done:
		select {
		case f := <-st.frames:
			return f
		default:
		}
		t.Fatal("stream closed while waiting for a frame")
	case <-time.After(frameTimeout):
		t.Fatal("timed out waiting for a frame")
	}
	return sseFrame{}
}

func decodeFrame(t *testing.T, f sseFrame) events.Event {
	t.Helper()
	var e events.Event
	if err := json.Unmarshal([]byte(f.data), &e); err != nil {
		t.Fatalf("decode data %q: %v", f.data, err)
	}
	if f.id != testBootID+"-"+strconv.FormatUint(e.ID, 10) {
		t.Fatalf("frame id %q does not match event id %d", f.id, e.ID)
	}
	if f.event != string(e.Type) {
		t.Fatalf("frame event %q != data type %q", f.event, e.Type)
	}
	return e
}

func assertReset(t *testing.T, f sseFrame, reason string) {
	t.Helper()
	if f.event != EventTypeReset {
		t.Fatalf("frame = %+v, want reset event", f)
	}
	var d struct{ Reason string }
	if err := json.Unmarshal([]byte(f.data), &d); err != nil || d.Reason != reason {
		t.Fatalf("reset data = %q, want reason %q", f.data, reason)
	}
}

func publish(b *events.Bus, typ events.Type, task string) events.Event {
	return b.Publish(events.Event{Type: typ, TaskID: task})
}

func TestStreamEventsLive(t *testing.T) {
	bus := newBus(t, events.Options{})
	ts := startHTTP(t, newEventsServer(t, bus, time.Hour).Handler())

	st := openStream(t.Context(), t, ts.URL, "", nil)
	want := bus.Publish(events.Event{Type: events.TypeTaskStatus, TaskID: "t1", QueueID: "q1", Data: map[string]string{"from": "queued", "to": "running"}})

	f := st.next(t)
	if f.id != testBootID+"-1" || f.event != "task.status" {
		t.Fatalf("frame = %+v, want id %s-1, event task.status", f, testBootID)
	}
	got := decodeFrame(t, f)
	if got.ID != want.ID || got.TaskID != "t1" || got.QueueID != "q1" || !got.Time.Equal(want.Time) {
		t.Fatalf("event = %+v, want %+v", got, want)
	}
	if !strings.Contains(f.data, `"data":{"from":"queued","to":"running"}`) {
		t.Fatalf("data = %s", f.data)
	}
}

func TestStreamEventsFilters(t *testing.T) {
	bus := newBus(t, events.Options{})
	ts := startHTTP(t, newEventsServer(t, bus, time.Hour).Handler())

	st := openStream(t.Context(), t, ts.URL, "?types=task.status,task.deleted&taskId=a", nil)
	publish(bus, events.TypeTaskStatus, "b")  // wrong task
	publish(bus, events.TypeTaskCreated, "a") // wrong type
	e3 := publish(bus, events.TypeTaskStatus, "a")
	bus.Publish(events.Event{Type: events.TypeQueueUpdated, QueueID: "q"})
	e5 := publish(bus, events.TypeTaskDeleted, "a")

	for _, want := range []uint64{e3.ID, e5.ID} {
		if got := decodeFrame(t, st.next(t)); got.ID != want {
			t.Fatalf("got event %d (%s %s), want %d", got.ID, got.Type, got.TaskID, want)
		}
	}

	st2 := openStream(t.Context(), t, ts.URL, "?queueId=q2", nil)
	publish(bus, events.TypeTaskStatus, "a")
	want := bus.Publish(events.Event{Type: events.TypeQueueUpdated, QueueID: "q2"})
	if got := decodeFrame(t, st2.next(t)); got.ID != want.ID {
		t.Fatalf("queue filter: got %d, want %d", got.ID, want.ID)
	}
}

func TestStreamEventsHeartbeat(t *testing.T) {
	bus := newBus(t, events.Options{})
	ts := startHTTP(t, newEventsServer(t, bus, 10*time.Millisecond).Handler())

	st := openStream(t.Context(), t, ts.URL, "", nil)
	if f := st.nextAny(t); f.comment != "ping" {
		t.Fatalf("first frame = %+v, want ping comment", f)
	}
}

func TestStreamEventsReplaySameBootNoLossNoDuplicates(t *testing.T) {
	bus := newBus(t, events.Options{})
	ts := startHTTP(t, newEventsServer(t, bus, time.Hour).Handler())

	for range 3 {
		publish(bus, events.TypeTaskUpdated, "a")
	}

	// Publish continuously while connecting so events land before, during
	// and after the subscription/replay handover.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			publish(bus, events.TypeTaskUpdated, "a")
			if i%10 == 0 {
				time.Sleep(time.Millisecond) // let the handler interleave
			}
			if i > 150 {
				return
			}
		}
	})
	st := openStream(t.Context(), t, ts.URL, "", map[string]string{"Last-Event-ID": testBootID + "-1"})
	wg.Wait()
	close(stop)
	sentinel := publish(bus, events.TypeTaskDeleted, "a")

	next := uint64(2)
	for {
		e := decodeFrame(t, st.next(t))
		if e.ID != next {
			t.Fatalf("got event %d, want %d (lost or duplicated)", e.ID, next)
		}
		if e.ID == sentinel.ID {
			break
		}
		next++
	}
}

func TestStreamEventsLastEventIDQueryFallback(t *testing.T) {
	bus := newBus(t, events.Options{})
	ts := startHTTP(t, newEventsServer(t, bus, time.Hour).Handler())
	for range 3 {
		publish(bus, events.TypeTaskUpdated, "a")
	}

	st := openStream(t.Context(), t, ts.URL, "?lastEventId="+testBootID+"-2", nil)
	if e := decodeFrame(t, st.next(t)); e.ID != 3 {
		t.Fatalf("query fallback: got %d, want 3", e.ID)
	}
	// The header wins over the query parameter.
	st2 := openStream(t.Context(), t, ts.URL, "?lastEventId="+testBootID+"-0", map[string]string{"Last-Event-ID": testBootID + "-2"})
	if e := decodeFrame(t, st2.next(t)); e.ID != 3 {
		t.Fatalf("header precedence: got %d, want 3", e.ID)
	}
}

func TestStreamEventsResetRestart(t *testing.T) {
	for _, lastID := range []string{"otherboot-5", "garbage", testBootID + "-x", "-3"} {
		t.Run(lastID, func(t *testing.T) {
			bus := newBus(t, events.Options{})
			ts := startHTTP(t, newEventsServer(t, bus, time.Hour).Handler())
			publish(bus, events.TypeTaskUpdated, "a")
			publish(bus, events.TypeTaskUpdated, "a")

			st := openStream(t.Context(), t, ts.URL, "", map[string]string{"Last-Event-ID": lastID})
			f := st.next(t)
			assertReset(t, f, ResetReasonRestart)
			if f.id != testBootID+"-2" {
				t.Fatalf("restart reset id = %q, want %s-2 (current position)", f.id, testBootID)
			}
			want := publish(bus, events.TypeTaskStatus, "a")
			if e := decodeFrame(t, st.next(t)); e.ID != want.ID {
				t.Fatalf("after reset got %d, want %d (no replay of old events)", e.ID, want.ID)
			}
		})
	}
}

func TestStreamEventsResetGap(t *testing.T) {
	bus := newBus(t, events.Options{ReplaySize: 4})
	ts := startHTTP(t, newEventsServer(t, bus, time.Hour).Handler())
	for range 10 { // ring keeps 7..10
		publish(bus, events.TypeTaskUpdated, "a")
	}

	st := openStream(t.Context(), t, ts.URL, "", map[string]string{"Last-Event-ID": testBootID + "-2"})
	f := st.next(t)
	assertReset(t, f, ResetReasonGap)
	if f.hasID {
		t.Fatalf("gap reset carries id %q", f.id)
	}
	for want := uint64(7); want <= 10; want++ {
		if e := decodeFrame(t, st.next(t)); e.ID != want {
			t.Fatalf("got %d, want %d", e.ID, want)
		}
	}
}

func TestStreamEventsNoLastEventIDNoReplay(t *testing.T) {
	bus := newBus(t, events.Options{})
	ts := startHTTP(t, newEventsServer(t, bus, time.Hour).Handler())
	publish(bus, events.TypeTaskUpdated, "a")

	st := openStream(t.Context(), t, ts.URL, "", nil)
	want := publish(bus, events.TypeTaskStatus, "a")
	if e := decodeFrame(t, st.next(t)); e.ID != want.ID {
		t.Fatalf("got %d, want %d", e.ID, want.ID)
	}
}

func TestStreamEventsRequiresAuth(t *testing.T) {
	s := newEventsServer(t, newBus(t, events.Options{}), time.Hour)
	w := do(t, s, request{method: http.MethodGet, path: "/v1/events"})
	assertProblem(t, w, http.StatusUnauthorized)
}

func TestStreamEventsNoBus(t *testing.T) {
	s := newEventsServer(t, nil, time.Hour)
	w := do(t, s, request{method: http.MethodGet, path: "/v1/events", headers: bearer(testToken)})
	assertProblem(t, w, http.StatusServiceUnavailable)
}

func TestStreamEventsEndsWhenBusCloses(t *testing.T) {
	bus := events.New(events.Options{})
	ts := startHTTP(t, newEventsServer(t, bus, time.Hour).Handler())
	st := openStream(t.Context(), t, ts.URL, "", nil)
	bus.Close()
	select {
	case <-st.done:
	case <-time.After(frameTimeout):
		t.Fatal("stream did not end after the bus closed")
	}
}

func TestStreamEventsEndsOnClientDisconnect(t *testing.T) {
	bus := newBus(t, events.Options{})
	ts := startHTTP(t, newEventsServer(t, bus, time.Hour).Handler())
	ctx, cancel := context.WithCancel(t.Context())
	openStream(ctx, t, ts.URL, "", nil)
	cancel()
	// The handler must return; httptest.Server.Close blocks until it does.
	closed := make(chan struct{})
	go func() { ts.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(frameTimeout):
		t.Fatal("handler did not return after the client disconnected")
	}
}

func TestStreamEventsEndsOnServerShutdown(t *testing.T) {
	bus := newBus(t, events.Options{})
	s := newEventsServer(t, bus, time.Hour)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, ln) }()

	st := openStream(t.Context(), t, "http://"+ln.Addr().String(), "", nil)
	start := time.Now()
	cancel()
	select {
	case <-st.done:
	case <-time.After(frameTimeout):
		t.Fatal("stream did not end on shutdown")
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve = %v, want nil (graceful)", err)
		}
	case <-time.After(frameTimeout):
		t.Fatal("Serve did not return")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("shutdown took %s; the stream should end promptly", d)
	}
}

// blockingWriter is an http.ResponseWriter whose Write blocks until
// released, simulating a slow client.
type blockingWriter struct {
	header    http.Header
	entered   chan struct{}
	release   chan struct{}
	flushed   chan struct{} // closed on the first Flush (headers committed)
	flushOnce sync.Once
	mu        sync.Mutex
	body      strings.Builder
}

func (w *blockingWriter) Header() http.Header { return w.header }
func (w *blockingWriter) WriteHeader(int)     {}
func (w *blockingWriter) Flush()              { w.flushOnce.Do(func() { close(w.flushed) }) }
func (w *blockingWriter) Write(p []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	case <-w.release: // closed: the test no longer paces writes
	}
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}

func TestStreamEventsResetOverflow(t *testing.T) {
	bus := newBus(t, events.Options{SubscriberBuffer: 2})
	s := newEventsServer(t, bus, time.Hour)
	w := &blockingWriter{header: http.Header{}, entered: make(chan struct{}), release: make(chan struct{}), flushed: make(chan struct{})}
	st := &sseStream{w: w, rc: http.NewResponseController(w), bootID: testBootID}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var wg sync.WaitGroup

	// The handler subscribes before its first flush; wait until it is
	// blocked writing event 1, then overflow its 2-event buffer.
	wg.Go(func() { s.runStream(ctx, st, events.Filter{}, "") })
	<-w.flushed
	publish(bus, events.TypeTaskUpdated, "a")
	<-w.entered // writing event 1
	for range 5 {
		publish(bus, events.TypeTaskUpdated, "a") // 2..6; 2..4 are dropped
	}
	w.release <- struct{}{}
	<-w.entered // next write: the overflow reset
	w.release <- struct{}{}
	<-w.entered // event 5
	cancel()
	close(w.release)

	wg.Wait()
	w.mu.Lock()
	body := w.body.String()
	w.mu.Unlock()
	frames := make(chan sseFrame, 16)
	readFrames(strings.NewReader(body), frames)
	close(frames)
	var got []sseFrame
	for f := range frames {
		got = append(got, f)
	}
	if len(got) < 2 {
		t.Fatalf("frames = %+v, want event 1 then a reset", got)
	}
	if e := decodeFrame(t, got[0]); e.ID != 1 {
		t.Fatalf("first event = %d, want 1", e.ID)
	}
	assertReset(t, got[1], ResetReasonOverflow)
}
