package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/ratelimit"
	"github.com/sabadia/dlm-server/internal/settings"
)

// watchdog aborts a request when one network read (or the wait for
// response headers) takes longer than the stall timeout. It is a single
// timer per stream, armed around each blocking read and disarmed after it,
// so time spent in the rate limiter or writing to disk never counts as a
// stall and no goroutine is started per read.
type watchdog struct {
	d     time.Duration
	timer *time.Timer
	fired atomic.Bool
}

func newWatchdog(d time.Duration, abort func()) *watchdog {
	w := &watchdog{d: d}
	w.timer = time.AfterFunc(d, func() {
		w.fired.Store(true)
		abort()
	})
	w.timer.Stop()
	return w
}

func (w *watchdog) arm()    { w.timer.Reset(w.d) }
func (w *watchdog) disarm() { w.timer.Stop() }

// watchReader arms the watchdog around every Read of the raw body.
type watchReader struct {
	r  io.Reader
	wd *watchdog
}

func (wr *watchReader) Read(p []byte) (int, error) {
	wr.wd.arm()
	n, err := wr.r.Read(p)
	wr.wd.disarm()
	return n, err //nolint:wrapcheck // io.Reader contract: io.EOF must stay unwrapped
}

// stream is an open response body positioned at a known file offset.
type stream struct {
	resp *http.Response
	// r is the body wrapped with the watchdog and the rate limiter, and
	// limited to the response's range when it is known (lr non-nil).
	r  io.Reader
	lr *io.LimitedReader
	wd *watchdog

	once    sync.Once
	release func()
}

// close closes the body and releases the request context. It is
// idempotent and safe on a nil stream.
func (s *stream) close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.wd.disarm()
		s.release()
	})
}

// newStream wraps resp's body. ctx bounds rate-limiter waits; abort stops a
// blocked read when the watchdog fires; limit (> 0) caps the bytes read;
// release frees everything.
func (d *Download) newStream(ctx context.Context, resp *http.Response, wd *watchdog, limit int64, release func()) *stream {
	st := &stream{resp: resp, wd: wd, release: release}
	st.r = ratelimit.Reader(ctx, &watchReader{r: resp.Body, wd: wd}, d.spec.Limiter)
	if limit > 0 {
		st.lr = &io.LimitedReader{R: st.r, N: limit}
		st.r = st.lr
	}
	return st
}

// attachmentStream turns the probe response kept from Resolve into a
// stream starting at offset 0, or returns nil (closing it) when it cannot
// be used: wrong status or range, or a size that disagrees with the
// resource. For a ranged download the stream ends where the probe's range
// ends; for a sequential one it must cover the whole file.
func (d *Download) attachmentStream(ctx context.Context, ranged bool) *stream {
	pr := d.attach
	d.attach = nil
	if pr == nil || pr.Response == nil || pr.Response.Body == nil {
		return nil
	}
	resp := pr.Response
	size := d.res.Size
	var limit int64
	ok := false
	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, last, total, valid := parseByteRange(resp.Header.Get("Content-Range"))
		if valid && start == 0 && total >= 0 && (size < 0 || total == size) {
			if ranged {
				ok, limit = true, last+1
			} else if last == total-1 {
				ok, limit = true, total
			}
		}
	case http.StatusOK:
		cl := resp.ContentLength
		switch {
		case ranged:
			ok, limit = cl < 0 || cl == size, size
		default:
			ok = size < 0 || cl < 0 || cl == size
		}
	}
	if !ok {
		_ = pr.Close() // unusable probe body; a fresh request replaces it
		return nil
	}
	body := resp.Body
	wd := newWatchdog(d.e.opts.stallTimeout, func() { _ = body.Close() })
	return d.newStream(ctx, resp, wd, limit, func() { _ = body.Close() })
}

// openRange requests [next, end) of the file. Request contexts derive from
// runCtx and are cancelled when ctx (the connection's context) ends, except
// that a 200 response kept for an onMismatch=restart switch is detached
// from ctx so it survives the end of the ranged phase.
func (d *Download) openRange(runCtx, ctx context.Context, next, end int64) (*stream, error) {
	reqCtx, cancel := context.WithCancel(runCtx) //nolint:gosec // released by the stream or below
	stop := context.AfterFunc(ctx, cancel)
	release := func() { stop(); cancel() }

	req, err := newRequest(reqCtx, d.source(), d.spec.Settings, d.requestURL())
	if err != nil {
		release()
		return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("httpx: %w", err))
	}
	req.Header.Set("Range", "bytes="+strconv.FormatInt(next, 10)+"-"+strconv.FormatInt(end-1, 10))
	if v := d.ifRange(); v != "" {
		req.Header.Set("If-Range", v)
	}
	wd := newWatchdog(d.e.opts.stallTimeout, cancel)
	wd.arm()
	resp, err := d.client.Do(req) //nolint:bodyclose // closed by the returned stream or below
	wd.disarm()
	if err != nil {
		release()
		return nil, d.streamErr(ctx, wd, err)
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
		if err := htmlInsteadOfFile(resp, d.source()); err != nil {
			_ = resp.Body.Close() // an error page, not the file
			release()
			return nil, err
		}
	}
	size := d.res.Size
	switch resp.StatusCode {
	case http.StatusPartialContent:
		cr := resp.Header.Get("Content-Range")
		start, last, total, ok := parseByteRange(cr)
		if !ok || start != next || last >= end || total != size || isCompressed(resp.Header) {
			_ = resp.Body.Close() // invalid response; its body cannot be placed
			release()
			return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf(
				"%w: requested bytes %d-%d/%d, got Content-Range %q", ErrRangeMismatch, next, end-1, size, cr))
		}
		body := resp.Body
		return d.newStream(reqCtx, resp, wd, last+1-next, func() { _ = body.Close(); release() }), nil
	case http.StatusOK:
		return nil, d.rangeIgnored(reqCtx, resp, wd, stop, cancel)
	default:
		se := NewStatusError(resp)
		_ = resp.Body.Close() // error response; status and headers suffice
		release()
		return nil, d.e.classifier.Classify(se)
	}
}

