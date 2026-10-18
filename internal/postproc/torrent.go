package postproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxTorrentFile bounds a .torrent file turned into a task (the torrent engine
// refuses larger ones).
const maxTorrentFile = 16 << 20

// NewTorrent describes a torrent task to create from a .torrent file.
type NewTorrent struct {
	// Data is the content of the .torrent file.
	Data []byte
	// Dir is where the torrent's files are downloaded.
	Dir string
	// QueueID is the queue of the new task.
	QueueID string
}

// TorrentAdder creates a task for a .torrent file. An already known torrent is
// not an error.
type TorrentAdder interface {
	AddTorrent(ctx context.Context, t NewTorrent) error
}

// TorrentAdderFunc adapts a function to TorrentAdder.
type TorrentAdderFunc func(ctx context.Context, t NewTorrent) error

// AddTorrent implements TorrentAdder.
func (f TorrentAdderFunc) AddTorrent(ctx context.Context, t NewTorrent) error { return f(ctx, t) }

// AddTorrent is the post action that creates a torrent task from a downloaded
// .torrent file (parameters: dir, delete). Other files are left alone.
type AddTorrent struct {
	Adder TorrentAdder
}

// Run implements Action.
func (a AddTorrent) Run(ctx context.Context, j *Job, params map[string]string) error {
	if !strings.EqualFold(filepath.Ext(j.Path), ".torrent") {
		return nil
	}
	if a.Adder == nil {
		return errors.New("torrents are not available")
	}
	f, err := os.Open(j.Path) //nolint:gosec // the task's own file
	if err != nil {
		return fmt.Errorf("open %q: %w", j.Path, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxTorrentFile+1))
	_ = f.Close() // read-only
	if err != nil {
		return fmt.Errorf("read %q: %w", j.Path, err)
	}
	if len(data) > maxTorrentFile {
		return fmt.Errorf("%q is larger than %d bytes", j.Path, maxTorrentFile)
	}
	dir := params["dir"]
	if dir == "" {
		dir = filepath.Dir(j.Path)
	}
	if err := a.Adder.AddTorrent(ctx, NewTorrent{Data: data, Dir: dir, QueueID: j.Task.QueueID}); err != nil {
		return err //nolint:wrapcheck // the adder's reason
	}
	j.Note("add-torrent: a torrent task was created from %s", filepath.Base(j.Path))
	if params["delete"] == "true" {
		if err := os.Remove(j.Path); err != nil {
			return fmt.Errorf("remove %q: %w", j.Path, err)
		}
	}
	return nil
}
