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
			"unknown sort":     {Sort: "size"},
			"unknown status":   {Statuses: []domain.TaskStatus{"nope"}},
		} {
			if _, err := s.Tasks().List(t.Context(), f); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
			}
		}
	})
}
