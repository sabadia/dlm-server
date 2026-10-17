package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// EventTypeReset is the type of the synthetic event the daemon sends when
// the client may have missed events (daemon restart, replay gap, slow
// client). On receiving it a client must refetch the state it mirrors;
// [Event.ResetReason] tells why.
const EventTypeReset = "reset"

// Default reconnect delays of [Client.Events].
const (
	DefaultRetryDelay    = time.Second
	DefaultMaxRetryDelay = 30 * time.Second
)

// Event is one event received from the stream-events operation. It mirrors
// the daemon's event JSON, plus the SSE event ID.
type Event struct {
	// ID is the daemon's per-process event sequence number (0 for reset
	// events).
	ID uint64 `json:"id,omitempty"`
	// EventID is the SSE event ID ("<bootID>-<seq>"), usable as
	// EventsOptions.LastEventID to resume the stream.
	EventID string    `json:"eventId,omitempty"`
	Type    string    `json:"type"`
	Time    time.Time `json:"time,omitzero"`
	TaskID  string    `json:"taskId,omitempty"`
	QueueID string    `json:"queueId,omitempty"`
	// Data is the type-specific payload, e.g. {"downloaded":1,"size":2,
	// "speed":3} for task.progress or {"from":"queued","to":"running"} for
	// task.status; {"reason":"gap"} for reset events.
	Data json.RawMessage `json:"data,omitempty"`
}

// ResetReason returns the reason of a reset event ("restart", "gap",
// "overflow"), or "" for other events.
func (e Event) ResetReason() string {
	if e.Type != EventTypeReset {
		return ""
	}
	var d struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(e.Data, &d); err != nil {
		return ""
	}
	return d.Reason
}

// EventsOptions configures [Client.Events].
type EventsOptions struct {
	// Types, TaskID and QueueID filter the stream (empty = everything).
	Types   []string
	TaskID  string
	QueueID string
	// LastEventID resumes after this SSE event ID (Event.EventID).
	LastEventID string
	// Reconnect makes the stream survive disconnects: after a transient
	// failure (daemon unreachable, connection lost, 5xx, stream closed by
	// the daemon) it reconnects with exponential backoff, resuming after
	// the last received event ID.
	Reconnect bool
	// RetryDelay is the first reconnect delay (default DefaultRetryDelay, or
	// the daemon's SSE retry field); it doubles after every failed attempt
	// up to MaxRetryDelay (default DefaultMaxRetryDelay).
	RetryDelay    time.Duration
	MaxRetryDelay time.Duration
	// OnRetry, if set, is called before waiting to reconnect, with the error
	// that ended the previous attempt and the delay before the next one.
	OnRetry func(err error, delay time.Duration)
}

// ErrStreamClosed is passed to EventsOptions.OnRetry when the daemon ended
// the stream cleanly (e.g. it is shutting down).
var ErrStreamClosed = errors.New("client: event stream closed by the daemon")

// protocolError marks malformed responses; reconnecting would not help.
type protocolError struct{ err error }

func (e *protocolError) Error() string { return e.err.Error() }
func (e *protocolError) Unwrap() error { return e.err }

// Events calls stream-events (GET /v1/events) and returns an iterator over
// the received events. It is not subject to the client's request timeout.
//
// Iteration ends when the caller stops ranging, when ctx is done (no error
// is yielded in that case), or after a terminal error, which is yielded once
// as (Event{}, err): without Reconnect any failure or the end of the stream
// is terminal (a clean end yields no error); with Reconnect only
// non-transient failures are (4xx responses such as an invalid token,
// malformed responses). The iterator starts no goroutines; each range over
// it opens a new connection.
func (c *Client) Events(ctx context.Context, opts EventsOptions) iter.Seq2[Event, error] {
	initial := opts.RetryDelay
	if initial <= 0 {
		initial = DefaultRetryDelay
	}
	maxDelay := opts.MaxRetryDelay
	if maxDelay <= 0 {
		maxDelay = DefaultMaxRetryDelay
	}
	return func(yield func(Event, error) bool) {
		st := &streamState{lastID: opts.LastEventID}
		delay := initial
		for {
			connected, stopped, err := c.streamOnce(ctx, opts, st, yield)
			if stopped || ctx.Err() != nil {
				return
			}
			if !opts.Reconnect {
				if err != nil {
					yield(Event{}, err)
				}
				return
			}
			if err == nil {
				err = ErrStreamClosed
			} else if !retryable(err) {
				yield(Event{}, err)
				return
			}
			if connected {
				delay = initial
				if st.retry > 0 {
					delay = min(st.retry, maxDelay)
				}
			}
			if opts.OnRetry != nil {
				opts.OnRetry(err, delay)
			}
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			delay = min(delay*2, maxDelay)
		}
	}
}

// streamState carries the resume position across reconnects.
type streamState struct {
	lastID string
	retry  time.Duration
}

// streamOnce runs one connection. connected reports whether the connection
// was established; stopped that the consumer stopped the iteration. A nil
// err means the daemon closed the stream cleanly.
func (c *Client) streamOnce(ctx context.Context, opts EventsOptions, st *streamState, yield func(Event, error) bool) (connected, stopped bool, err error) {
	path := "/v1/events" + eventsQuery(opts)
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return false, false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if st.lastID != "" {
		req.Header.Set("Last-Event-ID", st.lastID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, false, c.transportError(http.MethodGet, "/v1/events", err)
	}
	defer func() {
		// The stream is abandoned, not drained: closing ends the connection.
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, false, decodeError(resp)
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
		return false, false, &protocolError{fmt.Errorf("client: stream-events: unexpected content type %q", resp.Header.Get("Content-Type"))}
	}

	p := newSSEParser(resp.Body)
	for {
		m, err := p.next()
		if p.lastID != "" {
			st.lastID = p.lastID
		}
		if p.retry > 0 {
			st.retry = p.retry
		}
		if errors.Is(err, io.EOF) {
			return true, false, nil
		}
		if err != nil {
			return true, false, err
		}
		ev, err := decodeEvent(m)
		if err != nil {
			return true, false, err
		}
		if !yield(ev, nil) {
			return true, true, nil
		}
	}
}

func eventsQuery(opts EventsOptions) string {
	q := url.Values{}
	var types []string
	for _, t := range opts.Types {
		if t = strings.TrimSpace(t); t != "" {
			types = append(types, t)
		}
	}
	if len(types) > 0 {
		q.Set("types", strings.Join(types, ","))
	}
	if opts.TaskID != "" {
		q.Set("taskId", opts.TaskID)
	}
	if opts.QueueID != "" {
		q.Set("queueId", opts.QueueID)
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

func decodeEvent(m sseMessage) (Event, error) {
	if m.Event == EventTypeReset {
		if !json.Valid([]byte(m.Data)) {
			return Event{}, &protocolError{fmt.Errorf("client: stream-events: invalid reset data %q", m.Data)}
		}
		return Event{EventID: m.ID, Type: EventTypeReset, Data: json.RawMessage(m.Data)}, nil
	}
	var ev Event
	if err := json.Unmarshal([]byte(m.Data), &ev); err != nil {
		return Event{}, &protocolError{fmt.Errorf("client: stream-events: decode %s event: %w", m.Event, err)}
	}
	ev.EventID = m.ID
	if ev.Type == "" {
		ev.Type = m.Event
	}
	return ev, nil
}

// retryable reports whether a reconnect may succeed after err.
func retryable(err error) bool {
	var pe *protocolError
	if errors.As(err, &pe) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= http.StatusInternalServerError || apiErr.Status == http.StatusTooManyRequests
	}
	return true // transport errors, including an unreachable daemon
}
