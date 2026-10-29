package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/domain"
)

// SegmentList is the response of list-task-segments.
type SegmentList struct {
	Segments []domain.Segment `json:"segments" doc:"The byte ranges of the download in file order, as last checkpointed (about once a second while the task runs). Empty for engines that do not split the file and for tasks that have not started."`
}

// SetTaskFilesBody is the request of update-task-files.
type SetTaskFilesBody struct {
	Selected []int `json:"selected" minItems:"1" doc:"The indexes of the files to download; every other file of the task is skipped. At least one file must stay selected."`
}

func (s *Server) registerTaskDetail() {
	Register(s, huma.Operation{
		OperationID: "list-task-segments",
		Method:      http.MethodGet,
		Path:        "/v1/tasks/{id}/segments",
		Summary:     "List a task's segments",
		Description: "The byte ranges a task downloads in parallel and how much of each is done: the data of a segments map.",
		Tags:        []string{tagTasks},
		Errors:      []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.listTaskSegments)

	Register(s, huma.Operation{
		OperationID: "update-task-files",
		Method:      http.MethodPatch,
		Path:        "/v1/tasks/{id}/files",
		Summary:     "Choose the files of a task",
		Description: "Changes which files of a multi-file task (a torrent) are downloaded, also after it was created. " +
			"The task's size follows the selection and the next run downloads exactly the selected files; " +
			"files already downloaded and no longer selected stay on disk. Active and completed tasks cannot change " +
			"(409 task.files_locked: pause the task first, restart a completed one); an unknown index or an empty selection is 422.",
		Tags:   []string{tagTasks},
		Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.updateTaskFiles)

	Register(s, huma.Operation{
		OperationID: "get-task-torrent",
		Method:      http.MethodGet,
		Path:        "/v1/tasks/{id}/torrent",
		Summary:     "Describe a torrent task",
		Description: "The swarm of a torrent task: state, info hash, trackers, connected peers, bytes shared and the share ratio. " +
			"Live while the task runs; otherwise what its last run saved (state stopped). " +
			"404 task.no_torrent for a task of another engine or a torrent that never ran.",
		Tags:   []string{tagTasks},
		Errors: []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.getTaskTorrent)
}

func (s *Server) listTaskSegments(ctx context.Context, in *taskPathInput) (*struct{ Body SegmentList }, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	segs, err := s.tasks.Segments(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "list-task-segments", err, locBody)
	}
	if segs == nil {
		segs = []domain.Segment{}
	}
	return &struct{ Body SegmentList }{Body: SegmentList{Segments: segs}}, nil
}

func (s *Server) updateTaskFiles(ctx context.Context, in *struct {
	ID   string `path:"id" doc:"Task ID."`
	Body SetTaskFilesBody
},
) (*taskOutput, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	t, err := s.tasks.SetFiles(ctx, in.ID, in.Body.Selected)
	if err != nil {
		return nil, s.fail(ctx, "update-task-files", err, locBody)
	}
	return &taskOutput{Body: t}, nil
}

func (s *Server) getTaskTorrent(ctx context.Context, in *taskPathInput) (*struct{ Body *domain.TorrentInfo }, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	info, err := s.tasks.Torrent(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "get-task-torrent", err, locBody)
	}
	return &struct{ Body *domain.TorrentInfo }{Body: info}, nil
}
