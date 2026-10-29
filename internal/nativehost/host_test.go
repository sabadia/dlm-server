package nativehost

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
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

type fakePairing struct {
	started  []PairingRequest
	statuses map[string]PairingInfo
	err      error
}

func (f *fakePairing) Start(_ context.Context, req PairingRequest) (PairingInfo, error) {
	if f.err != nil {
		return PairingInfo{}, f.err
	}
	f.started = append(f.started, req)
	return PairingInfo{ID: "p1", Status: "pending", Code: "123456", ExpiresAt: time.Date(2026, 10, 30, 12, 2, 0, 0, time.UTC)}, nil
}

func (f *fakePairing) Status(_ context.Context, id string) (PairingInfo, error) {
	if f.err != nil {
		return PairingInfo{}, f.err
	}
	return f.statuses[id], nil
}

func testHost() *Host {
	return &Host{
		Daemon: func(context.Context) DaemonInfo {
			return DaemonInfo{BaseURL: "http://127.0.0.1:50123", Running: true, Version: "1.2.3", APIVersion: 1}
		},
		Pairing: &fakePairing{},
	}
}

func TestHostConnect(t *testing.T) {
	got := exchange(t, testHost(), `{"type":"connect","id":7}`)
	if len(got) != 1 {
		t.Fatalf("replies = %v", got)
	}
	r := got[0]
	if r["ok"] != true || r["type"] != "connect" || r["id"] != float64(7) || r["baseUrl"] != "http://127.0.0.1:50123" ||
		r["running"] != true || r["version"] != "1.2.3" || r["apiVersion"] != float64(1) {
		t.Errorf("reply = %v", r)
	}
	if _, has := r["token"]; has {
		t.Errorf("connect hands out a token: %v", r)
	}
}

func TestHostConnectReportsTLSFingerprintAndDownDaemon(t *testing.T) {
	h := testHost()
	h.Daemon = func(context.Context) DaemonInfo {
		return DaemonInfo{BaseURL: "https://nas.local:17171", Running: true, Version: "1.2.3", TLSFingerprint: "sha256:abc"}
	}
	if r := exchange(t, h, `{"type":"connect"}`)[0]; r["tlsFingerprint"] != "sha256:abc" {
		t.Errorf("reply = %v", r)
	}

	h.Daemon = func(context.Context) DaemonInfo { return DaemonInfo{BaseURL: "http://127.0.0.1:17171"} }
	r := exchange(t, h, `{"type":"connect"}`)[0]
	if r["ok"] != true || r["running"] != false || r["baseUrl"] != "http://127.0.0.1:17171" {
		t.Errorf("reply = %v, want ok with running=false", r)
	}
	if _, has := r["version"]; has {
		t.Errorf("reply has a version although the daemon is down: %v", r)
	}
}

func TestHostOriginCheck(t *testing.T) {
	tests := []struct {
		name    string
		origin  string
		allowed []string
		ok      bool
	}{
		{"the published extension", BuiltinChromiumOrigin, nil, true},
		{"listed", "chrome-extension://abc/", []string{"chrome-extension://abc"}, true},
		{"listed with slash", "chrome-extension://abc/", []string{"chrome-extension://abc/"}, true},
		{"not listed", "chrome-extension://evil/", []string{"chrome-extension://abc"}, false},
		{"nothing configured", "chrome-extension://abc/", nil, false},
		{"firefox passes no chrome origin", "", nil, true},
		{"firefox manifest argument", "/home/u/manifest.json", nil, true},
	}
	for _, tt := range tests {
		for _, msg := range []string{`{"type":"connect"}`, `{"type":"pair"}`, `{"type":"pair.status","pairingId":"p1"}`} {
			t.Run(tt.name+" "+msg, func(t *testing.T) {
				h := testHost()
				h.Origin, h.AllowedOrigins = tt.origin, tt.allowed
				r := exchange(t, h, msg)[0]
				if (r["ok"] == true) != tt.ok {
					t.Errorf("reply = %v, want ok=%v", r, tt.ok)
				}
				if !tt.ok && (r["error"] != CodeOriginNotAllowed || r["token"] != nil) {
					t.Errorf("refusal leaks or has the wrong code: %v", r)
				}
			})
		}
	}
}

func TestHostPair(t *testing.T) {
	pairing := &fakePairing{statuses: map[string]PairingInfo{
		"p1": {ID: "p1", Status: "approved", Code: "123456", Token: "dlmt_secret"},
		"p2": {ID: "p2", Status: "pending", Code: "654321"},
	}}
	h := testHost()
	h.Pairing = pairing
	h.Origin = BuiltinChromiumOrigin

	got := exchange(t, h,
		`{"type":"pair","id":1,"name":"Work Chrome","browser":"chrome","version":"0.1.0","clientId":"spoofed"}`,
		`{"type":"pair.status","pairingId":"p1"}`,
		`{"type":"pair.status","pairingId":"p2"}`,
		`{"type":"pair.status"}`,
	)
	if len(got) != 4 {
		t.Fatalf("replies = %v", got)
	}
	if r := got[0]; r["ok"] != true || r["pairingId"] != "p1" || r["code"] != "123456" || r["status"] != "pending" || r["id"] != float64(1) || r["expiresAt"] == nil {
		t.Errorf("pair = %v", r)
	}
	if len(pairing.started) != 1 {
		t.Fatalf("started = %+v", pairing.started)
	}
	// The identity comes from the browser, not from what the extension says.
	if q := pairing.started[0]; q.ClientID != BuiltinChromiumID || q.Name != "Work Chrome" || q.Browser != "chrome" || q.Version != "0.1.0" {
		t.Errorf("request = %+v", q)
	}
	if r := got[1]; r["ok"] != true || r["status"] != "approved" || r["token"] != "dlmt_secret" {
		t.Errorf("approved status = %v", r)
	}
	if r := got[2]; r["ok"] != true || r["status"] != "pending" || r["token"] != nil {
		t.Errorf("pending status = %v", r)
	}
	if r := got[3]; r["ok"] != false || r["error"] != CodeInvalidRequest {
		t.Errorf("missing id = %v", r)
	}
}

func TestHostPairFirefoxUsesTheAddonID(t *testing.T) {
	pairing := &fakePairing{}
	h := testHost()
	h.Pairing, h.AddonID = pairing, BuiltinFirefoxID
	exchange(t, h, `{"type":"pair","browser":"firefox"}`)
	if len(pairing.started) != 1 || pairing.started[0].ClientID != BuiltinFirefoxID {
		t.Errorf("started = %+v", pairing.started)
	}
}

func TestHostPairFailures(t *testing.T) {
	h := testHost()
	h.Pairing = &fakePairing{err: errors.New("pairing.too_many: 5 pairings already wait")}
	if r := exchange(t, h, `{"type":"pair"}`)[0]; r["ok"] != false || r["error"] != CodePairingFailed || r["message"] == "" {
		t.Errorf("reply = %v", r)
	}
	h.Pairing = nil
	if r := exchange(t, h, `{"type":"pair"}`)[0]; r["ok"] != false || r["error"] != CodePairingFailed {
		t.Errorf("without pairing: %v", r)
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
