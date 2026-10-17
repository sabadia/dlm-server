package client

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// CaptureCookie is a cookie sent with a capture.
type CaptureCookie struct {
	Domain    string     `json:"domain"`
	Name      string     `json:"name"`
	Value     string     `json:"value"`
	Path      string     `json:"path,omitempty"`
	Secure    bool       `json:"secure,omitempty"`
	HTTPOnly  bool       `json:"httpOnly,omitempty"`
	SameSite  string     `json:"sameSite,omitempty"` // lax, strict, none
	HostOnly  bool       `json:"hostOnly,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// MediaCandidate is a media resource found on the captured page.
type MediaCandidate struct {
	URL     string `json:"url"`
	Kind    string `json:"kind,omitempty"` // hls, dash, video, audio, file
	MIME    string `json:"mime,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Variant string `json:"variant,omitempty"`
}

// CaptureRequest is the request of capture-download.
type CaptureRequest struct {
	URL      string `json:"url,omitempty"`
	FinalURL string `json:"finalUrl,omitempty"`
	PageURL  string `json:"pageUrl,omitempty"`
	Referrer string `json:"referrer,omitempty"`
	Filename string `json:"filename,omitempty"`
	MIME     string `json:"mime,omitempty"`
	Size     int64  `json:"size,omitempty"`

	Cookies         []CaptureCookie   `json:"cookies,omitempty"`
	UserAgent       string            `json:"userAgent,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	MediaCandidates []MediaCandidate  `json:"mediaCandidates,omitempty"`

	QueueID  string            `json:"queueId,omitempty"`
	Dir      string            `json:"dir,omitempty"`
	Settings json.RawMessage   `json:"settings,omitempty"`
	Priority string            `json:"priority,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Paused   bool              `json:"paused,omitempty"`
}

// CaptureResult is the response of capture-download.
type CaptureResult struct {
	Task    *Task               `json:"task"`
	Cookies ImportCookiesResult `json:"cookies"`
}

// CaptureDownload calls capture-download (POST /v1/capture).
func (c *Client) CaptureDownload(ctx context.Context, req CaptureRequest) (*CaptureResult, error) {
	var out CaptureResult
	if err := c.do(ctx, http.MethodPost, "/v1/capture", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
