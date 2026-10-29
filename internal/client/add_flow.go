package client

import (
	"context"
	"net/http"
	"net/url"
)

// ExpandedPattern is the answer of expand-tasks.
type ExpandedPattern struct {
	Count int      `json:"count"`
	URLs  []string `json:"urls"`
}

// ExpandTasks calls expand-tasks (POST /v1/tasks/expand): how many URLs a
// pattern stands for, and the first preview of them (0: the daemon default).
func (c *Client) ExpandTasks(ctx context.Context, pattern string, preview int) (*ExpandedPattern, error) {
	body := struct {
		Pattern string `json:"pattern"`
		Preview int    `json:"preview,omitempty"`
	}{pattern, preview}
	var out ExpandedPattern
	if err := c.do(ctx, http.MethodPost, "/v1/tasks/expand", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResolveItem is the outcome of one item of resolve-tasks.
type ResolveItem struct {
	OK     bool           `json:"ok"`
	Result *ResolveResult `json:"result,omitempty"`
	Error  *APIError      `json:"error,omitempty"`
}

// ResolveTasks calls resolve-tasks (POST /v1/resolve/batch).
func (c *Client) ResolveTasks(ctx context.Context, items []ResolveRequest) ([]ResolveItem, error) {
	body := struct {
		Items []ResolveRequest `json:"items"`
	}{items}
	var out struct {
		Results []ResolveItem `json:"results"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/resolve/batch", body, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// FSEntry is a directory of an FSListing.
type FSEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// FSListing is the answer of get-fs-browse.
type FSListing struct {
	Path      string    `json:"path"`
	Parent    string    `json:"parent,omitempty"`
	Entries   []FSEntry `json:"entries"`
	Truncated bool      `json:"truncated,omitempty"`
}

// FSBrowse calls get-fs-browse (GET /v1/fs/browse); an empty path lists the
// roots.
func (c *Client) FSBrowse(ctx context.Context, path string) (*FSListing, error) {
	var out FSListing
	if err := c.do(ctx, http.MethodGet, "/v1/fs/browse?path="+url.QueryEscape(path), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FSFree is the answer of get-fs-free.
type FSFree struct {
	Path      string `json:"path"`
	FreeBytes uint64 `json:"freeBytes"`
}

// FSFreeSpace calls get-fs-free (GET /v1/fs/free).
func (c *Client) FSFreeSpace(ctx context.Context, path string) (*FSFree, error) {
	var out FSFree
	if err := c.do(ctx, http.MethodGet, "/v1/fs/free?path="+url.QueryEscape(path), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
