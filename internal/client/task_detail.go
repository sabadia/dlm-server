package client

import (
	"context"
	"net/http"
)

// Segment is a byte range of a download and how much of it is done.
type Segment struct {
	Index int   `json:"index"`
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	Done  int64 `json:"done"`
}

// TorrentPeer is one connected peer of a torrent task.
type TorrentPeer struct {
	Address      string `json:"address"`
	Client       string `json:"client,omitempty"`
	DownloadRate int64  `json:"downloadRate"`
}

// TorrentInfo describes the swarm of a torrent task.
type TorrentInfo struct {
	InfoHash       string        `json:"infoHash,omitempty"`
	Name           string        `json:"name,omitempty"`
	Private        bool          `json:"private"`
	State          string        `json:"state"`
	Downloaded     int64         `json:"downloaded"`
	Uploaded       int64         `json:"uploaded"`
	Ratio          float64       `json:"ratio"`
	SeedingSeconds int64         `json:"seedingSeconds"`
	Trackers       []string      `json:"trackers,omitempty"`
	PeerCount      int           `json:"peerCount"`
	KnownPeers     int           `json:"knownPeers"`
	Seeders        int           `json:"seeders"`
	Peers          []TorrentPeer `json:"peers,omitempty"`
}

// TaskSegments calls list-task-segments (GET /v1/tasks/{id}/segments).
func (c *Client) TaskSegments(ctx context.Context, id string) ([]Segment, error) {
	var out struct {
		Segments []Segment `json:"segments"`
	}
	if err := c.do(ctx, http.MethodGet, taskPath(id, "/segments"), nil, &out); err != nil {
		return nil, err
	}
	return out.Segments, nil
}

// SetTaskFiles calls update-task-files (PATCH /v1/tasks/{id}/files): only the
// files with the given indexes are downloaded.
func (c *Client) SetTaskFiles(ctx context.Context, id string, selected []int) (*Task, error) {
	var out Task
	body := struct {
		Selected []int `json:"selected"`
	}{selected}
	if err := c.do(ctx, http.MethodPatch, taskPath(id, "/files"), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TaskTorrent calls get-task-torrent (GET /v1/tasks/{id}/torrent).
func (c *Client) TaskTorrent(ctx context.Context, id string) (*TorrentInfo, error) {
	var out TorrentInfo
	if err := c.do(ctx, http.MethodGet, taskPath(id, "/torrent"), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
