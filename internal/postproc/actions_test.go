package postproc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

type actionFunc func(ctx context.Context, j *Job, params map[string]string) error

func (f actionFunc) Run(ctx context.Context, j *Job, params map[string]string) error {
	return f(ctx, j, params)
}

func jobWith(actions ...settings.PostAction) *Job {
	j := &Job{Task: domain.Task{ID: "t", QueueID: "q"}, Path: "/dl/f.bin"}
	j.Settings.PostActions = settings.Some(actions)
	return j
}

func TestActionsRunInOrderAndFailuresOnlyBecomeNotes(t *testing.T) {
	var ran []string
	mk := func(name string, err error) Action {
		return actionFunc(func(_ context.Context, _ *Job, p map[string]string) error {
			ran = append(ran, name+":"+p["k"])
			return err
		})
	}
	a := NewActions(nil, map[string]Action{"one": mk("one", nil), "bad": mk("bad", errors.New("it broke")), "two": mk("two", nil)})
	j := jobWith(
		settings.PostAction{Kind: "one", Params: map[string]string{"k": "1"}},
		settings.PostAction{Kind: "bad", Params: map[string]string{"k": "2"}},
		settings.PostAction{Kind: "nope"},
		settings.PostAction{Kind: "two", Params: map[string]string{"k": "3"}},
	)
	if err := a.Run(t.Context(), j); err != nil {
		t.Fatalf("a failing action must not fail the step: %v", err)
	}
	if strings.Join(ran, ",") != "one:1,bad:2,two:3" {
		t.Errorf("ran %v", ran)
	}
	if len(j.Notes) != 2 || !strings.Contains(j.Notes[0], "bad: it broke") || !strings.Contains(j.Notes[1], "nope: unknown") {
		t.Errorf("notes = %v", j.Notes)
	}
	// Nothing configured, nothing done.
	if err := a.Run(t.Context(), &Job{}); err != nil {
		t.Fatal(err)
	}
	// A cancelled context stops the actions, and the stop is not turned into a note.
	ctx, cancel := context.WithCancel(t.Context())
	stopper := NewActions(nil, map[string]Action{"x": actionFunc(func(ctx context.Context, _ *Job, _ map[string]string) error {
		cancel()
		return ctx.Err() //nolint:wrapcheck // a test double
	})})
	j = jobWith(settings.PostAction{Kind: "x"}, settings.PostAction{Kind: "x"})
	if err := stopper.Run(ctx, j); !errors.Is(err, context.Canceled) || len(j.Notes) != 0 {
		t.Errorf("err = %v notes %v", err, j.Notes)
	}
}

type adderRecorder struct {
	got []NewTorrent
	err error
}

func (a *adderRecorder) AddTorrent(_ context.Context, t NewTorrent) error {
	a.got = append(a.got, t)
	return a.err
}

func TestAddTorrentAction(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "x.torrent", []byte("d4:infod4:name1:aee"))
	write(t, dir, "x.txt", []byte("not a torrent"))

	// Only .torrent files, whatever the case of the extension.
	rec := &adderRecorder{}
	act := AddTorrent{Adder: rec}
	for _, name := range []string{"x.txt", "missing"} {
		if err := act.Run(t.Context(), jobFor(dir, name), nil); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(rec.got) != 0 {
		t.Fatalf("a task was made for a file that is no torrent: %+v", rec.got)
	}
	write(t, dir, "UPPER.TORRENT", []byte("d4:infod4:name1:bee"))
	j := jobFor(dir, "x.torrent")
	if err := act.Run(t.Context(), j, nil); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 1 || string(rec.got[0].Data) != "d4:infod4:name1:aee" || rec.got[0].Dir != dir || rec.got[0].QueueID != "" {
		t.Fatalf("added %+v", rec.got)
	}
	if len(j.Notes) != 1 || !strings.Contains(j.Notes[0], "x.torrent") {
		t.Errorf("notes = %v", j.Notes)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.torrent")); err != nil {
		t.Error("the .torrent file was removed without delete")
	}
	if err := act.Run(t.Context(), jobFor(dir, "UPPER.TORRENT"), map[string]string{"dir": "/else/where", "delete": "true"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 2 || rec.got[1].Dir != "/else/where" {
		t.Errorf("added %+v", rec.got)
	}
	if _, err := os.Stat(filepath.Join(dir, "UPPER.TORRENT")); !os.IsNotExist(err) {
		t.Error("delete did not remove the .torrent file")
	}

	// A failing adder leaves the file in place and reports.
	failing := AddTorrent{Adder: &adderRecorder{err: errors.New("no engine")}}
	if err := failing.Run(t.Context(), jobFor(dir, "x.torrent"), map[string]string{"delete": "true"}); err == nil {
		t.Error("want the adder's error")
	}
	if _, err := os.Stat(filepath.Join(dir, "x.torrent")); err != nil {
		t.Error("the file was deleted although no task was made")
	}
	if err := (AddTorrent{}).Run(t.Context(), jobFor(dir, "x.torrent"), nil); err == nil {
		t.Error("without an adder the action must say so")
	}
	// An oversized file is refused.
	big := filepath.Join(dir, "big.torrent")
	if err := os.WriteFile(big, make([]byte, maxTorrentFile+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := act.Run(t.Context(), jobFor(dir, "big.torrent"), nil); err == nil {
		t.Error("an oversized .torrent was accepted")
	}
}

func TestDefaultPipelineRunsActionsAfterTheChecksum(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "pack.zip", zipOf(t, sample))
	rec := &adderRecorder{}
	p := Default(Options{Torrents: rec})
	tk := &domain.Task{ID: "t", Dir: dir, Name: "pack.zip"}
	var eff settings.Settings
	eff.PostActions = settings.Some([]settings.PostAction{{Kind: settings.PostActionExtract}})
	eff.Checksum = settings.Some(settings.Checksum{Algorithm: settings.ChecksumMD5, Expected: strings.Repeat("0", 32)})
	// A mismatch stops the pipeline before anything is unpacked.
	if _, err := p.Process(t.Context(), tk, eff); err == nil {
		t.Fatal("want a checksum mismatch")
	}
	if _, err := os.Stat(filepath.Join(dir, "pack")); err == nil {
		t.Fatal("the archive was unpacked although its checksum was wrong")
	}
	eff.Checksum = settings.Optional[settings.Checksum]{}
	j, err := p.Process(t.Context(), tk, eff)
	if err != nil || len(j.Notes) != 1 {
		t.Fatalf("err %v notes %v", err, j.Notes)
	}
	if readFile(t, filepath.Join(dir, "pack", "top.txt")) != "top" {
		t.Error("not unpacked")
	}
}
