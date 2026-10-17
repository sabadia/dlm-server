package client

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// maxSSELine bounds one SSE line; event payloads are small JSON documents.
const maxSSELine = 1 << 20

// sseMessage is one dispatched Server-Sent Events message.
type sseMessage struct {
	ID    string // last event ID at dispatch time (may be inherited)
	Event string // event type; "message" when the server sent none
	Data  string // data lines joined with "\n"
}

// sseParser reads Server-Sent Events as specified by the WHATWG HTML
// standard ("Parsing an event stream"): lines end in CRLF, LF or CR, lines
// starting with ':' are comments, a blank line dispatches the event, and an
// event without data is not dispatched.
type sseParser struct {
	sc     *bufio.Scanner
	lastID string        // the "last event ID buffer"; persists across events
	retry  time.Duration // last valid retry field, 0 if none
}

func newSSEParser(r io.Reader) *sseParser {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxSSELine)
	sc.Split(scanSSELines)
	return &sseParser{sc: sc}
}

// next returns the next dispatched message. It returns io.EOF when the
// stream ends; a partially received event at the end is discarded, as the
// standard requires.
func (p *sseParser) next() (sseMessage, error) {
	var (
		event   string
		data    strings.Builder
		hasData bool
	)
	for p.sc.Scan() {
		line := p.sc.Text()
		if line == "" {
			if !hasData {
				event = ""
				continue
			}
			if event == "" {
				event = "message"
			}
			return sseMessage{ID: p.lastID, Event: event, Data: strings.TrimSuffix(data.String(), "\n")}, nil
		}
		if line[0] == ':' {
			continue // comment
		}
		field, value, found := strings.Cut(line, ":")
		if found {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			event = value
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
			hasData = true
		case "id":
			if !strings.ContainsRune(value, 0) {
				p.lastID = value
			}
		case "retry":
			if ms, err := strconv.ParseUint(value, 10, 31); err == nil {
				p.retry = time.Duration(ms) * time.Millisecond
			}
		default:
			// Unknown fields are ignored.
		}
	}
	if err := p.sc.Err(); err != nil {
		return sseMessage{}, fmt.Errorf("client: read event stream: %w", err)
	}
	return sseMessage{}, io.EOF
}

// scanSSELines is a bufio.SplitFunc that splits on CRLF, LF or a lone CR.
func scanSSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\n' {
			return i + 1, data[:i], nil
		}
		// CR: it may be the first half of a CRLF split across reads.
		if i+1 < len(data) {
			if data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
			return i + 1, data[:i], nil
		}
		if atEOF {
			return i + 1, data[:i], nil
		}
		return 0, nil, nil // need one more byte
	}
	if atEOF {
		// A final line without terminator never completes an event, but
		// return it so the scanner state stays consistent.
		return len(data), data, nil
	}
	return 0, nil, nil
}
