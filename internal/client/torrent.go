package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// TorrentUpload are the options of AddTorrentFile.
type TorrentUpload struct {
	QueueID        string
	Name           string
	Dir            string
	Priority       string
	Paused         bool
	AllowDuplicate bool
}

// AddTorrentFile calls add-torrent-file (POST /v1/tasks/torrent): data is the
// content of a .torrent file, up to 16 MiB.
func (c *Client) AddTorrentFile(ctx context.Context, data []byte, opts TorrentUpload) (*Task, error) {
	q := url.Values{}
	for k, v := range map[string]string{"queueId": opts.QueueID, "name": opts.Name, "dir": opts.Dir, "priority": opts.Priority} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if opts.Paused {
		q.Set("paused", strconv.FormatBool(true))
	}
	if opts.AllowDuplicate {
		q.Set("allowDuplicate", strconv.FormatBool(true))
	}
	path := "/v1/tasks/torrent"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out Task
	if err := c.doBytes(ctx, http.MethodPost, path, "application/x-bittorrent", data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
