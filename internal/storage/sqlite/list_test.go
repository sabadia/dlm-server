package sqlite_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
)

// seedTasks creates 30 tasks in Main and 5 in queue "q1" with a mix of
// priorities, statuses, duplicate positions and duplicate creation times so
// that the ID tie-breaker is exercised.
func seedTasks(t *testing.T, s *sqlite.Store) []*domain.Task {
	t.Helper()
	ctx := t.Context()
	if err := s.Queues().Create(ctx, newQueue(t, "q1", "Other", 1)); err != nil {
		t.Fatal(err)
	}
	var all []*domain.Task
	for i := range 35 {
		queueID := domain.MainQueueID
		if i >= 30 {
			queueID = "q1"
		}
		name := fmt.Sprintf("file-%02d.bin", i)
		url := fmt.Sprintf("https://host-%d.example.com/%s", i%3, name)
		if i == 7 {
			name = "Movie_Special%.mkv"
		}
		task := newTask(t, fmt.Sprintf("t%02d", i), queueID, name, url)
		task.Priority = domain.Priority(i%3 - 1)
		task.Position = int64(i % 4)
		task.CreatedAt = baseTime.Add(time.Duration(i/2) * time.Second)
		task.UpdatedAt = task.CreatedAt
		if i%5 == 0 {
			if err := task.TransitionTo(domain.StatusPaused, task.CreatedAt); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Tasks().Create(ctx, task); err != nil {
			t.Fatal(err)
		}
		all = append(all, truncTask(task))
	}
	return all
}

func positionOrder(a, b *domain.Task) int {
	switch {
	case a.Priority != b.Priority:
		return int(b.Priority - a.Priority)
	case a.Position != b.Position:
		return int(a.Position - b.Position)
	case !a.CreatedAt.Equal(b.CreatedAt):
		return a.CreatedAt.Compare(b.CreatedAt)
	default:
		return compareStrings(a.ID, b.ID)
	}
}

func createdOrder(a, b *domain.Task) int {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return b.CreatedAt.Compare(a.CreatedAt)
	}
	return compareStrings(b.ID, a.ID)
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// listAll pages through the listing and returns the IDs in order.
func listAll(t *testing.T, s *sqlite.Store, f storage.TaskFilter) []string {
	t.Helper()
	var ids []string
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page > 100 {
			t.Fatal("pagination does not terminate")
		}
		res, err := s.Tasks().List(t.Context(), f)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if f.Limit > 0 && len(res.Tasks) > f.Limit {
			t.Fatalf("page has %d tasks, limit %d", len(res.Tasks), f.Limit)
		}
		for _, task := range res.Tasks {
			if seen[task.ID] {
				t.Fatalf("duplicate task %s across pages", task.ID)
			}
			seen[task.ID] = true
			ids = append(ids, task.ID)
		}
		if res.NextCursor == "" {
			return ids
		}
		f.Cursor = res.NextCursor
	}
}

func expectIDs(all []*domain.Task, keep func(*domain.Task) bool, order func(a, b *domain.Task) int) []string {
	var sel []*domain.Task
	for _, task := range all {
		if keep(task) {
			sel = append(sel, task)
		}
	}
	slices.SortFunc(sel, order)
	ids := []string{}
	for _, task := range sel {
		ids = append(ids, task.ID)
	}
	return ids
}

