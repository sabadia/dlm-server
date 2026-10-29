package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postTorrent(t *testing.T, s *Server, path string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func torrentBody(n int) []byte {
	return append(append([]byte("d4:infod4:name1:a"), bytes.Repeat([]byte("x"), n)...), "ee"...)
}

func TestTorrentUploadAcceptsWhatTheJSONLimitRefuses(t *testing.T) {
	s := newTestServer(t)

	// A 2 MiB file is far above the 1 MiB of an ordinary request, and this one takes it
	// (the services are not wired in this test, so the answer after the checks is 503).
	w := postTorrent(t, s, "/v1/tasks/torrent", torrentBody(2<<20), "application/x-bittorrent")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("2 MiB torrent: status = %d, want 503 (past the body limit and the shape check); body: %s", w.Code, w.Body)
	}
	// The same size in the JSON of create-task is refused.
	w = postTorrent(t, s, "/v1/tasks", torrentBody(2<<20), "application/json")
	assertProblem(t, w, http.StatusRequestEntityTooLarge)

	// 16 MiB is the limit.
	w = postTorrent(t, s, "/v1/tasks/torrent", torrentBody(int(MaxTorrentBodyBytes)), "application/x-bittorrent")
	assertProblem(t, w, http.StatusRequestEntityTooLarge)
}

func TestTorrentUploadRefusesWhatIsNotATorrent(t *testing.T) {
	s := newTestServer(t)
	assertProblem(t, postTorrent(t, s, "/v1/tasks/torrent", nil, "application/x-bittorrent"), http.StatusBadRequest) // no body at all
	for name, body := range map[string][]byte{
		"text":       []byte("hello"),
		"not a dict": []byte("l4:spame"),
		"unclosed":   []byte("d4:name1:a"),
	} {
		w := postTorrent(t, s, "/v1/tasks/torrent", body, "application/x-bittorrent")
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "torrent.not_a_torrent") {
			t.Errorf("%s: status %d body %s", name, w.Code, w.Body)
		}
	}
}
