package service

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/ratelimit"
	"github.com/sabadia/dlm-server/internal/settings"
)

// swarmStub is a torrent-like engine: its download never ends by itself and
// reports uploads and peers, like a seeding torrent.
type swarmStub struct{ uploaded atomic.Int64 }

func (*swarmStub) Name() string { return "swarm-stub" }
func (*swarmStub) Match(src domain.Source) bool {
	return strings.HasPrefix(src.CurrentURL, "swarm://")
}

func (*swarmStub) Resolve(_ context.Context, req engine.Request) (*engine.Probe, error) {
	return &engine.Probe{
		Engine: "swarm-stub", Source: req.Source, Suggested: "pack",
		Resource: domain.Resource{Name: "pack", Size: 1000},
	}, nil
}

func (s *swarmStub) NewDownload(engine.Spec) (engine.Download, error) {
	return &swarmDownload{stub: s}, nil
}

type swarmDownload struct{ stub *swarmStub }

func (d *swarmDownload) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// Progress reports 50 kB more uploaded with every call.
func (d *swarmDownload) Progress() engine.Progress {
	return engine.Progress{Downloaded: 1000, Size: 1000, Uploaded: d.stub.uploaded.Add(50_000), Peers: 3}
}

func (d *swarmDownload) Torrent() (*domain.TorrentInfo, bool) {
	return &domain.TorrentInfo{State: domain.TorrentSeeding, PeerCount: 3, InfoHash: "abc"}, true
}

func TestRunnerReportsUploadAndExposesTheRunningTorrent(t *testing.T) {
	f := newSvcFixture(t)
	reg, err := engine.NewRegistry(&swarmStub{})
	if err != nil {
		t.Fatal(err)
	}
	defaults := settings.Defaults(f.dir)
	runner := NewRunner(RunnerDeps{
		Store: f.store, Engines: reg, Limits: ratelimit.NewManager(0), Bus: f.bus,
		Defaults: func() settings.Settings { return defaults }, ProgressInterval: 10 * time.Millisecond,
		TaskLocks: f.tasks.d.TaskLocks,
	})
	d := f.tasks.d
	d.Engines, d.Runner = reg, runner
	tasks := NewTaskService(d)
	task, err := tasks.Create(t.Context(), CreateTaskInput{Source: domain.Source{CurrentURL: "swarm://x"}, AllowDuplicate: true})
	if err != nil {
		t.Fatal(err)
	}
	// Progress events are coalesced and delivered by the bus loop.
	go func() { _ = f.bus.Run(t.Context()) }()
	sub := f.bus.Subscribe(t.Context(), events.Filter{Types: []events.Type{events.TypeTaskProgress}})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, task.ID) }()

	var got ProgressData
	deadline := time.After(5 * time.Second)
	for got.UploadSpeed == 0 {
		select {
		case e := <-sub.C():
			pd, ok := e.Data.(ProgressData)
			if !ok {
				t.Fatalf("payload %T", e.Data)
			}
			got = pd
		case err := <-done:
			t.Fatalf("Run ended early: %v", err)
		case <-deadline:
			t.Fatalf("no progress event with an upload speed; last: %+v; task: %+v", got, f.get(t, task.ID))
		}
	}
	if got.Peers != 3 {
		t.Errorf("peers = %d, want 3", got.Peers)
	}

	info, err := tasks.Torrent(t.Context(), task.ID)
	if err != nil || info.State != domain.TorrentSeeding || info.InfoHash != "abc" {
		t.Fatalf("running torrent = %+v, %v", info, err)
	}
	if _, ok := runner.Running(task.ID); !ok {
		t.Error("Running does not know the running download")
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run after cancel: %v", err)
	}
	if _, ok := runner.Running(task.ID); ok {
		t.Error("Running still knows a download that ended")
	}
}
