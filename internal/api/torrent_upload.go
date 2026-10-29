package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
)

// AddTorrentInput is the request of add-torrent-file: the .torrent file as the
// body and the task's options in the query.
type AddTorrentInput struct {
	QueueID        string          `query:"queueId" doc:"Queue to add the task to (default: the Main queue)."`
	Name           string          `query:"name" doc:"File or folder name without directories (default: the torrent's name)."`
	Dir            string          `query:"dir" doc:"Absolute download directory (default: the effective downloadDir)."`
	Priority       domain.Priority `query:"priority" enum:"low,normal,high" doc:"Priority within the queue (default normal)."`
	Paused         bool            `query:"paused" doc:"Create the task paused instead of queued."`
	AllowDuplicate bool            `query:"allowDuplicate" doc:"Add the task even when the same torrent is already a task."`
	// RawBody is the .torrent file.
	RawBody []byte `contentType:"application/x-bittorrent"`
}

func (s *Server) registerTorrentUpload() {
	Register(s, huma.Operation{
		OperationID: "add-torrent-file",
		Method:      http.MethodPost,
		Path:        torrentPath,
		Summary:     "Add a download from a .torrent file",
		Description: "Creates a torrent task from the content of a .torrent file, sent as the request body (Content-Type application/x-bittorrent), up to 16 MiB. " +
			"Use it instead of a data: URL in create-task, which is limited to the 1 MiB of an ordinary request.",
		Tags:          []string{tagTasks},
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  MaxTorrentBodyBytes,
		Errors:        []int{http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.addTorrentFile)
}

func (s *Server) addTorrentFile(ctx context.Context, in *AddTorrentInput) (*taskOutput, error) {
	data := bytes.TrimSpace(in.RawBody)
	// A .torrent file is one bencoded dictionary: d…e.
	if len(data) < 2 || data[0] != 'd' || data[len(data)-1] != 'e' {
		return nil, newProblem(http.StatusUnprocessableEntity, "the body is not a .torrent file").withCode("torrent.not_a_torrent", nil)
	}
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	t, err := s.tasks.Create(ctx, service.CreateTaskInput{
		Source:  domain.Source{CurrentURL: "data:application/x-bittorrent;base64," + base64.StdEncoding.EncodeToString(data)},
		QueueID: in.QueueID, Name: in.Name, Dir: in.Dir, Priority: in.Priority, Paused: in.Paused, AllowDuplicate: in.AllowDuplicate,
	})
	if err != nil {
		return nil, s.fail(ctx, "add-torrent-file", err, locBody)
	}
	return &taskOutput{Body: t}, nil
}
