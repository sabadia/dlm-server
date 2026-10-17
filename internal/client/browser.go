package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// BrowserCandidate is a download found on a page.
type BrowserCandidate struct {
	Index int    `json:"index"`
	URL   string `json:"url"`
	// Kind is hls, dash, video, audio or file.
	Kind     string `json:"kind"`
	MIME     string `json:"mime,omitempty"`
	Size     int64  `json:"size"` // -1 when unknown
	Filename string `json:"filename,omitempty"`
	// Source is network, download or blob.
	Source  string            `json:"source"`
	Page    string            `json:"page,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	FoundAt time.Time         `json:"foundAt"`
	// File is set (on the daemon's disk) for downloads the browser saved.
	File string `json:"file,omitempty"`
}

// PageResult is the outcome of ResolvePage.
type PageResult struct {
	ID         string             `json:"id"`
	URL        string             `json:"url"`
	FinalURL   string             `json:"finalUrl"`
	Title      string             `json:"title"`
	Group      string             `json:"group"`
	Candidates []BrowserCandidate `json:"candidates"`
	ExpiresAt  time.Time          `json:"expiresAt"`
}

// BrowserInstall describes the browser the daemon uses.
type BrowserInstall struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
}

// BrowserRunning is a running browser process.
type BrowserRunning struct {
	Group    string `json:"group"`
	Sessions int    `json:"sessions"`
}

// BrowserStatus is the response of get-browser-status.
type BrowserStatus struct {
	Browser *BrowserInstall  `json:"browser,omitempty"`
	Running []BrowserRunning `json:"running"`
	Max     int              `json:"max"`
}

// DownloadCandidateRequest is the request of download-candidate.
type DownloadCandidateRequest struct {
	QueueID  string            `json:"queueId,omitempty"`
	Name     string            `json:"name,omitempty"`
	Dir      string            `json:"dir,omitempty"`
	Settings json.RawMessage   `json:"settings,omitempty"`
	Priority string            `json:"priority,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Paused   bool              `json:"paused,omitempty"`
}

// ResolvePage calls resolve-page (POST /v1/browser/resolve). waitSeconds 0
// uses the daemon's default.
func (c *Client) ResolvePage(ctx context.Context, pageURL string, waitSeconds int) (*PageResult, error) {
	body := struct {
		URL         string `json:"url"`
		WaitSeconds int    `json:"waitSeconds,omitempty"`
	}{URL: pageURL, WaitSeconds: waitSeconds}
	var out PageResult
	if err := c.do(ctx, http.MethodPost, "/v1/browser/resolve", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DownloadCandidate calls download-candidate
// (POST /v1/browser/results/{id}/candidates/{index}/download).
func (c *Client) DownloadCandidate(ctx context.Context, resultID string, index int, req DownloadCandidateRequest) (*Task, error) {
	path := "/v1/browser/results/" + url.PathEscape(resultID) + "/candidates/" + strconv.Itoa(index) + "/download"
	return c.taskCall(ctx, http.MethodPost, path, req)
}

// BrowserStatus calls get-browser-status (GET /v1/browser).
func (c *Client) BrowserStatus(ctx context.Context) (*BrowserStatus, error) {
	var out BrowserStatus
	if err := c.do(ctx, http.MethodGet, "/v1/browser", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BrowserProfiles calls list-browser-profiles (GET /v1/browser/profiles).
func (c *Client) BrowserProfiles(ctx context.Context) ([]string, error) {
	var out struct {
		Profiles []string `json:"profiles"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/browser/profiles", nil, &out); err != nil {
		return nil, err
	}
	return out.Profiles, nil
}

// DeleteBrowserProfile calls delete-browser-profile (DELETE /v1/browser/profiles/{group}).
func (c *Client) DeleteBrowserProfile(ctx context.Context, group string) error {
	return c.do(ctx, http.MethodDelete, "/v1/browser/profiles/"+url.PathEscape(group), nil, nil)
}