func TestTaskList(t *testing.T) {
	s := openStore(t)
	all := seedTasks(t, s)
	everything := func(*domain.Task) bool { return true }

	tests := []struct {
		name   string
		filter storage.TaskFilter
		keep   func(*domain.Task) bool
		order  func(a, b *domain.Task) int
	}{
		{"default sort single page", storage.TaskFilter{}, everything, positionOrder},
		{"position sort paged", storage.TaskFilter{Sort: storage.TaskSortPosition, Limit: 4}, everything, positionOrder},
		{"created sort paged", storage.TaskFilter{Sort: storage.TaskSortCreated, Limit: 3}, everything, createdOrder},
		{"page size 1", storage.TaskFilter{Limit: 1}, everything, positionOrder},
		{
			"queue filter",
			storage.TaskFilter{QueueID: "q1", Limit: 2},
			func(t *domain.Task) bool { return t.QueueID == "q1" },
			positionOrder,
		},
		{
			"status filter",
			storage.TaskFilter{Statuses: []domain.TaskStatus{domain.StatusPaused}, Limit: 2, Sort: storage.TaskSortCreated},
			func(t *domain.Task) bool { return t.Status == domain.StatusPaused },
			createdOrder,
		},
		{
			"multiple statuses with queue",
			storage.TaskFilter{QueueID: domain.MainQueueID, Statuses: []domain.TaskStatus{domain.StatusPaused, domain.StatusQueued}, Limit: 7},
			func(t *domain.Task) bool { return t.QueueID == domain.MainQueueID },
			positionOrder,
		},
		{
			"search name case-insensitive",
			storage.TaskFilter{Search: "FILE-1"},
			func(t *domain.Task) bool { return t.Name >= "file-10" && t.Name < "file-20" },
			positionOrder,
		},
		{
			"search url",
			storage.TaskFilter{Search: "HOST-2.example", Limit: 3},
			func(t *domain.Task) bool { return t.Source.CurrentURL[len("https://host-")] == '2' },
			positionOrder,
		},
		{
			"search wildcards are literal",
			storage.TaskFilter{Search: "l%"},
			func(t *domain.Task) bool { return t.ID == "t07" },
			positionOrder,
		},
		{
			"search underscore is literal",
			storage.TaskFilter{Search: "_"},
			func(t *domain.Task) bool { return t.ID == "t07" },
			positionOrder,
		},
		{"no match", storage.TaskFilter{Search: "zzz"}, func(*domain.Task) bool { return false }, positionOrder},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := expectIDs(all, tc.keep, tc.order)
			got := listAll(t, s, tc.filter)
			if got == nil {
				got = []string{}
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("ids (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("returns full tasks", func(t *testing.T) {
		res, err := s.Tasks().List(t.Context(), storage.TaskFilter{Search: "Movie"})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Tasks) != 1 {
			t.Fatalf("got %d tasks", len(res.Tasks))
		}
		if diff := cmp.Diff(all[7], res.Tasks[0], allowUnexported); diff != "" {
			t.Errorf("task (-want +got):\n%s", diff)
		}
	})

	t.Run("default and max limit", func(t *testing.T) {
		for i := range 40 {
			id := fmt.Sprintf("extra-%02d", i)
			if err := s.Tasks().Create(t.Context(), newTask(t, id, "", id, "https://e.com/"+id)); err != nil {
				t.Fatal(err)
			}
		}
		res, err := s.Tasks().List(t.Context(), storage.TaskFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Tasks) != storage.DefaultTaskLimit || res.NextCursor == "" {
			t.Fatalf("default page: %d tasks, cursor %q", len(res.Tasks), res.NextCursor)
		}
		res, err = s.Tasks().List(t.Context(), storage.TaskFilter{Limit: 100000})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Tasks) != 75 || res.NextCursor != "" {
			t.Fatalf("clamped page: %d tasks, cursor %q", len(res.Tasks), res.NextCursor)
		}
	})

	t.Run("invalid input", func(t *testing.T) {
		res, err := s.Tasks().List(t.Context(), storage.TaskFilter{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		for name, f := range map[string]storage.TaskFilter{
			"garbage cursor":   {Cursor: "!!!"},
			"non-json cursor":  {Cursor: "YWJj"},
			"cursor sort swap": {Cursor: res.NextCursor, Sort: storage.TaskSortCreated},
			"unknown sort":     {Sort: "bogus"},
			"position order":   {Order: storage.OrderDesc},
			"unknown order":    {Sort: storage.TaskSortName, Order: "sideways"},
			"unknown status":   {Statuses: []domain.TaskStatus{"nope"}},
		} {
			if _, err := s.Tasks().List(t.Context(), f); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
			}
		}
	})
}

// seedVaried makes tasks that differ in everything the extra filters and
// sorts look at.
func seedVaried(t *testing.T, s *sqlite.Store) {
	t.Helper()
	type spec struct {
		id, name, engine, ctype string
		size                    int64
		labels                  map[string]string
		created                 int
		completed               int // seconds after baseTime, 0: not completed
	}
	for _, sp := range []spec{
		{"v1", "Alpha.iso", "http", "application/x-iso9660-image", 5_000, map[string]string{"project": "os", "owner": "ann"}, 1, 50},
		{"v2", "bravo.ZIP", "http", "application/zip", 100, map[string]string{"project": "docs"}, 2, 0},
		{"v3", "charlie.mkv", "bt", "video/x-matroska", 9_000, nil, 3, 30},
		{"v4", "Delta.mp4", "http", "video/mp4", -1, map[string]string{"owner": "bob"}, 4, 0},
		{"v5", "echo.txt", "ftp", "", 100, nil, 5, 10},
	} {
		task := newTask(t, sp.id, "", sp.name, "https://e.com/"+sp.id)
		task.Engine, task.Size, task.Labels = sp.engine, sp.size, sp.labels
		task.Source.Fingerprint.ContentType = sp.ctype
		task.CreatedAt = baseTime.Add(time.Duration(sp.created) * time.Second)
		task.UpdatedAt = task.CreatedAt
		if sp.completed > 0 {
			at := baseTime.Add(time.Duration(sp.completed) * time.Second)
			task.CompletedAt = &at
		}
		if err := s.Tasks().Create(t.Context(), task); err != nil {
			t.Fatal(err)
		}
	}
}

func idsOf(t *testing.T, s *sqlite.Store, f storage.TaskFilter) []string {
	t.Helper()
	got := listAll(t, s, f)
	if got == nil {
		got = []string{}
	}
	return got
}

