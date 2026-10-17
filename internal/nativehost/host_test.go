package nativehost

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

// exchange sends the given raw messages to a host and returns its replies.
func exchange(t *testing.T, h *Host, msgs ...string) []map[string]any {
	t.Helper()
	var in, out bytes.Buffer
	for _, m := range msgs {
		if err := WriteMessage(&in, []byte(m)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Serve(context.Background(), &in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var replies []map[string]any
	for {
		raw, err := ReadMessage(&out, MaxResponseSize)
		if errors.Is(err, io.EOF) {
			return replies
		}
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("reply %q: %v", raw, err)
		}
		replies = append(replies, m)
	}
}

func testHost() *Host {
	return &Host{
		BaseURL: "http://127.0.0.1:17171",
		Token:   func() (string, error) { return "tok-123", nil },
		Health:  func(context.Context) (string, error) { return "1.2.3", nil },
	}
}

func TestHostConnect(t *testing.T) {
	got := exchange(t, testHost(), `{"type":"connect","id":7}`)
	if len(got) != 1 {
		t.Fatalf("replies = %v", got)
	}
	r := got[0]
	if r["ok"] != true || r["type"] != "connect" || r["id"] != float64(7) || r["baseUrl"] != "http://127.0.0.1:17171" ||
		r["token"] != "tok-123" || r["running"] != true || r["version"] != "1.2.3" {
		t.Errorf("reply = %v", r)
	}
}

func TestHostConnectDaemonDown(t *testing.T) {
	h := testHost()
	h.Health = func(context.Context) (string, error) { return "", errors.New("connection refused") }
	r := exchange(t, h, `{"type":"connect"}`)[0]
	if r["ok"] != true || r["running"] != false || r["token"] != "tok-123" {
		t.Errorf("reply = %v, want ok with running=false", r)
	}
	if _, has := r["version"]; has {
		t.Errorf("reply has a version although the daemon is down: %v", r)
	}
}

func TestHostConnectWithoutToken(t *testing.T) {
	for name, tok := range map[string]func() (string, error){
		"missing": func() (string, error) { return "", errors.New("no such file") },
		"empty":   func() (string, error) { return "", nil },
	} {
		t.Run(name, func(t *testing.T) {
			h := testHost()
			h.Token = tok
			r := exchange(t, h, `{"type":"connect"}`)[0]
			if r["ok"] != false || r["error"] != CodeTokenUnavailable || r["token"] != nil {
				t.Errorf("reply = %v", r)
			}
		})
	}
}

func TestHostOriginCheck(t *testing.T) {
	tests := []struct {
		name    string
		origin  string
		allowed []string
		ok      bool
	}{
		{"listed", "chrome-extension://abc/", []string{"chrome-extension://abc"}, true},
		{"listed with slash", "chrome-extension://abc/", []string{"chrome-extension://abc/"}, true},
		{"not listed", "chrome-extension://evil/", []string{"chrome-extension://abc"}, false},
		{"nothing configured", "chrome-extension://abc/", nil, false},
		{"firefox passes no chrome origin", "", nil, true},
		{"firefox manifest argument", "/home/u/manifest.json", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := testHost()
			h.Origin, h.AllowedOrigins = tt.origin, tt.allowed
			r := exchange(t, h, `{"type":"connect"}`)[0]
			if (r["ok"] == true) != tt.ok {
				t.Errorf("reply = %v, want ok=%v", r, tt.ok)
			}
			if !tt.ok && (r["error"] != CodeOriginNotAllowed || r["token"] != nil) {
				t.Errorf("refusal leaks or has the wrong code: %v", r)
			}
		})
	}
}

func TestHostPingAndErrors(t *testing.T) {
	got := exchange(t, testHost(), `{"type":"ping","id":"a"}`, `{"type":"nope","id":"b"}`, `not json`, `{}`, `[1]`)
	want := []map[string]any{
		{"ok": true, "type": "ping", "id": "a"},
		{"ok": false, "type": "nope", "id": "b", "error": CodeUnknownType},
		{"ok": false, "error": CodeInvalidRequest},
		{"ok": false, "error": CodeInvalidRequest},
		{"ok": false, "error": CodeInvalidRequest},
	}
	if len(got) != len(want) {
		t.Fatalf("replies = %v", got)
	}
	for i := range want {
		for k, v := range want[i] {
			if got[i][k] != v {
				t.Errorf("reply %d: %s = %v, want %v (reply %v)", i, k, got[i][k], v, got[i])
			}
		}
		if got[i]["token"] != nil {
			t.Errorf("reply %d carries a token: %v", i, got[i])
		}
	}
}

func TestHostServeEndsOnBrokenStream(t *testing.T) {
	hdr := make([]byte, 4)
	binary.NativeEndian.PutUint32(hdr, MaxRequestSize+1)
	var out bytes.Buffer
	err := testHost().Serve(context.Background(), bytes.NewReader(hdr), &out)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("Serve = %v, want ErrMessageTooLarge", err)
	}
	raw, rerr := ReadMessage(&out, MaxResponseSize)
	if rerr != nil || !bytes.Contains(raw, []byte(CodeInvalidRequest)) {
		t.Errorf("no error reply before ending: %q, %v", raw, rerr)
	}
}

func TestHostServeStopsWhenContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var in bytes.Buffer
	_ = WriteMessage(&in, []byte(`{"type":"ping"}`))
	var out bytes.Buffer
	if err := testHost().Serve(ctx, &in, &out); err != nil || out.Len() != 0 {
		t.Errorf("Serve = %v with %d bytes written, want nil and none", err, out.Len())
	}
}
