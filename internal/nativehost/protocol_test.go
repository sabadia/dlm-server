package nativehost

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	for _, msg := range []string{`{"a":1}`, ``, `{"emoji":"héllo ✓"}`} {
		if err := WriteMessage(&buf, []byte(msg)); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{`{"a":1}`, ``, `{"emoji":"héllo ✓"}`} {
		got, err := ReadMessage(&buf, MaxRequestSize)
		if err != nil || string(got) != want {
			t.Fatalf("ReadMessage = %q, %v; want %q", got, err, want)
		}
	}
	if _, err := ReadMessage(&buf, MaxRequestSize); !errors.Is(err, io.EOF) {
		t.Errorf("ReadMessage at end = %v, want io.EOF", err)
	}
}

func TestWriteMessageUsesNativeByteOrder(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if len(b) != 7 || binary.NativeEndian.Uint32(b[:4]) != 3 || string(b[4:]) != "abc" {
		t.Errorf("frame = %v", b)
	}
}

func TestReadMessageErrors(t *testing.T) {
	frame := func(n uint32, body string) *bytes.Reader {
		hdr := make([]byte, 4)
		binary.NativeEndian.PutUint32(hdr, n)
		return bytes.NewReader(append(hdr, body...))
	}
	tests := []struct {
		name string
		in   io.Reader
		want error
	}{
		{"too large", frame(MaxRequestSize+1, ""), ErrMessageTooLarge},
		{"truncated header", bytes.NewReader([]byte{1, 0}), io.ErrUnexpectedEOF},
		{"truncated body", frame(10, "abc"), io.ErrUnexpectedEOF},
		{"missing body", frame(5, ""), io.ErrUnexpectedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ReadMessage(tt.in, MaxRequestSize); !errors.Is(err, tt.want) {
				t.Errorf("ReadMessage = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestWriteMessageTooLarge(t *testing.T) {
	if err := WriteMessage(io.Discard, make([]byte, MaxResponseSize+1)); !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("WriteMessage = %v, want ErrMessageTooLarge", err)
	}
}
