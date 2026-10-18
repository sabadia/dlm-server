package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/portable"
	"github.com/sabadia/dlm-server/internal/storage"
	"github.com/sabadia/dlm-server/internal/urlpattern"
)

// ExportResult is a list of tasks written in one of the portable formats.
type ExportResult struct {
	Format  string
	Count   int
	Content string
}

// Export writes the tasks matching f (every page of them) as a list in the
// given format. Only what is needed to download again is exported: the link,
// its mirrors, the file name and directory, the referrer and the user agent.
// Captured headers and cookies are left out, since they may hold logins.
func (s *TaskService) Export(ctx context.Context, format string, f storage.TaskFilter) (*ExportResult, error) {
	if !portable.Valid(format) {
		return nil, fmt.Errorf("service: export tasks: %w", domain.InvalidArgument("unknown format %q (use json, urls or aria2)", format))
	}
	f.Sort, f.Limit, f.Cursor = storage.TaskSortCreated, storage.MaxTaskLimit, ""
	var entries []portable.Entry
	for {
		page, err := s.d.Store.Tasks().List(ctx, f)
		if err != nil {
			return nil, fmt.Errorf("service: export tasks: %w", err)
		}
		for _, t := range page.Tasks {
			entries = append(entries, portable.Entry{
				URL: t.Source.CurrentURL, Mirrors: t.Source.Mirrors, Name: t.Name, Dir: t.Dir,
				Referrer: t.Source.Referrer, UserAgent: t.Source.UserAgent,
			})
		}
		if page.NextCursor == "" {
			break
		}
		f.Cursor = page.NextCursor
	}
	// The newest come first from the store; a list reads better oldest first.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	content, err := portable.Format(format, entries)
	if err != nil {
		return nil, fmt.Errorf("service: export tasks: %w", err)
	}
	return &ExportResult{Format: format, Count: len(entries), Content: content}, nil
}

// ImportInput is a list of downloads to add.
type ImportInput struct {
	Format  string
	Content string
	// QueueID defaults to Main.
	QueueID string
	// Paused adds the tasks paused.
	Paused bool
	// AllowDuplicate skips the duplicate check.
	AllowDuplicate bool
	// ExpandPatterns expands ranges in the URLs (see urlpattern).
	ExpandPatterns bool
}

// Import parses a list and creates one task per entry, independently: the
// result has one item per task (an entry that fails to be created does not stop
// the others). An unreadable list returns an error matching
// domain.ErrInvalidArgument and creates nothing.
func (s *TaskService) Import(ctx context.Context, in ImportInput) ([]BatchResult, error) {
	entries, err := portable.Parse(in.Format, in.Content)
	if err != nil {
		return nil, fmt.Errorf("service: import tasks: %w", domain.InvalidArgument("%v", err))
	}
	var results []BatchResult
	var inputs []CreateTaskInput
	var slot []int // index in results of each input
	for _, e := range entries {
		item := CreateTaskInput{
			Source: domain.Source{
				CurrentURL: strings.TrimSpace(e.URL), Mirrors: e.Mirrors, Referrer: e.Referrer, UserAgent: e.UserAgent, Headers: e.Headers,
			},
			QueueID: in.QueueID, Name: e.Name, Dir: e.Dir, Paused: in.Paused, AllowDuplicate: in.AllowDuplicate,
		}
		items := []CreateTaskInput{item}
		if in.ExpandPatterns {
			var err error
			if items, err = ExpandPatternInput(item); err != nil {
				results = append(results, BatchResult{Err: err})
				continue
			}
		}
		for _, it := range items {
			slot = append(slot, len(results))
			results = append(results, BatchResult{})
			inputs = append(inputs, it)
		}
	}
	for j, r := range s.CreateBatch(ctx, inputs) {
		results[slot[j]] = r
	}
	return results, nil
}

// ExpandPatternInput returns one input per URL the range pattern in in's source
// stands for (see package urlpattern); an input without a range is returned as
// it is. A task name cannot be shared by several tasks, so a name together with
// a pattern that expands to several URLs is refused.
func ExpandPatternInput(in CreateTaskInput) ([]CreateTaskInput, error) {
	if in.ResolveID != "" {
		return []CreateTaskInput{in}, nil
	}
	urls, err := urlpattern.Expand(in.Source.CurrentURL, 0)
	if err != nil {
		return nil, domain.InvalidArgument("%v", err)
	}
	if len(urls) > 1 && in.Name != "" {
		return nil, domain.InvalidArgument("name cannot be set when the url expands to several tasks")
	}
	out := make([]CreateTaskInput, len(urls))
	for i, u := range urls {
		out[i] = in
		out[i].Source.CurrentURL = u
		if len(urls) > 1 || u != in.Source.CurrentURL {
			out[i].Source.OriginalURL = "" // defaults to the expanded link, not the pattern
		}
	}
	return out, nil
}

// Stats summarizes the downloads.
type Stats struct {
	Tasks int
	// ByStatus counts tasks per status; statuses without tasks are absent.
	ByStatus map[domain.TaskStatus]int
	// ByEngine has the tasks and bytes per protocol engine.
	ByEngine []storage.EngineStats
	// DownloadedBytes is what all tasks have downloaded so far.
	DownloadedBytes int64
	// Completed are the downloads completed in the last 24 hours, 7 days and
	// 30 days.
	Completed []CompletedWindow
}

// CompletedWindow is the downloads completed within a period.
type CompletedWindow struct {
	Period string
	Tasks  int
	Bytes  int64
}

// Stats returns the download statistics.
func (s *TaskService) Stats(ctx context.Context) (*Stats, error) {
	now := s.d.now()
	periods := []struct {
		name string
		d    time.Duration
	}{{"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}}
	since := make([]time.Time, len(periods))
	for i, p := range periods {
		since[i] = now.Add(-p.d)
	}
	raw, err := s.d.Store.Tasks().Stats(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("service: stats: %w", err)
	}
	st := &Stats{ByStatus: raw.ByStatus, ByEngine: raw.ByEngine, DownloadedBytes: raw.DownloadedBytes}
	for _, n := range raw.ByStatus {
		st.Tasks += n
	}
	for i, c := range raw.Completed {
		st.Completed = append(st.Completed, CompletedWindow{Period: periods[i].name, Tasks: c.Tasks, Bytes: c.Bytes})
	}
	return st, nil
}
