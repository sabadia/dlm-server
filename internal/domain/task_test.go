package domain

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/sabadia/dlm-server/internal/settings"
)

func TestNewTaskDefaults(t *testing.T) {
	labels := map[string]string{"k": "v"}
	task, err := NewTask("id1", t0, NewTaskParams{
		Source: Source{CurrentURL: "https://example.com/a.bin"},
		Labels: labels,
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		ok   bool
	}{
		{"queue defaults to main", task.QueueID == MainQueueID},
		{"status queued", task.Status == StatusQueued},
		{"priority normal", task.Priority == PriorityNormal},
		{"size unknown", task.Size == -1},
		{"originalUrl defaulted", task.Source.OriginalURL == "https://example.com/a.bin"},
		{"method defaulted", task.Source.Method == http.MethodGet},
		{"resolver defaulted", task.Source.ResolverKind == ResolverDirect},
		{"createdAt", task.CreatedAt.Equal(t0) && task.UpdatedAt.Equal(t0)},
		{"no start/complete", task.StartedAt == nil && task.CompletedAt == nil},
		{"progress unknown", task.Progress() == -1},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s: failed (task %+v)", c.name, task)
		}
	}
	labels["k"] = "changed"
	if task.Labels["k"] != "v" {
		t.Error("labels must be copied")
	}
}

func TestNewTaskKeepsExplicitValues(t *testing.T) {
	task, err := NewTask("id1", t0, NewTaskParams{
		QueueID:  "fast",
		Name:     "a.bin",
		Engine:   "http",
		Priority: PriorityHigh,
		Source:   Source{CurrentURL: "https://cdn.example.com/a", OriginalURL: "https://example.com/a", Method: http.MethodPost},
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.QueueID != "fast" || task.Priority != PriorityHigh || task.Source.OriginalURL != "https://example.com/a" || task.Source.Method != http.MethodPost {
		t.Fatalf("explicit values not kept: %+v", task)
	}
}

func TestNewTaskValidation(t *testing.T) {
	src := Source{CurrentURL: "https://example.com/a"}
	tests := []struct {
		name string
		id   string
		p    NewTaskParams
	}{
		{"empty id", "", NewTaskParams{Source: src}},
		{"missing url", "x", NewTaskParams{}},
		{"relative url", "x", NewTaskParams{Source: Source{CurrentURL: "/a/b"}}},
		{"bad priority", "x", NewTaskParams{Source: src, Priority: 5}},
		{"empty label key", "x", NewTaskParams{Source: src, Labels: map[string]string{" ": "v"}}},
		{"queue-only setting", "x", NewTaskParams{Source: src, Settings: settings.Settings{MaxConcurrentDownloads: settings.Some(2)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewTask(tt.id, t0, tt.p)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestTaskValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Task)
		wantErr bool
	}{
		{"valid", func(*Task) {}, false},
		{"size known, partial", func(tk *Task) { tk.Size = 10; tk.Downloaded = 5 }, false},
		{"size known, full", func(tk *Task) { tk.Size = 10; tk.Downloaded = 10 }, false},
		{"size unknown, any downloaded", func(tk *Task) { tk.Downloaded = 1 << 40 }, false},
		{"size < -1", func(tk *Task) { tk.Size = -2 }, true},
		{"negative downloaded", func(tk *Task) { tk.Downloaded = -1 }, true},
		{"downloaded > size", func(tk *Task) { tk.Size = 10; tk.Downloaded = 11 }, true},
		{"empty queue", func(tk *Task) { tk.QueueID = "" }, true},
		{"unknown status", func(tk *Task) { tk.Status = "nope" }, true},
		{"task checksum allowed", func(tk *Task) {
			tk.Settings.Checksum = settings.Some(settings.Checksum{Algorithm: settings.ChecksumMD5, Expected: "d41d8cd98f00b204e9800998ecf8427e"})
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := newTestTask(t, StatusQueued)
			tt.mutate(task)
			err := task.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestTaskSettingsErrorUnwraps(t *testing.T) {
	task := newTestTask(t, StatusQueued)
	task.Settings.MaxConcurrentDownloads = settings.Some(3)
	err := task.Validate()
	var ve *settings.ValidationError
	if !errors.Is(err, ErrInvalidArgument) || !errors.As(err, &ve) {
		t.Fatalf("err = %v, want ErrInvalidArgument and *settings.ValidationError", err)
	}
	if ve.Level != settings.LevelTask || len(ve.Fields) != 1 || ve.Fields[0].Path != "maxConcurrentDownloads" {
		t.Fatalf("validation error = %+v", ve)
	}
}

func TestProgress(t *testing.T) {
	tests := []struct {
		size, done int64
		status     TaskStatus
		want       float64
	}{
		{-1, 100, StatusRunning, -1},
		{0, 0, StatusRunning, 0},
		{0, 0, StatusCompleted, 1},
		{100, 0, StatusRunning, 0},
		{100, 25, StatusRunning, 0.25},
		{100, 100, StatusRunning, 1},
	}
	for _, tt := range tests {
		task := &Task{Size: tt.size, Downloaded: tt.done, Status: tt.status}
		if got := task.Progress(); got != tt.want {
			t.Errorf("Progress(size=%d, done=%d, %s) = %v, want %v", tt.size, tt.done, tt.status, got, tt.want)
		}
	}
}

func TestPriorityJSON(t *testing.T) {
	tests := []struct {
		p    Priority
		json string
	}{
		{PriorityLow, `"low"`},
		{PriorityNormal, `"normal"`},
		{PriorityHigh, `"high"`},
	}
	for _, tt := range tests {
		t.Run(tt.p.String(), func(t *testing.T) {
			b, err := json.Marshal(tt.p)
			if err != nil || string(b) != tt.json {
				t.Fatalf("Marshal = %s, %v; want %s", b, err, tt.json)
			}
			var got Priority
			if err := json.Unmarshal(b, &got); err != nil || got != tt.p {
				t.Fatalf("Unmarshal = %v, %v", got, err)
			}
		})
	}
	if _, err := json.Marshal(Priority(7)); err == nil {
		t.Error("marshal of invalid priority must fail")
	}
	var p Priority
	if err := json.Unmarshal([]byte(`"urgent"`), &p); err == nil {
		t.Error("unmarshal of unknown priority must fail")
	}
}

func TestTaskJSONRoundTrip(t *testing.T) {
	task := newTestTask(t, StatusQueued)
	task.Priority = PriorityLow
	task.Labels = map[string]string{"a": "b"}
	task.Source.Body = []byte{0, 1, 2}
	if err := task.TransitionTo(StatusRunning, t1); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "queueId", "status", "priority", "createdAt", "updatedAt", "startedAt", "source"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing JSON key %q in %s", k, b)
		}
	}
	if raw["priority"] != "low" || raw["status"] != "running" {
		t.Errorf("priority=%v status=%v", raw["priority"], raw["status"])
	}
	var got Task
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != task.ID || got.Priority != task.Priority || got.Status != task.Status || string(got.Source.Body) != string(task.Source.Body) || !got.StartedAt.Equal(*task.StartedAt) {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}
