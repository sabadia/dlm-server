package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/retry"
)

// runRanged downloads the segments with up to maxConns connections. All
// goroutines (the controller and every connection) belong to one errgroup:
// the first error cancels the others and is returned after all have
// stopped.
func (d *Download) runRanged(runCtx context.Context) error {
	t := d.table()
	first := t.acquire()
	if first == nil {
		return nil // restored segments are already complete
	}
	var attached *stream
	if first.start == 0 && first.next == 0 {
		attached = d.attachmentStream(runCtx, true)
	}
	_ = d.attach.Close() // not usable for a resumed first segment
	d.attach = nil

	g, ctx := errgroup.WithContext(runCtx)
	idle := make(chan struct{}, 1)
	spawn := func(s *segment, st *stream) {
		d.conns.Add(1)
		g.Go(func() error { return d.connection(runCtx, ctx, t, s, st, idle) })
	}
	spawn(first, attached)
	g.Go(func() error { return d.control(ctx, t, idle, func(s *segment) { spawn(s, nil) }) })
	return g.Wait() //nolint:wrapcheck // errors are classified where they occur
}

// control grows the number of connections (slow start), restarts work if
// every connection exited early, and writes periodic checkpoints. It
// returns when every byte is written or ctx is done.
//
// Slow start: every slowStartInterval, when the download made progress
// since the previous tick and fewer than maxConns connections are active,
// the number of connections is doubled (capped at maxConns); each new
// connection receives work from segTable.acquire (an unowned restored
// segment, or half of the largest remaining segment if it is at least the
// minimum split size). A stalled or failing download therefore never adds
// connections.
func (d *Download) control(ctx context.Context, t *segTable, idle <-chan struct{}, spawn func(*segment)) error {
	grow := time.NewTicker(d.e.opts.slowStartIntv)
	defer grow.Stop()
	ckpt := time.NewTicker(d.e.opts.checkpointIntv)
	defer ckpt.Stop()
	last := d.downloaded.Load()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-idle:
			if t.complete() {
				return nil
			}
			if d.conns.Load() == 0 {
				s := t.acquire()
				if s == nil {
					return engine.Fatal(domain.ErrorCodeFatal, errors.New("httpx: incomplete segments but no work to assign"))
				}
				spawn(s)
			}
		case <-grow.C:
			cur := d.downloaded.Load()
			progressed := cur > last
			last = cur
			active := int(d.conns.Load())
			if !progressed || active >= d.maxConns {
				continue
			}
			for range min(max(active, 1), d.maxConns-active) {
				s := t.acquire()
				if s == nil {
					break
				}
				spawn(s)
			}
		case <-ckpt.C:
			d.checkpoint(ctx)
		}
	}
}

// connection is one transfer connection: it downloads its segment, then
// steals more work (segTable.acquire) until none is left.
func (d *Download) connection(runCtx, ctx context.Context, t *segTable, seg *segment, st *stream, idle chan<- struct{}) error {
	defer func() {
		d.conns.Add(-1)
		select {
		case idle <- struct{}{}:
		default: // a wake-up is already pending
		}
	}()
	buf := make([]byte, bufferSize)
	for seg != nil {
		if err := d.fetchSegment(runCtx, ctx, t, seg, st, buf); err != nil {
			return err
		}
		st = nil
		t.release(seg)
		seg = t.acquire()
	}
	return nil
}

// fetchSegment downloads seg, retrying transient failures with the task's
// retry policy. Bytes written since the previous failure reset the retry
// budget (retry.ResetOnProgress). st, if non-nil, is used for the first
// attempt instead of a new request.
func (d *Download) fetchSegment(runCtx, ctx context.Context, t *segTable, seg *segment, st *stream, buf []byte) error {
	defer st.close()
	var prog retry.Progress
	err := retry.Do(ctx, d.policy, func(ctx context.Context, _ int) error {
		first := st
		st = nil
		return d.fetchOnce(runCtx, ctx, t, seg, first, buf, &prog)
	}, retry.ResetOnProgress(prog.Moved), retry.WithSleeper(d.e.opts.sleep), retry.OnRetry(d.logRetry(seg.idx)))
	if err != nil {
		return fmt.Errorf("httpx: segment %d: %w", seg.idx, err)
	}
	return nil
}

// fetchOnce issues range requests until seg is complete. A response that
// covers less than requested (a probe body or a server capping range
// sizes) is followed by a request for the rest.
func (d *Download) fetchOnce(runCtx, ctx context.Context, t *segTable, seg *segment, st *stream, buf []byte, prog *retry.Progress) error {
	for {
		next, end := t.span(seg)
		if next >= end {
			st.close()
			return nil
		}
		if st == nil {
			var err error
			if st, err = d.openRange(runCtx, ctx, next, end); err != nil {
				return err
			}
		}
		err := d.copySegment(ctx, t, seg, st, buf, prog)
		st.close()
		st = nil
		if err != nil {
			return err
		}
	}
}

// runSequential downloads the whole file over one connection from offset
// 0, using st (a kept probe or restart response) for the first attempt
// when non-nil. Every retry starts again from 0. A retry counts as
// progress (resetting the retry budget) only when it got further than any
// previous attempt, so a server that always fails at the same point
// exhausts the budget.
func (d *Download) runSequential(ctx context.Context, st *stream) error {
	d.conns.Store(1)
	defer d.conns.Store(0)
	defer st.close()
	buf := make([]byte, bufferSize)
	var high int64
	var prog retry.Progress
	err := retry.Do(ctx, d.policy, func(ctx context.Context, _ int) error {
		cur := st
		st = nil
		if cur == nil {
			var err error
			if cur, err = d.openFull(ctx); err != nil {
				return err
			}
		}
		defer cur.close()
		return d.copySequential(ctx, cur, buf, &high, &prog)
	}, retry.ResetOnProgress(prog.Moved), retry.WithSleeper(d.e.opts.sleep), retry.OnRetry(d.logRetry(0)))
	if err != nil {
		return fmt.Errorf("httpx: sequential download: %w", err)
	}
	return nil
}

func (d *Download) copySequential(ctx context.Context, st *stream, buf []byte, high *int64, prog *retry.Progress) error {
	d.downloaded.Store(0)
	size := d.size.Load()
	var pos int64
	for {
		n, rerr := st.r.Read(buf)
		if n > 0 {
			if size >= 0 && pos+int64(n) > size {
				return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w: more than %d bytes received", ErrRangeMismatch, size))
			}
			if _, err := d.f.WriteAt(buf[:n], pos); err != nil {
				return d.fileError("write", err)
			}
			pos += int64(n)
			d.downloaded.Store(pos)
			if pos > *high {
				prog.Add(pos - *high)
				*high = pos
			}
			d.maybeCheckpoint(ctx)
		}
		if errors.Is(rerr, io.EOF) {
			if size >= 0 && pos != size {
				return engine.Transient(domain.ErrorCodeNetwork,
					fmt.Errorf("httpx: body ended at %d of %d bytes: %w", pos, size, io.ErrUnexpectedEOF))
			}
			return nil
		}
		if rerr != nil {
			return d.streamErr(ctx, st.wd, rerr)
		}
	}
}
