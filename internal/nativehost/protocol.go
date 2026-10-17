package nativehost

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Message size limits of the Native Messaging protocol. A message is a
// 32-bit length in native byte order followed by that many bytes of UTF-8
// JSON. Browsers accept at most 1 MiB from a host; the host accepts at most
// MaxRequestSize from the browser (the protocol allows 4 GiB, far more than
// this host ever needs).
const (
	MaxResponseSize = 1 << 20
	MaxRequestSize  = 1 << 20
)

// ErrMessageTooLarge reports a message above the size limit.
var ErrMessageTooLarge = errors.New("nativehost: message too large")

// ReadMessage reads one framed message. It returns io.EOF when the input
// ended cleanly before a message, and io.ErrUnexpectedEOF when it ended in
// the middle of one.
func ReadMessage(r io.Reader, limit uint32) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err //nolint:wrapcheck // io.EOF / io.ErrUnexpectedEOF are matched by callers
	}
	n := binary.NativeEndian.Uint32(hdr[:])
	if n > limit {
		return nil, fmt.Errorf("%w: %d bytes (limit %d)", ErrMessageTooLarge, n, limit)
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err //nolint:wrapcheck // io.ErrUnexpectedEOF is matched by callers
	}
	return msg, nil
}

// WriteMessage writes one framed message.
func WriteMessage(w io.Writer, msg []byte) error {
	if len(msg) > MaxResponseSize {
		return fmt.Errorf("%w: %d bytes (limit %d)", ErrMessageTooLarge, len(msg), MaxResponseSize)
	}
	buf := make([]byte, 4, 4+len(msg))
	binary.NativeEndian.PutUint32(buf, uint32(len(msg))) //nolint:gosec // bounded by MaxResponseSize above
	buf = append(buf, msg...)
	// One write, so the frame is never interleaved with other output.
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("nativehost: write message: %w", err)
	}
	return nil
}
