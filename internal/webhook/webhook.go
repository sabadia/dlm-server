// Package webhook delivers events to the HTTP endpoints users registered.
//
// Each delivery is a JSON POST signed with HMAC-SHA256 when the endpoint has a
// secret. Deliveries run on a bounded worker pool and are retried with backoff
// on network errors, 5xx, 408 and 429; other 4xx answers are permanent.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/sabadia/dlm-server/internal/events"
)

// Headers sent with every delivery.
const (
	HeaderEvent     = "X-DLM-Event"
	HeaderDelivery  = "X-DLM-Delivery"
	HeaderSignature = "X-DLM-Signature"
)

// Target is a webhook as the dispatcher needs it.
type Target struct {
	ID     string
	URL    string
	Events []string
	// Secret signs the body; empty means unsigned.
	Secret []byte
}

// Source provides the enabled targets. Version changes whenever they may have
// changed, so the dispatcher reloads only then.
type Source interface {
	Targets(ctx context.Context) ([]Target, error)
	Version() uint64
}

// Payload is the JSON body of a delivery.
type Payload struct {
	ID      uint64      `json:"id"`
	Type    events.Type `json:"type"`
	Time    time.Time   `json:"time"`
	TaskID  string      `json:"taskId,omitempty"`
	QueueID string      `json:"queueId,omitempty"`
	Data    any         `json:"data,omitempty"`
}

// Options configure a Dispatcher.
type Options struct {
	Logger *slog.Logger
	// Client sends the requests; the default has Timeout and follows no
	// redirects.
	Client *http.Client
	// Timeout bounds one attempt; default 10s.
	Timeout time.Duration
	// Workers is the number of concurrent deliveries; default 4.
	Workers int
	// Queue is how many deliveries may wait; the oldest are never dropped
	// silently, new ones are logged and dropped when full. Default 256.
	Queue int
	// Attempts is the total tries per delivery; default 4.
	Attempts int
	// Backoff is the wait before retry n (1-based); default 1s·4^(n-1).
	Backoff func(retry int) time.Duration
}

// Dispatcher sends bus events to webhooks.
type Dispatcher struct {
	bus  *events.Bus
	src  Source
	opts Options
	log  *slog.Logger

	mu      sync.Mutex
	version uint64
	loaded  bool
	targets []Target

	ready chan struct{}
}

// ErrPermanent marks a delivery the endpoint refused for good (a 4xx answer
// other than 408 and 429); it is not retried.
var ErrPermanent = errors.New("webhook: endpoint refused the delivery")

// New creates a Dispatcher.
func New(bus *events.Bus, src Source, opts Options) *Dispatcher {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.Queue <= 0 {
		opts.Queue = 256
	}
	if opts.Attempts <= 0 {
		opts.Attempts = 4
	}
	if opts.Backoff == nil {
		opts.Backoff = func(retry int) time.Duration {
			return time.Second << (2 * (min(retry, 5) - 1))
		}
	}
	if opts.Client == nil {
		opts.Client = &http.Client{
			Timeout:       opts.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Dispatcher{bus: bus, src: src, opts: opts, log: log.With("component", "webhook"), ready: make(chan struct{})}
}

type job struct {
	target Target
	event  events.Event
}

// Ready is closed once Run is subscribed to the bus: events published before
// that are not delivered.
func (d *Dispatcher) Ready() <-chan struct{} { return d.ready }

// Run delivers events until ctx ends; deliveries still waiting are dropped.
// It must be called once.
func (d *Dispatcher) Run(ctx context.Context) error {
	sub := d.bus.Subscribe(ctx, events.Filter{})
	close(d.ready)
	defer sub.Unsubscribe()
	jobs := make(chan job, d.opts.Queue)
	var wg sync.WaitGroup
	for range d.opts.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-jobs:
					d.deliverWithRetry(ctx, j)
				}
			}
		}()
	}
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-sub.C():
			if !ok {
				return nil
			}
			if e.Type == events.TypeTaskProgress {
				continue // far too chatty to deliver
			}
			for _, t := range d.matching(ctx, e) {
				select {
				case jobs <- job{target: t, event: e}:
				default:
					d.log.WarnContext(ctx, "webhook queue full; delivery dropped", "webhook", t.ID, "event", e.Type)
				}
			}
		}
	}
}

// matching returns the targets subscribed to e.
func (d *Dispatcher) matching(ctx context.Context, e events.Event) []Target {
	d.mu.Lock()
	defer d.mu.Unlock()
	if v := d.src.Version(); !d.loaded || v != d.version {
		ts, err := d.src.Targets(ctx)
		if err != nil {
			d.log.ErrorContext(ctx, "load webhooks", "error", err)
			if !d.loaded {
				return nil
			}
			// keep the previous snapshot and retry on the next event
		} else {
			d.targets, d.version, d.loaded = ts, v, true
		}
	}
	var out []Target
	for _, t := range d.targets {
		if slices.Contains(t.Events, string(e.Type)) {
			out = append(out, t)
		}
	}
	return out
}

func (d *Dispatcher) deliverWithRetry(ctx context.Context, j job) {
	var err error
	for attempt := 1; attempt <= d.opts.Attempts; attempt++ {
		if err = d.Deliver(ctx, j.target, j.event); err == nil || errors.Is(err, ErrPermanent) || ctx.Err() != nil {
			break
		}
		if attempt == d.opts.Attempts {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.opts.Backoff(attempt)):
		}
	}
	if err != nil && ctx.Err() == nil {
		d.log.WarnContext(ctx, "webhook delivery failed", "webhook", j.target.ID, "event", j.event.Type, "error", err)
	}
}

// Deliver sends one event to the target and returns the outcome of that single
// attempt: nil for a 2xx answer, an error wrapping ErrPermanent for a refusal,
// another error for failures worth retrying.
func (d *Dispatcher) Deliver(ctx context.Context, t Target, e events.Event) error {
	body, err := json.Marshal(Payload{ID: e.ID, Type: e.Type, Time: e.Time, TaskID: e.TaskID, QueueID: e.QueueID, Data: e.Data})
	if err != nil {
		return fmt.Errorf("%w: encode payload: %w", ErrPermanent, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPermanent, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "dlm-webhook")
	req.Header.Set(HeaderEvent, string(e.Type))
	req.Header.Set(HeaderDelivery, strconv.FormatUint(e.ID, 10)+"-"+t.ID)
	if len(t.Secret) > 0 {
		req.Header.Set(HeaderSignature, Sign(t.Secret, body))
	}
	resp, err := d.opts.Client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("webhook: endpoint answered %d", resp.StatusCode)
	default:
		return fmt.Errorf("%w: status %d", ErrPermanent, resp.StatusCode)
	}
}

// Sign returns the X-DLM-Signature value for body: "sha256=" and the hex
// HMAC-SHA256 of the body under secret.
func Sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// FuncSource adapts two functions to a Source, for wiring a dispatcher before
// the service that owns the webhooks exists.
type FuncSource struct {
	Load        func(ctx context.Context) ([]Target, error)
	VersionFunc func() uint64
}

// Targets implements Source.
func (f FuncSource) Targets(ctx context.Context) ([]Target, error) { return f.Load(ctx) }

// Version implements Source.
func (f FuncSource) Version() uint64 { return f.VersionFunc() }
