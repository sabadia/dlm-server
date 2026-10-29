package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/urlpattern"
)

// MaxExpandPreview bounds how many URLs expand-tasks lists.
const MaxExpandPreview = 1000

// ExpandTasksBody is the request of expand-tasks.
type ExpandTasksBody struct {
	Pattern string `json:"pattern" minLength:"1" maxLength:"8192" doc:"A URL with ranges: file[001-100].jpg, img[a-e].png, p[1-9][1-3].html; [first-last:step] steps."`
	Preview int    `json:"preview,omitempty" minimum:"0" maximum:"1000" doc:"How many of the URLs to list (default 20); count always has the full number."`
}

// ExpandedPattern is the response of expand-tasks.
type ExpandedPattern struct {
	Count int      `json:"count" doc:"How many URLs the pattern stands for (a pattern without ranges: 1)."`
	URLs  []string `json:"urls" doc:"The first URLs, up to preview."`
}

// ResolveTasksBody is the request of resolve-tasks.
type ResolveTasksBody struct {
	Items []ResolveTaskBody `json:"items" minItems:"1" maxItems:"100" doc:"The downloads to probe; each is like a resolve-task request."`
}

// ResolveItem is the outcome of one item of resolve-tasks.
type ResolveItem struct {
	OK     bool                   `json:"ok" doc:"Whether the probe succeeded."`
	Result *service.ResolveResult `json:"result,omitempty" doc:"What the source would download (as resolve-task answers), with duplicateOf when another task already has it."`
	Error  *Problem               `json:"error,omitempty" doc:"Why the item failed (RFC 9457 problem with a code)."`
}

// ResolveTasksOutput is the response of resolve-tasks: always 200, one item
// per request item.
type ResolveTasksOutput struct {
	Body struct {
		Results []ResolveItem `json:"results" doc:"One result per item, in the order of the request."`
	}
}

// FSBrowseInput is the request of get-fs-browse.
type FSBrowseInput struct {
	Path string `query:"path" doc:"An absolute directory below server.fs_roots; empty lists the roots."`
}

// FSEntry is a directory in an FSListing.
type FSEntry struct {
	Name string `json:"name"`
	Path string `json:"path" doc:"Absolute path, usable as the path of the next listing."`
}

// FSListing is the response of get-fs-browse.
type FSListing struct {
	Path      string    `json:"path" doc:"The listed directory; empty when the roots are listed."`
	Parent    string    `json:"parent,omitempty" doc:"The directory above path; absent at a root."`
	Entries   []FSEntry `json:"entries" doc:"The sub-directories, sorted by name."`
	Truncated bool      `json:"truncated,omitempty" doc:"The directory has more sub-directories than were listed."`
}

// FSFree is the response of get-fs-free.
type FSFree struct {
	Path      string `json:"path"`
	FreeBytes uint64 `json:"freeBytes" doc:"Bytes available to the daemon in the directory's file system."`
}