// rangeIgnored handles a 200 answer to a range request. With
// onMismatch=restart the first such response is kept (see restartSequential)
// and ErrRangeIgnored ends the ranged phase; otherwise it is closed and the
// download fails with a link-expired ErrRangeIgnored.
func (d *Download) rangeIgnored(reqCtx context.Context, resp *http.Response, wd *watchdog, stop func() bool, cancel context.CancelFunc) error {
	err := engine.LinkExpired(fmt.Errorf(
		"%w (status 200 to a range request): the file changed or the server stopped honouring ranges", ErrRangeIgnored))
	if d.onMismatch() == settings.MismatchRestart && stop() {
		body := resp.Body
		st := d.newStream(reqCtx, resp, wd, 0, func() { _ = body.Close(); cancel() })
		d.mu.Lock()
		kept := d.held == nil
		if kept {
			d.held = st
		}
		d.mu.Unlock()
		if !kept {
			st.close()
		}
		return err
	}
	_ = resp.Body.Close() // not used
	stop()
	cancel()
	return err
}

// openFull requests the whole file without Range (sequential mode).
func (d *Download) openFull(ctx context.Context) (*stream, error) {
	reqCtx, cancel := context.WithCancel(ctx) //nolint:gosec // released by the stream or below
	req, err := newRequest(reqCtx, d.source(), d.spec.Settings, d.requestURL())
	if err != nil {
		cancel()
		return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("httpx: %w", err))
	}
	wd := newWatchdog(d.e.opts.stallTimeout, cancel)
	wd.arm()
	resp, err := d.client.Do(req) //nolint:bodyclose // closed by the returned stream or below
	wd.disarm()
	if err != nil {
		cancel()
		return nil, d.streamErr(ctx, wd, err)
	}
	if resp.StatusCode != http.StatusOK {
		se := NewStatusError(resp)
		_ = resp.Body.Close() // error response; status and headers suffice
		cancel()
		return nil, d.e.classifier.Classify(se)
	}
	if err := htmlInsteadOfFile(resp, d.source()); err != nil {
		_ = resp.Body.Close() // an error page, not the file
		cancel()
		return nil, err
	}
	if err := d.adoptSize(resp.ContentLength); err != nil {
		_ = resp.Body.Close() // rejected response
		cancel()
		return nil, err
	}
	body := resp.Body
	return d.newStream(reqCtx, resp, wd, 0, func() { _ = body.Close(); cancel() }), nil
}

// adoptSize reconciles the size announced by a full response with the
// known size. A sequential download always restarts from 0, so a different
// size only means the file changed: allowed with onMismatch=restart (the
// new size and fingerprint size are adopted), a link-expired ErrSizeMismatch
// otherwise. An unknown size simply becomes the announced one.
func (d *Download) adoptSize(cl int64) error {
	if cl < 0 {
		return nil
	}
	cur := d.size.Load()
	if cur >= 0 && cl != cur {
		if d.onMismatch() != settings.MismatchRestart {
			return engine.LinkExpired(fmt.Errorf("%w: got %d, expected %d", ErrSizeMismatch, cl, cur))
		}
		d.updateSource(func(s *domain.Source) { s.Fingerprint.Size = cl })
	}
	d.size.Store(cl)
	return nil
}

// streamErr classifies a request or body error. A fired watchdog becomes a
// transient ErrStalled; cancellation of the connection context is returned
// as ctx.Err(); a cancelled request context that is not ours (for example
// the probe response's) is a transient network error.
func (d *Download) streamErr(ctx context.Context, wd *watchdog, err error) error {
	if ctx.Err() != nil {
		return ctx.Err() //nolint:wrapcheck // cancellation is reported as is
	}
	if wd != nil && wd.fired.Load() {
		return engine.Transient(domain.ErrorCodeNetwork,
			fmt.Errorf("%w: no data for %s: %w", ErrStalled, d.e.opts.stallTimeout, err))
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return engine.Transient(domain.ErrorCodeNetwork, fmt.Errorf("httpx: request aborted: %w", err))
	}
	return d.e.classifier.Classify(err)
}

// copySegment streams st into seg until the segment ends (its end may
// shrink while copying when another connection splits it), the response
// range is exhausted (nil, the caller requests the rest) or an error.
func (d *Download) copySegment(ctx context.Context, t *segTable, seg *segment, st *stream, buf []byte, prog interface{ Add(int64) }) error {
	for {
		n, rerr := st.r.Read(buf)
		if n > 0 {
			pos, w, atEnd := t.reserve(seg, int64(n))
			if w > 0 {
				if _, err := d.f.WriteAt(buf[:w], pos); err != nil {
					t.rollback(seg, pos)
					return d.fileError("write", err)
				}
				t.commit(seg, w)
				d.downloaded.Add(w)
				prog.Add(w)
			}
			if atEnd {
				return nil
			}
		}
		if errors.Is(rerr, io.EOF) {
			if st.lr != nil && st.lr.N == 0 {
				return nil // this response's range is done
			}
			return engine.Transient(domain.ErrorCodeNetwork, fmt.Errorf("httpx: body ended early: %w", io.ErrUnexpectedEOF))
		}
		if rerr != nil {
			return d.streamErr(ctx, st.wd, rerr)
		}
	}
}
