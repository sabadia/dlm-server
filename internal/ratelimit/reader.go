package ratelimit

import (
	"context"
	"io"

	"github.com/sabadia/dlm-server/internal/engine"
)

// Reader returns a reader that charges l for every byte read from r. Each
// Read waits for exactly the number of bytes it returned, after reading
// them, so short reads are never over-charged. When the wait fails (ctx
// done) Read returns the bytes it read together with the wait error. A nil
// limiter returns r unchanged.
func Reader(ctx context.Context, r io.Reader, l engine.Limiter) io.Reader {
	if l == nil {
		return r
	}
	return &limitedReader{ctx: ctx, r: r, l: l}
}

type limitedReader struct {
	ctx context.Context // stored: io.Reader.Read has no ctx parameter
	r   io.Reader
	l   engine.Limiter
}

func (lr *limitedReader) Read(p []byte) (int, error) {
	n, err := lr.r.Read(p)
	if n > 0 {
		if werr := lr.l.WaitN(lr.ctx, n); werr != nil {
			return n, werr //nolint:wrapcheck // limiter errors are already wrapped
		}
	}
	return n, err //nolint:wrapcheck // io.Reader contract: io.EOF must be returned unwrapped
}
