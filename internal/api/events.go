package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/events"
)

const (
	// DefaultEventsHeartbeat is the default interval between SSE keep-alive
	// comments on the event stream.
	DefaultEventsHeartbeat = 15 * time.Second

	// EventTypeReset is the SSE event type telling the client that it may
	// have missed events and must refetch the state it mirrors. Its data is
	// {"reason": "<ResetReason...>"}.
	EventTypeReset = "reset"
	// ResetReasonRestart: the Last-Event-ID belongs to another daemon process
	// (or is malformed), so the client's position is meaningless.
	ResetReasonRestart = "restart"
	// ResetReasonGap: events after the Last-Event-ID were evicted from the
	// replay buffer; the replay that follows is incomplete.
	ResetReasonGap = "gap"
	// ResetReasonOverflow: the client read too slowly and events were dropped
	// from its buffer.
	ResetReasonOverflow = "overflow"

	// eventsWriteTimeout bounds each write so a stalled client cannot pin a
	// handler forever.
	eventsWriteTimeout = 10 * time.Second
)

// newBootID returns a random token identifying this daemon process in SSE
// event IDs. It never contains '-', the separator of "<bootID>-<seq>".
func newBootID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("api: generate boot id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func validBootID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// formatEventID returns the SSE id of the event with bus sequence seq.
func formatEventID(bootID string, seq uint64) string {
	return bootID + "-" + strconv.FormatUint(seq, 10)
}

// parseEventID splits an SSE id into its boot ID and bus sequence.
func parseEventID(id string) (bootID string, seq uint64, ok bool) {
	boot, num, found := strings.Cut(id, "-")
	if !found || !validBootID(boot) {
		return "", 0, false
	}
	seq, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return boot, seq, true
}

// StreamEventsInput are the parameters of stream-events.
type StreamEventsInput struct {
	Types       []string `query:"types" doc:"Only stream events of these types (comma-separated), e.g. task.progress,task.status. Empty streams all types."`
	TaskID      string   `query:"taskId" doc:"Only stream events of this task."`
	QueueID     string   `query:"queueId" doc:"Only stream events of this queue."`
	LastEventID string   `query:"lastEventId" doc:"Fallback for clients that cannot set the Last-Event-ID header; the header takes precedence."`
	LastEventH  string   `header:"Last-Event-ID" doc:"ID of the last event the client received (\"<bootID>-<seq>\"). Missed events are replayed; if that is impossible a reset event is sent first."`
}

// resetData is the data of a reset event.
type resetData struct {
	Reason string `json:"reason" enum:"restart,gap,overflow" doc:"Why the client must refetch its state."`
}

const streamEventsDescription = `Streams events as Server-Sent Events (text/event-stream).

Each event is sent as:

    id: <bootID>-<seq>
    event: <type>            (e.g. task.progress, task.status)
    data: <JSON event>

The id combines a token identifying the daemon process with the bus sequence
number, so a reconnecting client's Last-Event-ID is only honoured by the same
daemon process. On reconnect with Last-Event-ID the missed events are replayed
before live events. A ` + "`reset`" + ` event (data ` + "`{\"reason\": \"restart\"|\"gap\"|\"overflow\"}`" + `)
tells the client it missed events and must refetch the state it mirrors:
"restart" when the Last-Event-ID belongs to another daemon process, "gap" when
the replay is incomplete, "overflow" when the client read too slowly.
A ": ping" comment is sent periodically to keep proxies from closing the
connection.`

func (s *Server) registerEvents() {
	reg := s.api.OpenAPI().Components.Schemas
	eventSchema := registerEventSchemas(reg)
	resetSchema := reg.Schema(reflect.TypeFor[resetData](), true, "ResetEventData")
	Register(s, huma.Operation{
		OperationID: "stream-events",
		Method:      http.MethodGet,
		Path:        "/v1/events",
		Summary:     "Stream events (SSE)",
		Description: streamEventsDescription,
		Tags:        []string{"events"},
		Errors:      []int{http.StatusServiceUnavailable},
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Server-Sent Events stream. The data of each event is a StreamEvent (one of the typed events, discriminated by type), except for reset events.",
				Content: map[string]*huma.MediaType{
					"text/event-stream": {
						Schema: &huma.Schema{
							Title:       "Server Sent Events",
							Description: "SSE messages whose data is a StreamEvent or, for event name reset, a ResetEventData.",
							Type:        huma.TypeArray,
							Items:       &huma.Schema{OneOf: []*huma.Schema{eventSchema, resetSchema}},
						},
					},
				},
			},
		},
	}, s.streamEvents)
}