func (s *Server) registerAddFlow() {
	Register(s, huma.Operation{
		OperationID: "expand-tasks",
		Method:      http.MethodPost,
		Path:        "/v1/tasks/expand",
		Summary:     "Preview a URL pattern",
		Description: "Says how many URLs a pattern with ranges stands for and lists the first ones, so a client can show a preview before adding them. " +
			"A pattern that expands to more than 10000 URLs, or a malformed range, is 422.",
		Tags:   []string{tagTasks},
		Errors: []int{http.StatusUnprocessableEntity},
	}, s.expandTasks)

	Register(s, huma.Operation{
		OperationID: "resolve-tasks",
		Method:      http.MethodPost,
		Path:        "/v1/resolve/batch",
		Summary:     "Resolve several downloads",
		Description: "Probes up to 100 sources, a few at a time, and answers one result per item (200 even when some failed). " +
			"Each success has a resolveId like resolve-task, and duplicateOf when another task already has the link or the file name and size.",
		Tags:   []string{tagTasks},
		Errors: []int{http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.resolveTasks)

	Register(s, huma.Operation{
		OperationID: "get-fs-browse",
		Method:      http.MethodGet,
		Path:        "/v1/fs/browse",
		Summary:     "List directories on the daemon's machine",
		Description: "Lists the sub-directories of a directory below server.fs_roots, for choosing a download directory on a daemon that runs elsewhere. " +
			"Without a path it lists the roots. 404 fs.disabled when server.fs_roots is empty; 422 for a path outside the roots (also through symbolic links), not absolute or not a directory.",
		Tags:   []string{"system"},
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, s.fsBrowse)

	Register(s, huma.Operation{
		OperationID: "get-fs-free",
		Method:      http.MethodGet,
		Path:        "/v1/fs/free",
		Summary:     "Free space of a directory on the daemon's machine",
		Description: "The bytes available to the daemon in a directory below server.fs_roots. Same errors as get-fs-browse.",
		Tags:        []string{"system"},
		Errors:      []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, s.fsFree)
}

func (s *Server) expandTasks(_ context.Context, in *struct{ Body ExpandTasksBody }) (*struct{ Body ExpandedPattern }, error) {
	urls, err := urlpattern.Expand(in.Body.Pattern, 0)
	if err != nil {
		return nil, newProblem(http.StatusUnprocessableEntity, err.Error(),
			&huma.ErrorDetail{Message: err.Error(), Location: "body.pattern"})
	}
	n := in.Body.Preview
	if n == 0 {
		n = 20
	}
	out := ExpandedPattern{Count: len(urls), URLs: urls[:min(n, len(urls), MaxExpandPreview)]}
	return &struct{ Body ExpandedPattern }{Body: out}, nil
}

func (s *Server) resolveTasks(ctx context.Context, in *struct{ Body ResolveTasksBody }) (*ResolveTasksOutput, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	inputs := make([]service.ResolveInput, len(in.Body.Items))
	out := &ResolveTasksOutput{}
	out.Body.Results = make([]ResolveItem, len(in.Body.Items))
	valid := make([]int, 0, len(inputs)) // indexes of the items that have a usable source
	var sub []service.ResolveInput
	for i, it := range in.Body.Items {
		src, err := it.source(true)
		if err != nil {
			out.Body.Results[i] = ResolveItem{Error: s.asProblem(ctx, "resolve-tasks", err, locBodySettings)}
			continue
		}
		inputs[i] = service.ResolveInput{Source: src, QueueID: it.QueueID, Settings: it.Settings}
		valid = append(valid, i)
		sub = append(sub, inputs[i])
	}
	for j, r := range s.tasks.ResolveBatch(ctx, sub) {
		i := valid[j]
		if r.Err != nil {
			out.Body.Results[i] = ResolveItem{Error: s.problemFor(ctx, "resolve-tasks", r.Err, locBodySettings)}
			continue
		}
		out.Body.Results[i] = ResolveItem{OK: true, Result: r.Result}
	}
	return out, nil
}

func (s *Server) fsBrowse(ctx context.Context, in *FSBrowseInput) (*struct{ Body FSListing }, error) {
	l, err := s.fs.List(in.Path)
	if err != nil {
		return nil, s.fail(ctx, "get-fs-browse", err, locBody)
	}
	out := FSListing{Path: l.Path, Parent: l.Parent, Truncated: l.Truncated, Entries: make([]FSEntry, len(l.Entries))}
	for i, e := range l.Entries {
		out.Entries[i] = FSEntry{Name: e.Name, Path: e.Path}
	}
	return &struct{ Body FSListing }{Body: out}, nil
}

func (s *Server) fsFree(ctx context.Context, in *FSBrowseInput) (*struct{ Body FSFree }, error) {
	n, err := s.fs.Free(in.Path)
	if err != nil {
		return nil, s.fail(ctx, "get-fs-free", err, locBody)
	}
	return &struct{ Body FSFree }{Body: FSFree{Path: in.Path, FreeBytes: n}}, nil
}
