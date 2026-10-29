package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/logging"
)

const tagLogs = "logs"

// GetLogsInput is the request of get-logs.
type GetLogsInput struct {
	Level  string    `query:"level" enum:"debug,info,warn,error" doc:"Only records of at least this level (default: all that were kept)."`
	Q      string    `query:"q" doc:"Only records whose message or attributes contain this text, ignoring case."`
	Since  time.Time `query:"since" doc:"Only records at or after this time (RFC 3339)."`
	Until  time.Time `query:"until" doc:"Only records at or before this time (RFC 3339)."`
	Before int64     `query:"before" minimum:"0" doc:"Only records older than this id: pass the first id of the previous answer to page backwards."`
	Limit  int       `query:"limit" minimum:"1" maximum:"1000" default:"200" doc:"How many records, counted from the newest."`
}

// LogList is the response of get-logs.
type LogList struct {
	Records []logging.Record `json:"records" doc:"The newest matching records, oldest first."`
	HasMore bool             `json:"hasMore" doc:"Older matching records exist: ask again with before set to the first id."`
	LastID  uint64           `json:"lastId" doc:"Id of the newest record the daemon has (0 when it has none), also when it does not match: start a stream after it."`
	BootID  string           `json:"bootId" doc:"Identifies this daemon process; record ids restart with it. Stream ids are <bootId>-<id>."`
}

// StreamLogsInput is the request of stream-logs.
type StreamLogsInput struct {
	Level       string `query:"level" enum:"debug,info,warn,error" doc:"Only records of at least this level."`
	Q           string `query:"q" doc:"Only records whose message or attributes contain this text, ignoring case."`
	LastEventID string `query:"lastEventId" doc:"Fallback for clients that cannot set the Last-Event-ID header; the header takes precedence."`
	LastEventH  string `header:"Last-Event-ID" doc:"<bootId>-<id> of the last record the client received: later records are sent first. An id of another daemon process is ignored (the stream starts at the next record)."`
}

const streamLogsDescription = "Streams log records as Server-Sent Events: event name log, data a LogRecord, id <bootId>-<id>. " +
	"With Last-Event-ID (or lastEventId) the records after it that are still kept are sent first; when some were already dropped a reset event with " +
	"reason gap comes first, and one with reason overflow when the client reads too slowly. get-logs answers the id to start from (lastId)."

func (s *Server) registerLogs() {
	Register(s, huma.Operation{
		OperationID: "get-logs",
		Method:      http.MethodGet,
		Path:        "/v1/logs",
		Summary:     "Read the daemon's log",
		Description: "The daemon keeps its most recent log records (the last 5000 since it started) in memory. This lists them, newest last, " +
			"filtered by level, text and time; older records are in the log file of the daemon's log directory.",
		Tags:   []string{tagLogs, tagSystem},
		Errors: []int{http.StatusServiceUnavailable},
	}, s.getLogs)

	reg := s.api.OpenAPI().Components.Schemas
	recordSchema := reg.Schema(reflect.TypeFor[logging.Record](), true, "LogRecord")
	resetSchema := reg.Schema(reflect.TypeFor[resetData](), true, "ResetEventData")
	Register(s, huma.Operation{
		OperationID: "stream-logs",
		Method:      http.MethodGet,
		Path:        "/v1/logs/stream",
		Summary:     "Stream the daemon's log (SSE)",
		Description: streamLogsDescription,
		Tags:        []string{tagLogs, tagSystem},
		Errors:      []int{http.StatusServiceUnavailable},
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Server-Sent Events stream of log records.",
				Content: map[string]*huma.MediaType{
					"text/event-stream": {
						Schema: &huma.Schema{
							Title:       "Server Sent Events",
							Description: "SSE messages whose data is a LogRecord or, for event name reset, a ResetEventData.",
							Type:        huma.TypeArray,
							Items:       &huma.Schema{OneOf: []*huma.Schema{recordSchema, resetSchema}},
						},
					},
				},
			},
		},
	}, s.streamLogs)
}

func logFilter(level, q string, since, until time.Time) (logging.Filter, error) {
	f := logging.Filter{Query: q, Since: since, Until: until}
	if level != "" {
		l, err := logging.ParseLevel(level)
		if err != nil {
			return f, newProblem(http.StatusUnprocessableEntity, err.Error(), &huma.ErrorDetail{Message: err.Error(), Location: "query.level"})
		}
		f.MinLevel = &l
	}
	return f, nil
}

func (s *Server) getLogs(_ context.Context, in *GetLogsInput) (*struct{ Body LogList }, error) {
	if s.logs == nil {
		return nil, errServicesUnavailable()
	}
	f, err := logFilter(in.Level, in.Q, in.Since, in.Until)
	if err != nil {
		return nil, err
	}
	recs, more := s.logs.Query(f, uint64(max(in.Before, 0)), in.Limit)
	if recs == nil {
		recs = []logging.Record{}
	}
	return &struct{ Body LogList }{Body: LogList{Records: recs, HasMore: more, LastID: s.logs.LastID(), BootID: s.bootID}}, nil
}

func (s *Server) streamLogs(_ context.Context, in *StreamLogsInput) (*huma.StreamResponse, error) {
	if s.logs == nil {
		return nil, errServicesUnavailable()
	}
	f, err := logFilter(in.Level, in.Q, time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}
	lastID := in.LastEventH
	if lastID == "" {
		lastID = in.LastEventID
	}
	return &huma.StreamResponse{
		Body: func(hctx huma.Context) { //nolint:contextcheck // streams under the request context, hctx.Context()
			hctx.SetHeader("Content-Type", "text/event-stream")
			hctx.SetHeader("Cache-Control", "no-cache")
			hctx.SetHeader("X-Accel-Buffering", "no")
			hctx.SetStatus(http.StatusOK)
			rw, ok := hctx.BodyWriter().(http.ResponseWriter)
			if !ok {
				s.log.ErrorContext(hctx.Context(), "logs: body writer is not an http.ResponseWriter")
				return
			}
			st := &sseStream{w: rw, rc: http.NewResponseController(rw), bootID: s.bootID}
			s.runLogStream(hctx.Context(), st, f, lastID)
			st.clearDeadline()
		},
	}, nil
}

func (s *Server) runLogStream(ctx context.Context, st *sseStream, f logging.Filter, lastID string) {
	var cursor uint64
	replay := false
	if lastID != "" {
		if boot, id, ok := parseEventID(lastID); ok && boot == s.bootID {
			replay, cursor = true, id
		}
	}
	sub := s.logs.Subscribe(ctx, f)
	defer sub.Unsubscribe()
	if err := st.flush(); err != nil {
		return
	}
	sent := s.logs.LastID() // live records at or below this are not sent twice
	if replay {
		recs, complete := s.logs.Since(cursor, f)
		if !complete {
			if err := st.writeReset("", ResetReasonGap); err != nil {
				return
			}
		}
		sent = cursor
		for _, r := range recs {
			if err := s.writeLog(st, r); err != nil {
				return
			}
			sent = r.ID
		}
		sent = max(sent, cursor)
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
		case r := <-sub.C():
			if d := sub.Dropped(); d > dropped {
				dropped = d
				if err := st.writeReset("", ResetReasonOverflow); err != nil {
					return
				}
			}
			if r.ID <= sent {
				continue
			}
			if err := s.writeLog(st, r); err != nil {
				return
			}
			sent = r.ID
		}
	}
}

func (s *Server) writeLog(st *sseStream, r logging.Record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return nil //nolint:nilerr // a record that cannot be encoded is skipped, not fatal
	}
	return st.write(formatEventID(st.bootID, r.ID), "log", data)
}