func TestTaskListFiltersAndSorts(t *testing.T) {
	s := openStore(t)
	seedVaried(t, s)
	ptr := func(n int64) *int64 { return &n }
	at := func(sec int) time.Time { return baseTime.Add(time.Duration(sec) * time.Second) }

	filters := []struct {
		name string
		f    storage.TaskFilter
		want []string // in created order, newest first
	}{
		{"engine", storage.TaskFilter{Engine: "http"}, []string{"v4", "v2", "v1"}},
		{"label key", storage.TaskFilter{Label: "owner"}, []string{"v4", "v1"}},
		{"label key=value", storage.TaskFilter{Label: "owner=ann"}, []string{"v1"}},
		{"label value mismatch", storage.TaskFilter{Label: "owner=zed"}, []string{}},
		{"suffix, case-insensitive", storage.TaskFilter{NameSuffixes: []string{".zip", ".iso"}}, []string{"v2", "v1"}},
		{"content type", storage.TaskFilter{ContentTypes: []string{"application/zip"}}, []string{"v2"}},
		{"content type wildcard", storage.TaskFilter{ContentTypes: []string{"video/*"}}, []string{"v4", "v3"}},
		{"suffix or content type", storage.TaskFilter{NameSuffixes: []string{".txt"}, ContentTypes: []string{"video/*"}}, []string{"v5", "v4", "v3"}},
		{"min size leaves out unknown", storage.TaskFilter{MinSize: ptr(100)}, []string{"v5", "v3", "v2", "v1"}},
		{"size range", storage.TaskFilter{MinSize: ptr(100), MaxSize: ptr(5_000)}, []string{"v5", "v2", "v1"}},
		{"max size leaves out unknown", storage.TaskFilter{MaxSize: ptr(200)}, []string{"v5", "v2"}},
		{"created range", storage.TaskFilter{CreatedAfter: at(2), CreatedBefore: at(4)}, []string{"v4", "v3", "v2"}},
		{"completed range leaves out open tasks", storage.TaskFilter{CompletedAfter: at(20)}, []string{"v3", "v1"}},
		{"completed before", storage.TaskFilter{CompletedBefore: at(30)}, []string{"v5", "v3"}},
	}
	for _, tc := range filters {
		t.Run(tc.name, func(t *testing.T) {
			tc.f.Sort = storage.TaskSortCreated
			if diff := cmp.Diff(tc.want, idsOf(t, s, tc.f)); diff != "" {
				t.Errorf("ids (-want +got):\n%s", diff)
			}
		})
	}

	sorts := []struct {
		name string
		f    storage.TaskFilter
		want []string
	}{
		{"name", storage.TaskFilter{Sort: storage.TaskSortName}, []string{"v1", "v2", "v3", "v4", "v5"}},
		{"name desc", storage.TaskFilter{Sort: storage.TaskSortName, Order: storage.OrderDesc}, []string{"v5", "v4", "v3", "v2", "v1"}},
		{"size, ties by id", storage.TaskFilter{Sort: storage.TaskSortSize}, []string{"v4", "v2", "v5", "v1", "v3"}},
		{"size desc", storage.TaskFilter{Sort: storage.TaskSortSize, Order: storage.OrderDesc}, []string{"v3", "v1", "v5", "v2", "v4"}},
		{"created asc", storage.TaskFilter{Sort: storage.TaskSortCreated, Order: storage.OrderAsc}, []string{"v1", "v2", "v3", "v4", "v5"}},
		{"completed puts open tasks first", storage.TaskFilter{Sort: storage.TaskSortCompleted}, []string{"v2", "v4", "v5", "v3", "v1"}},
		{"completed desc", storage.TaskFilter{Sort: storage.TaskSortCompleted, Order: storage.OrderDesc}, []string{"v1", "v3", "v5", "v4", "v2"}},
	}
	for _, tc := range sorts {
		for _, limit := range []int{0, 2} { // one page, and paged by two
			t.Run(fmt.Sprintf("%s limit %d", tc.name, limit), func(t *testing.T) {
				tc.f.Limit = limit
				if diff := cmp.Diff(tc.want, idsOf(t, s, tc.f)); diff != "" {
					t.Errorf("ids (-want +got):\n%s", diff)
				}
			})
		}
	}

	t.Run("a cursor is only valid for its order", func(t *testing.T) {
		res, err := s.Tasks().List(t.Context(), storage.TaskFilter{Sort: storage.TaskSortName, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Tasks().List(t.Context(), storage.TaskFilter{Sort: storage.TaskSortName, Order: storage.OrderDesc, Cursor: res.NextCursor})
		if !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("err = %v, want ErrInvalidArgument", err)
		}
	})

	t.Run("total counts the whole match, not the page", func(t *testing.T) {
		res, err := s.Tasks().List(t.Context(), storage.TaskFilter{Engine: "http", Limit: 1, WithTotal: true})
		if err != nil {
			t.Fatal(err)
		}
		if res.Total != 3 || len(res.Tasks) != 1 {
			t.Errorf("total %d, page %d", res.Total, len(res.Tasks))
		}
		res, err = s.Tasks().List(t.Context(), storage.TaskFilter{Engine: "http", Limit: 1})
		if err != nil || res.Total != 0 {
			t.Errorf("total without WithTotal = %d, %v", res.Total, err)
		}
	})
}
