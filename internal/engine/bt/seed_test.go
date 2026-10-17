package bt

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/anacrolix/torrent"

	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/ratelimit"
)

// enginePort is the port the engine's client listens on.
func enginePort(t *testing.T, e *Engine) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		cl := e.client
		e.mu.Unlock()
		if cl != nil {
			return cl.LocalPort()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the engine did not start a client")
	return 0
}

// leech downloads the torrent from the engine only (no tracker, no other
// peer) and returns when it has everything.
func leech(t *testing.T, sw *swarm, port int) {
	t.Helper()
	mi := *sw.mi
	mi.Announce, mi.AnnounceList = "", nil
	dir := t.TempDir()
	cc := testClientConfig(t, dir)
	cl, err := torrent.NewClient(cc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	tor, err := cl.AddTorrent(&mi)
	if err != nil {
		t.Fatal(err)
	}
	peer := []torrent.PeerInfo{{Addr: ipPort{net.ParseIP(loopback), port}, Trusted: true}}
	tor.AddPeers(peer)
	tor.DownloadAll()
	// The library sometimes leaves a connection stuck on loopback; a new one
	// gets going (the engine does the same, see WithStallReset).
	deadline := time.After(40 * time.Second)
	for {
		select {
		case <-tor.Complete().On():
			return
		case <-time.After(3 * time.Second):
			for _, pc := range tor.PeerConns() {
				pc.Close()
			}
			tor.AddPeers(peer)
		case <-deadline:
			t.Fatal("the leecher did not get the data from the engine")
		}
	}
}

type ipPort struct {
	ip   net.IP
	port int
}

func (a ipPort) String() string { return net.JoinHostPort(a.ip.String(), strconv.Itoa(a.port)) }

// waitComplete waits until the recorder saw the engine state with Complete.
func waitComplete(t *testing.T, rec *recorder) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, blob := rec.last(); blob != nil {
			if st, ok := decodeState(blob); ok && st.Complete {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the download did not complete")
}

func TestFinishedFilesAreWritable(t *testing.T) {
	sw := newSwarm(t, "pack", multi, false)
	dir := t.TempDir()
	if _, err := runWithin(t, newEngine(t, testConfig()), spec(dir, sw.dataURL(), "pack", testSettings(), nil), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"a.bin", "sub/b.bin", "sub/deep/c.txt"} {
		fi, err := os.Stat(filepath.Join(dir, "pack", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o200 == 0 {
			t.Errorf("%s is read-only (%v)", rel, fi.Mode())
		}
	}
}

func TestNoSeedingWithoutRules(t *testing.T) {
	sw := newSwarm(t, "single.bin", content{"": payload(200_000, 20)}, false)
	start := time.Now()
	if _, err := runWithin(t, newEngine(t, testConfig()), spec(t.TempDir(), sw.dataURL(), "single.bin", testSettings(), nil), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %v: it seemed to seed", time.Since(start))
	}
}

func TestSeedsUntilTheRatioIsReached(t *testing.T) {
	data := payload(300_000, 21)
	sw := newSwarm(t, "single.bin", content{"": data}, false)
	e := newEngine(t, testConfig())
	rec := &recorder{}
	sp := spec(t.TempDir(), sw.dataURL(), "single.bin", testSettings(withSeeding(1.0, 0, false)), rec)
	dl, err := e.NewDownload(sp)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- dl.Run(t.Context()) }()
	waitComplete(t, rec)
	select {
	case err := <-done:
		t.Fatalf("Run returned (%v) before anyone downloaded from the seed", err)
	case <-time.After(300 * time.Millisecond):
	}
	leech(t, sw, enginePort(t, e))
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("seeding did not stop at the ratio")
	}
}

func TestSeedsForTheConfiguredTime(t *testing.T) {
	sw := newSwarm(t, "single.bin", content{"": payload(100_000, 22)}, false)
	e := newEngine(t, testConfig())
	rec := &recorder{}
	sp := spec(t.TempDir(), sw.dataURL(), "single.bin", testSettings(withSeeding(0, 800*time.Millisecond, false)), rec)
	start := time.Now()
	dl, err := e.NewDownload(sp)
	if err != nil {
		t.Fatal(err)
	}
	if err := dl.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The download takes a moment; seeding then lasts the configured time.
	if el := time.Since(start); el < 800*time.Millisecond || el > 15*time.Second {
		t.Errorf("Run lasted %v", el)
	}
}

func TestKeepSeedingUntilPausedAndSeedTimeCarriesOver(t *testing.T) {
	data := payload(100_000, 23)
	sw := newSwarm(t, "single.bin", content{"": data}, false)
	e := newEngine(t, testConfig())
	dir := t.TempDir()
	rec := &recorder{}
	keep := spec(dir, sw.dataURL(), "single.bin", testSettings(withSeeding(0, 0, true)), rec)
	dl, err := e.NewDownload(keep)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- dl.Run(ctx) }()
	waitComplete(t, rec)
	select {
	case err := <-done:
		t.Fatalf("Run returned (%v) although the task keeps seeding", err)
	case <-time.After(700 * time.Millisecond):
	}
	cancel()
	if err := <-done; !isCanceled(err) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	_, blob := rec.last()
	st, ok := decodeState(blob)
	if !ok || !st.Complete || st.SeedMillis < 500 {
		t.Fatalf("state after pausing = %+v (decoded %v), want >= 500ms of seeding", st, ok)
	}

	// The same task, now limited to one second of seeding: the time already
	// seeded counts.
	limited := keep
	limited.Settings = testSettings(withSeeding(0, time.Second, false))
	limited.EngineState = blob
	start := time.Now()
	dl2, err := e.NewDownload(limited)
	if err != nil {
		t.Fatal(err)
	}
	if err := dl2.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 900*time.Millisecond {
		t.Errorf("the resumed run seeded for %v although %dms were already seeded", el, st.SeedMillis)
	}
	if !bytes.Equal(readFile(t, filepath.Join(dir, "single.bin")), data) {
		t.Error("content differs")
	}
}

func isCanceled(err error) bool { return err != nil && err.Error() == context.Canceled.Error() }

func TestFinishedFilesAreTrustedOnResume(t *testing.T) {
	data := payload(200_000, 24)
	sw := newSwarm(t, "single.bin", content{"": data}, false)
	e := newEngine(t, testConfig())
	dir := t.TempDir()
	rec := &recorder{}
	if _, err := runWithin(t, e, spec(dir, sw.dataURL(), "single.bin", testSettings(), rec), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	_, blob := rec.last()
	// Nobody serves the torrent any more, yet a run with the saved state
	// finds the finished file in place and completes.
	sw.seeder.Close()
	sp := spec(dir, "magnet:?xt=urn:btih:"+sw.mi.HashInfoBytes().HexString(), "single.bin", testSettings(), nil)
	sp.EngineState = blob
	dl, err := runWithin(t, newEngine(t, testConfig()), sp, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if p := dl.Progress(); p.Downloaded != int64(len(data)) {
		t.Errorf("progress = %+v", p)
	}
}

func TestResumeKeepsWhatWasDownloaded(t *testing.T) {
	data := payload(1_200_000, 25)
	sw := newSwarm(t, "single.bin", content{"": data}, false)
	e := newEngine(t, testConfig())
	dir := t.TempDir()
	rec := &recorder{}
	first := spec(dir, sw.dataURL(), "single.bin", testSettings(), rec)
	first.Limiter = ratelimit.NewBucket(250_000)
	dl, err := e.NewDownload(first)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- dl.Run(ctx) }()
	deadline := time.Now().Add(30 * time.Second)
	for verifiedPieces(e) < 7 {
		if time.Now().After(deadline) {
			t.Fatalf("no progress: %+v", dl.Progress())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; !isCanceled(err) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	cp, blob := rec.last()
	if cp.Downloaded < 6*(64<<10) || cp.Downloaded >= int64(len(data)) {
		t.Fatalf("checkpoint = %+v", cp)
	}

	// A new engine (a restarted daemon) continues. Verified pieces are kept
	// (the pieces that were only partly written are fetched again), so the
	// seeder is not asked for everything.
	before := sw.uploaded()
	second := spec(dir, sw.dataURL(), "single.bin", testSettings(), nil)
	second.EngineState = blob
	if _, err := runWithin(t, newEngine(t, testConfig()), second, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, filepath.Join(dir, "single.bin")), data) {
		t.Fatal("content differs after resuming")
	}
	if fetched := sw.uploaded() - before; fetched > int64(len(data))-4*(64<<10) {
		t.Errorf("the resumed run fetched %d of %d bytes: nothing was kept", fetched, len(data))
	}
}

func TestTheTaskLimiterSlowsTheDownload(t *testing.T) {
	sw := newSwarm(t, "single.bin", content{"": payload(900_000, 26)}, false)
	sp := spec(t.TempDir(), sw.dataURL(), "single.bin", testSettings(), nil)
	sp.Limiter = ratelimit.NewBucket(200_000)
	start := time.Now()
	if _, err := runWithin(t, newEngine(t, testConfig()), sp, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 1500*time.Millisecond {
		t.Errorf("900 kB at 200 kB/s took only %v", el)
	}
}

var _ engine.Limiter = (*ratelimit.Bucket)(nil)

// verifiedPieces counts the pieces the engine's torrents have verified.
func verifiedPieces(e *Engine) int {
	e.mu.Lock()
	cl := e.client
	e.mu.Unlock()
	if cl == nil {
		return 0
	}
	n := 0
	for _, t := range cl.Torrents() {
		stats := t.Stats()
		n += stats.PiecesComplete
	}
	return n
}
