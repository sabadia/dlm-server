package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/logging"
)

func newLogsServer(t *testing.T, ring *logging.Ring) *Server {
	t.Helper()
	cfg := config.Default()
	s, err := New(&cfg, Deps{Token: testToken, Logs: ring, BootID: testBootID, EventsHeartbeat: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func addLogs(r *logging.Ring, n int) {
	for i := 1; i <= n; i++ {
		lvl := "info"
		if i%4 == 0 {
			lvl = "error"
		}
		r.Add(logging.Record{Time: time.Now(), Level: lvl, Message: fmt.Sprintf("message %d", i), Attrs: map[string]any{"n": i}})
	}
}

func TestGetLogs(t *testing.T) {
	ring := logging.NewRing(100)
	addLogs(ring, 10)
	s := newLogsServer(t, ring)

	var l LogList
	callServer(t, s, http.MethodGet, "/v1/logs?limit=3", "", "", http.StatusOK, &l)
	if len(l.Records) != 3 || l.Records[0].ID != 8 || l.Records[2].ID != 10 || !l.HasMore || l.LastID != 10 || l.BootID != testBootID {
		t.Errorf("newest three = %+v", l)
	}
	callServer(t, s, http.MethodGet, "/v1/logs?limit=3&before=8", "", "", http.StatusOK, &l)
	if len(l.Records) != 3 || l.Records[0].ID != 5 || l.Records[2].ID != 7 || !l.HasMore {
		t.Errorf("page before 8 = %+v", l)
	}
	callServer(t, s, http.MethodGet, "/v1/logs?level=error", "", "", http.StatusOK, &l)
	if len(l.Records) != 2 || l.Records[0].ID != 4 || l.Records[1].ID != 8 || l.HasMore {
		t.Errorf("errors = %+v", l)
	}
	callServer(t, s, http.MethodGet, "/v1/logs?q="+url.QueryEscape("MESSAGE 7"), "", "", http.StatusOK, &l)
	if len(l.Records) != 1 || l.Records[0].ID != 7 {
		t.Errorf("search = %+v", l)
	}
	callServer(t, s, http.MethodGet, "/v1/logs?q=nothing-like-this", "", "", http.StatusOK, &l)
	if l.Records == nil || len(l.Records) != 0 {
		t.Errorf("no match must be []: %+v", l.Records)
	}
	callServer(t, s, http.MethodGet, "/v1/logs?level=loud", "", "", http.StatusUnprocessableEntity, nil)

	off := newLogsServer(t, nil)
	callServer(t, off, http.MethodGet, "/v1/logs", "", "", http.StatusServiceUnavailable, nil)
	callServer(t, off, http.MethodGet, "/v1/logs/stream", "", "", http.StatusServiceUnavailable, nil)
}

func logFrame(t *testing.T, f sseFrame) logging.Record {
	t.Helper()
	if f.event != "log" {
		t.Fatalf("frame = %+v, want a log event", f)
	}
	var r logging.Record
	if err := json.Unmarshal([]byte(f.data), &r); err != nil {
		t.Fatal(err)
	}
	if f.id != fmt.Sprintf("%s-%d", testBootID, r.ID) {
		t.Fatalf("frame id %q does not match record %d", f.id, r.ID)
	}
	return r
}

func TestStreamLogs(t *testing.T) {
	ring := logging.NewRing(5)
	addLogs(ring, 8) // records 4..8 are kept
	ts := startHTTP(t, newLogsServer(t, ring).Handler())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// A stream without an id starts at the next record.
	st := openStreamAt(ctx, t, ts.URL, "/v1/logs/stream", "?level=error", nil)
	ring.Add(logging.Record{Level: "info", Message: "quiet"})
	ring.Add(logging.Record{Level: "error", Message: "loud", Time: time.Now()})
	if r := logFrame(t, st.next(t)); r.Message != "loud" || r.ID != 10 {
		t.Errorf("first live record = %+v", r)
	}

	// Resuming replays what came after the id.
	st2 := openStreamAt(ctx, t, ts.URL, "/v1/logs/stream", "", map[string]string{"Last-Event-ID": testBootID + "-7"})
	for _, want := range []uint64{8, 9, 10} {
		if r := logFrame(t, st2.next(t)); r.ID != want {
			t.Errorf("replayed %d, want %d", r.ID, want)
		}
	}
	ring.Add(logging.Record{Level: "info", Message: "live", Time: time.Now()})
	if r := logFrame(t, st2.next(t)); r.ID != 11 {
		t.Errorf("live after the replay = %+v", r)
	}

	// An id whose records were dropped says so first.
	st3 := openStreamAt(ctx, t, ts.URL, "/v1/logs/stream", "", map[string]string{"Last-Event-ID": testBootID + "-1"})
	if f := st3.next(t); f.event != EventTypeReset || f.data != `{"reason":"gap"}` {
		t.Errorf("first frame = %+v, want a gap reset", f)
	}
	if r := logFrame(t, st3.next(t)); r.ID != 7 {
		t.Errorf("then the oldest kept record: %+v", r)
	}

	// An id of another daemon process is ignored: nothing is replayed.
	st4 := openStreamAt(ctx, t, ts.URL, "/v1/logs/stream", "?lastEventId=other-3", nil)
	ring.Add(logging.Record{Level: "info", Message: "fresh", Time: time.Now()})
	if r := logFrame(t, st4.next(t)); r.Message != "fresh" {
		t.Errorf("foreign id replayed %+v", r)
	}
}
