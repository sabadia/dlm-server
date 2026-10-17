package domain

import (
	"errors"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/settings"
)

func TestNewMainQueue(t *testing.T) {
	q := NewMainQueue(t0)
	if q.ID != MainQueueID || q.Name != MainQueueName || !q.IsDefault || q.State != QueueRunning || !q.CreatedAt.Equal(t0) {
		t.Fatalf("main queue = %+v", q)
	}
	if err := q.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := q.CanDelete(); !errors.Is(err, ErrConflict) {
		t.Fatalf("CanDelete = %v, want ErrConflict", err)
	}
}

func TestNewQueue(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		qname    string
		s        settings.Settings
		wantErr  bool
		wantName string
	}{
		{"valid", "q1", "Fast", settings.Settings{}, false, "Fast"},
		{"trims name", "q1", "  Fast  ", settings.Settings{}, false, "Fast"},
		{"max length", "q1", strings.Repeat("é", MaxQueueNameLength), settings.Settings{}, false, strings.Repeat("é", MaxQueueNameLength)},
		{"too long", "q1", strings.Repeat("é", MaxQueueNameLength+1), settings.Settings{}, true, ""},
		{"empty name", "q1", "   ", settings.Settings{}, true, ""},
		{"empty id", "", "Fast", settings.Settings{}, true, ""},
		{"queue setting ok", "q1", "Fast", settings.Settings{MaxConcurrentDownloads: settings.Some(2)}, false, "Fast"},
		{"checksum rejected", "q1", "Fast", settings.Settings{Checksum: settings.Some(settings.Checksum{Algorithm: settings.ChecksumMD5, Expected: "d41d8cd98f00b204e9800998ecf8427e"})}, true, ""},
		{"global-only rejected", "q1", "Fast", settings.Settings{MaxActiveDownloadsTotal: settings.Some(3)}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := NewQueue(tt.id, tt.qname, t0, tt.s)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidArgument) || q != nil {
					t.Fatalf("got %+v, %v; want ErrInvalidArgument", q, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if q.Name != tt.wantName || q.IsDefault || q.State != QueueRunning || !q.UpdatedAt.Equal(t0) {
				t.Fatalf("queue = %+v", q)
			}
			if err := q.CanDelete(); err != nil {
				t.Fatalf("CanDelete = %v", err)
			}
		})
	}
}

func TestQueueSettingsErrorUnwraps(t *testing.T) {
	q := NewMainQueue(t0)
	bad := settings.Settings{Checksum: settings.Some(settings.Checksum{Algorithm: settings.ChecksumMD5, Expected: "d41d8cd98f00b204e9800998ecf8427e"})}
	err := q.SetSettings(bad, t1)
	var ve *settings.ValidationError
	if !errors.Is(err, ErrInvalidArgument) || !errors.As(err, &ve) || ve.Level != settings.LevelQueue {
		t.Fatalf("err = %v", err)
	}
	if q.Settings.Checksum.IsSet() || !q.UpdatedAt.Equal(t0) {
		t.Fatal("queue changed on rejected settings")
	}
	good := settings.Settings{MaxConcurrentDownloads: settings.Some(4)}
	if err := q.SetSettings(good, t1); err != nil {
		t.Fatal(err)
	}
	if q.Settings.MaxConcurrentDownloads.Value() != 4 || !q.UpdatedAt.Equal(t1) {
		t.Fatalf("settings not applied: %+v", q)
	}
}

func TestQueueRename(t *testing.T) {
	q := NewMainQueue(t0)
	if err := q.Rename("  ", t1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
	if q.Name != MainQueueName || !q.UpdatedAt.Equal(t0) {
		t.Fatal("queue changed on rejected rename")
	}
	if err := q.Rename(" Default ", t1); err != nil {
		t.Fatal(err)
	}
	if q.Name != "Default" || !q.UpdatedAt.Equal(t1) {
		t.Fatalf("queue = %+v", q)
	}
}

func TestQueueStartStop(t *testing.T) {
	q := NewMainQueue(t0)
	q.Start(t1) // already running: no-op
	if !q.UpdatedAt.Equal(t0) {
		t.Fatal("idempotent Start must not touch UpdatedAt")
	}
	q.Stop(t1)
	if q.State != QueueStopped || !q.UpdatedAt.Equal(t1) {
		t.Fatalf("after Stop: %+v", q)
	}
	q.Start(t2)
	if q.State != QueueRunning || !q.UpdatedAt.Equal(t2) {
		t.Fatalf("after Start: %+v", q)
	}
}

func TestQueueValidateState(t *testing.T) {
	q := NewMainQueue(t0)
	q.State = "paused"
	if err := q.Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
}
