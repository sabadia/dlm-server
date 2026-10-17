package httpx

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNewStatusError(t *testing.T) {
	u, _ := url.Parse("https://user:secret@example.com/f.zip")
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable",
		Header:  http.Header{"Retry-After": {"5"}},
		Request: &http.Request{URL: u},
	}
	e := NewStatusError(resp)
	resp.Header.Set("Retry-After", "9") // must be a copy
	if e.StatusCode != http.StatusServiceUnavailable || e.Header.Get("Retry-After") != "5" {
		t.Errorf("unexpected %+v", e)
	}
	if strings.Contains(e.Error(), "secret") || !strings.Contains(e.Error(), "503") {
		t.Errorf("Error() = %q; must include status and redact credentials", e.Error())
	}
}
