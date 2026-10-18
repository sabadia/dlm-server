package service

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/postproc"
	"github.com/sabadia/dlm-server/internal/storage"
)

// torrentStub takes embedded .torrent sources; it never downloads.
type torrentStub struct{}

func (torrentStub) Name() string { return "stub-bt" }
func (torrentStub) Match(src domain.Source) bool {
	return strings.HasPrefix(src.CurrentURL, "data:application/x-bittorrent")
}

func (torrentStub) Resolve(context.Context, engine.Request) (*engine.Probe, error) {
	return nil, errors.New("not used")
}

func (torrentStub) NewDownload(engine.Spec) (engine.Download, error) {
	return nil, errors.New("not used")
}

func TestAddTorrentFileCreatesATaskFromTheFile(t *testing.T) {
	f := newSvcFixture(t)
	reg, err := engine.NewRegistry(torrentStub{})
	if err != nil {
		t.Fatal(err)
	}
	d := f.tasks.d
	d.Engines = reg
	tasks := NewTaskService(d)
	dir := t.TempDir()
	data := []byte("d4:infod4:name1:aee")

	if err := tasks.AddTorrentFile(t.Context(), postproc.NewTorrent{Data: data, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	page, err := f.store.Tasks().List(t.Context(), storageFilter())
	if err != nil || len(page.Tasks) != 1 {
		t.Fatalf("tasks = %v, %v", page.Tasks, err)
	}
	got := page.Tasks[0]
	want := "data:application/x-bittorrent;base64," + base64.StdEncoding.EncodeToString(data)
	if got.Source.CurrentURL != want || got.Dir != dir || got.Engine != "stub-bt" || got.QueueID != domain.MainQueueID {
		t.Errorf("task = %+v", got)
	}
	// The same torrent again is a duplicate, which is fine.
	if err := tasks.AddTorrentFile(t.Context(), postproc.NewTorrent{Data: data, Dir: dir}); err != nil {
		t.Errorf("a known torrent is not an error: %v", err)
	}
	if page, _ := f.store.Tasks().List(t.Context(), storageFilter()); len(page.Tasks) != 1 {
		t.Errorf("%d tasks after adding the same torrent twice", len(page.Tasks))
	}
	// Problems are reported: a relative directory, an unknown queue.
	if err := tasks.AddTorrentFile(t.Context(), postproc.NewTorrent{Data: []byte("other"), Dir: "relative"}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("relative dir: err = %v", err)
	}
	if err := tasks.AddTorrentFile(t.Context(), postproc.NewTorrent{Data: []byte("other"), Dir: dir, QueueID: "nope"}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown queue: err = %v", err)
	}
	// Without a torrent engine the action cannot work, and says why.
	if err := f.tasks.AddTorrentFile(t.Context(), postproc.NewTorrent{Data: data, Dir: dir}); err == nil {
		t.Error("want an error when no engine takes the torrent")
	}
}

func storageFilter() storage.TaskFilter { return storage.TaskFilter{Limit: storage.MaxTaskLimit} }