func (s *Server) streamEvents(_ context.Context, in *StreamEventsInput) (*huma.StreamResponse, error) {
	if s.bus == nil {
		return nil, huma.Error503ServiceUnavailable("event stream is not available") //nolint:wrapcheck // huma status errors are returned as-is
	}
	filter := events.Filter{TaskID: in.TaskID, QueueID: in.QueueID}
	for _, t := range in.Types {
		if t = strings.TrimSpace(t); t != "" {
			filter.Types = append(filter.Types, events.Type(t))
		}
	}
	lastID := in.LastEventH
	if lastID == "" {
		lastID = in.LastEventID
	}
	return &huma.StreamResponse{
		Body: func(hctx huma.Context) { //nolint:contextcheck // streams under the request context, hctx.Context()
			hctx.SetHeader("Content-Type", "text/event-stream")
			hctx.SetHeader("Cache-Control", "no-cache")
			hctx.SetHeader("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
			hctx.SetStatus(http.StatusOK)
			w := hctx.BodyWriter()
			rw, ok := w.(http.ResponseWriter)
			if !ok {
				s.log.ErrorContext(hctx.Context(), "events: body writer is not an http.ResponseWriter")
				return
			}
			st := &sseStream{w: rw, rc: http.NewResponseController(rw), bootID: s.bootID}
			s.runStream(hctx.Context(), st, filter, lastID)
			st.clearDeadline()
		},
	}, nil
}

// runStream writes the replay and then live events until ctx is done, the
// bus closes, or a write fails.
func (s *Server) runStream(ctx context.Context, st *sseStream, filter events.Filter, lastID string) {
	// Decide the replay cursor before subscribing: for a stale/foreign id it
	// is the current bus position, so events published while we subscribe
	// are still replayed below.
	var (
		replay   bool
		cursor   uint64
		resetWhy string
		resetSeq uint64
	)
	if lastID != "" {
		boot, seq, ok := parseEventID(lastID)
		if ok && boot == s.bootID {
			replay, cursor = true, seq
		} else {
			resetWhy = ResetReasonRestart
			replay, cursor = true, s.bus.LastID()
			resetSeq = cursor
		}
	}

	sub := s.bus.Subscribe(ctx, filter)
	defer sub.Unsubscribe()

	// Commit the headers so the client sees the stream open immediately.
	if err := st.flush(); err != nil {
		return
	}

	var sent uint64 // highest seq written; live events at or below it are duplicates
	if replay {
		evs, complete := s.bus.Since(cursor, filter)
		if resetWhy == "" && !complete {
			resetWhy = ResetReasonGap
		}
		if resetWhy != "" {
			// A restart reset carries the new cursor as its id so a client
			// reconnecting before the next event is not reset again.
			id := ""
			if resetWhy == ResetReasonRestart {
				id = formatEventID(s.bootID, resetSeq)
			}
			if err := st.writeReset(id, resetWhy); err != nil {
				return
			}
		}
		sent = cursor
		for _, e := range evs {
			if err := s.writeEvent(ctx, st, e); err != nil {
				return
			}
			sent = e.ID
		}
	}

	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()
	var dropped uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if err := st.writeComment("ping"); err != nil {
				return
			}
		case e, ok := <-sub.C():
			if !ok {
				return // bus closed
			}
			if d := sub.Dropped(); d > dropped {
				dropped = d
				if err := st.writeReset("", ResetReasonOverflow); err != nil {
					return
				}
			}
			if e.ID <= sent {
				continue // already sent by the replay
			}
			if err := s.writeEvent(ctx, st, e); err != nil {
				return
			}
			sent = e.ID
		}
	}
}

func (s *Server) writeEvent(ctx context.Context, st *sseStream, e events.Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		// A publisher put unencodable data on the bus; skip the event rather
		// than kill the stream.
		s.log.WarnContext(ctx, "events: encode event", "event_type", string(e.Type), "event_id", e.ID, "error", err)
		return nil
	}
	return st.write(formatEventID(st.bootID, e.ID), string(e.Type), data)
}

// sseStream writes SSE messages to one response.
type sseStream struct {
	w      http.ResponseWriter
	rc     *http.ResponseController
	bootID string
	buf    bytes.Buffer
}

func (st *sseStream) writeReset(id, reason string) error {
	data, err := json.Marshal(resetData{Reason: reason})
	if err != nil {
		return fmt.Errorf("api: encode reset event: %w", err)
	}
	return st.write(id, EventTypeReset, data)
}

// write sends one event. data must not contain newlines (compact JSON).
func (st *sseStream) write(id, event string, data []byte) error {
	st.buf.Reset()
	if id != "" {
		st.buf.WriteString("id: " + id + "\n")
	}
	st.buf.WriteString("event: " + event + "\n")
	st.buf.WriteString("data: ")
	st.buf.Write(data)
	st.buf.WriteString("\n\n")
	return st.send()
}

func (st *sseStream) writeComment(text string) error {
	st.buf.Reset()
	st.buf.WriteString(": " + text + "\n\n")
	return st.send()
}

func (st *sseStream) send() error {
	st.setDeadline(time.Now().Add(eventsWriteTimeout))
	if _, err := st.w.Write(st.buf.Bytes()); err != nil {
		return fmt.Errorf("api: write event: %w", err)
	}
	return st.flush()
}

func (st *sseStream) flush() error {
	if err := st.rc.Flush(); err != nil {
		return fmt.Errorf("api: flush event stream: %w", err)
	}
	return nil
}

func (st *sseStream) setDeadline(t time.Time) {
	// Ignored: http.ErrNotSupported (e.g. test recorders) only loses the
	// stall protection, and a broken connection surfaces on the next write.
	_ = st.rc.SetWriteDeadline(t)
}

// clearDeadline removes the write deadline so a kept-alive connection can be
// reused after the stream ends.
func (st *sseStream) clearDeadline() { st.setDeadline(time.Time{}) }
