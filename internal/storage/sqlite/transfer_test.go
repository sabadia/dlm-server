package sqlite_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/storage"
)

func TestTransferAddAndTimeline(t *testing.T) {
	s := openStore(t)
	day := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	h := func(d, hour int) time.Time { return day.AddDate(0, 0, d).Add(time.Duration(hour) * time.Hour) }

	rows := []storage.TransferRow{
		{Bucket: h(0, 10), QueueID: "main", Engine: "http", Down: 100, Up: 0},
		{Bucket: h(0, 10), QueueID: "main", Engine: "bt", Down: 50, Up: 5},
		{Bucket: h(0, 11), QueueID: "q1", Engine: "http", Down: 10},
		{Bucket: h(1, 3), QueueID: "main", Engine: "http", Down: 7},
	}
	if err := s.Transfer().Add(t.Context(), rows); err != nil {
		t.Fatal(err)
	}
	// Adding again to the same hour, queue and engine accumulates.
	if err := s.Transfer().Add(t.Context(), []storage.TransferRow{{Bucket: h(0, 10), QueueID: "main", Engine: "http", Down: 1, Up: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Transfer().Add(t.Context(), nil); err != nil {
		t.Fatal(err)
	}

	from, to := h(0, 0), h(2, 0)
	tests := []struct {
		name   string
		size   time.Duration
		group  storage.TransferGroup
		want   []storage.TransferPoint
		wantOK bool
	}{
		{"hours", time.Hour, storage.TransferGroupNone, []storage.TransferPoint{
			{Bucket: h(0, 10), Down: 151, Up: 7}, {Bucket: h(0, 11), Down: 10}, {Bucket: h(1, 3), Down: 7},
		}, true},
		{"days", 24 * time.Hour, storage.TransferGroupNone, []storage.TransferPoint{
			{Bucket: h(0, 0), Down: 161, Up: 7}, {Bucket: h(1, 0), Down: 7},
		}, true},
		{"hours by engine", time.Hour, storage.TransferGroupEngine, []storage.TransferPoint{
			{Bucket: h(0, 10), Group: "bt", Down: 50, Up: 5},
			{Bucket: h(0, 10), Group: "http", Down: 101, Up: 2},
			{Bucket: h(0, 11), Group: "http", Down: 10},
			{Bucket: h(1, 3), Group: "http", Down: 7},
		}, true},
		{"days by queue", 24 * time.Hour, storage.TransferGroupQueue, []storage.TransferPoint{
			{Bucket: h(0, 0), Group: "main", Down: 151, Up: 7}, {Bucket: h(0, 0), Group: "q1", Down: 10}, {Bucket: h(1, 0), Group: "main", Down: 7},
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Transfer().Timeline(t.Context(), from, to, tc.size, tc.group)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("points (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("the range is half open", func(t *testing.T) {
		got, err := s.Transfer().Timeline(t.Context(), h(0, 11), h(1, 3), time.Hour, storage.TransferGroupNone)
		if err != nil || len(got) != 1 || got[0].Bucket != h(0, 11) {
			t.Errorf("points = %+v, %v", got, err)
		}
	})
	t.Run("bad arguments", func(t *testing.T) {
		if _, err := s.Transfer().Timeline(t.Context(), from, to, 90*time.Minute, storage.TransferGroupNone); err == nil {
			t.Error("a bucket of 90 minutes was accepted")
		}
		if _, err := s.Transfer().Timeline(t.Context(), from, to, time.Hour, "queue-ish"); err == nil {
			t.Error("an unknown grouping was accepted")
		}
	})
}
