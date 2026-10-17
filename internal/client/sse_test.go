package client

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/go-cmp/cmp"
)

func parseAll(t *testing.T, r io.Reader) ([]sseMessage, *sseParser) {
	t.Helper()
	p := newSSEParser(r)
	var out []sseMessage
	for {
		m, err := p.next()
		if errors.Is(err, io.EOF) {
			return out, p
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		out = append(out, m)
	}
}

func TestSSEParser(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []sseMessage
	}{
		{
			name: "basic",
			in:   "id: b-1\nevent: task.status\ndata: {\"a\":1}\n\n",
			want: []sseMessage{{ID: "b-1", Event: "task.status", Data: `{"a":1}`}},
		},
		{
			name: "multi-line data",
			in:   "data: line1\ndata: line2\ndata:\n\n",
			want: []sseMessage{{Event: "message", Data: "line1\nline2\n"}},
		},
		{
			name: "comments and heartbeats are ignored",
			in:   ": ping\n\n:another\ndata: x\n: inline comment\n\n",
			want: []sseMessage{{Event: "message", Data: "x"}},
		},
		{
			name: "CRLF line endings",
			in:   "id: b-2\r\nevent: e\r\ndata: y\r\n\r\n",
			want: []sseMessage{{ID: "b-2", Event: "e", Data: "y"}},
		},
		{
			name: "lone CR line endings",
			in:   "event: e\rdata: z\r\r",
			want: []sseMessage{{Event: "e", Data: "z"}},
		},
		{
			name: "id persists and an event without data is not dispatched",
			in:   "id: b-3\nevent: ignored\n\ndata: first\n\nevent: reset\ndata: {}\n\n",
			want: []sseMessage{{ID: "b-3", Event: "message", Data: "first"}, {ID: "b-3", Event: "reset", Data: "{}"}},
		},
		{
			name: "no space after colon and field without colon",
			in:   "data:nospace\nevent\ndata\n\n",
			want: []sseMessage{{Event: "message", Data: "nospace\n"}},
		},
		{
			name: "unterminated last event is discarded",
			in:   "data: done\n\ndata: partial\n",
			want: []sseMessage{{Event: "message", Data: "done"}},
		},
		{
			name: "id with NUL is ignored",
			in:   "id: ok\n\nid: bad\x00\ndata: d\n\n",
			want: []sseMessage{{ID: "ok", Event: "message", Data: "d"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := parseAll(t, strings.NewReader(tt.in))
			if diff := cmp.Diff(tt.want, got, cmp.AllowUnexported(sseMessage{})); diff != "" {
				t.Errorf("messages mismatch (-want +got):\n%s", diff)
			}
			// Byte-at-a-time reads exercise CRLF split across reads.
			got, _ = parseAll(t, iotest.OneByteReader(strings.NewReader(tt.in)))
			if diff := cmp.Diff(tt.want, got, cmp.AllowUnexported(sseMessage{})); diff != "" {
				t.Errorf("one-byte reader: messages mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSSEParserRetry(t *testing.T) {
	_, p := parseAll(t, strings.NewReader("retry: 2500\n\nretry: bogus\n\n"))
	if p.retry != 2500*time.Millisecond {
		t.Fatalf("retry = %s, want 2.5s", p.retry)
	}
}

func TestSSEParserReadError(t *testing.T) {
	p := newSSEParser(iotest.ErrReader(errors.New("boom")))
	if _, err := p.next(); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("next = %v, want read error", err)
	}
}
