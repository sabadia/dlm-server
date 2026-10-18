package service

import (
	"context"
	"encoding/base64"
	"errors"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/postproc"
)

// AddTorrentFile creates a torrent task from the content of a .torrent file
// (the add-torrent post action). The file travels in the task's source as a
// data URL. A torrent that another task already has is not an error.
func (s *TaskService) AddTorrentFile(ctx context.Context, t postproc.NewTorrent) error {
	src := domain.Source{CurrentURL: "data:application/x-bittorrent;base64," + base64.StdEncoding.EncodeToString(t.Data)}
	_, err := s.Create(ctx, CreateTaskInput{Source: src, QueueID: t.QueueID, Dir: t.Dir})
	if errors.Is(err, domain.ErrConflict) {
		return nil
	}
	return err
}
