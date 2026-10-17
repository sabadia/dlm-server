package bt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/netclient"
	"github.com/sabadia/dlm-server/internal/settings"
)

const loopback = "127.0.0.1"

func payload(n int, salt byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*13+i/251) ^ salt
	}
	return b
}

// content is the files of a test torrent by slash-separated path.
type content map[string][]byte

// swarm is a seeder plus a tracker pointing at it.
type swarm struct {
	t       *testing.T
	seeder  *torrent.Client
	tracker *httptest.Server
	mi      *metainfo.MetaInfo
	info    metainfo.Info
	name    string
	trkMu   sync.Mutex
	peers   []byte
}

func testClientConfig(t *testing.T, dir string) *torrent.ClientConfig {
	t.Helper()
	cc := torrent.NewDefaultClientConfig()
	cc.DataDir = dir
	cc.ListenPort = 0 // a free port: tests run several clients
	cc.ListenHost = func(string) string { return loopback }
	cc.DisableIPv6 = true
	cc.NoDHT = true
	cc.DisablePEX = true
	cc.NoDefaultPortForwarding = true
	cc.Seed = true
	cc.DefaultStorage = storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir: dir, UsePartFiles: boolOpt(false)})
	return cc
}

// newSwarm writes files below a temp directory, seeds them and starts a
// tracker that announces the seeder. single makes a one-file torrent out of
// the only entry of files.
func newSwarm(t *testing.T, name string, files content, private bool) *swarm {
	return newSwarmTweak(t, func(*torrent.ClientConfig) {}, name, files, private)
}

func newSwarmTweak(t *testing.T, tweak func(*torrent.ClientConfig), name string, files content, private bool) *swarm {
	t.Helper()
	seedDir := t.TempDir()
	root := filepath.Join(seedDir, name)
	if len(files) == 1 && files[""] != nil {
		if err := os.WriteFile(root, files[""], 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		for p, data := range files {
			full := filepath.Join(root, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	info := metainfo.Info{PieceLength: 64 << 10}
	if private {
		info.Private = boolPtr(true)
	}
	if err := info.BuildFromFilePath(root); err != nil {
		t.Fatal(err)
	}
	info.Name = name
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	s := &swarm{t: t, name: name, info: info}
	s.tracker = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.trkMu.Lock()
		peers := s.peers
		s.trkMu.Unlock()
		out, _ := bencode.Marshal(map[string]any{"interval": 1, "min interval": 1, "complete": 1, "incomplete": 0, "peers": peers}) //nolint:errcheck // plain data
		_, _ = w.Write(out)
	}))
	t.Cleanup(s.tracker.Close)
	s.mi = &metainfo.MetaInfo{InfoBytes: infoBytes, Announce: s.tracker.URL + "/announce"}

	cc := testClientConfig(t, seedDir)
	tweak(cc)
	s.seeder, err = torrent.NewClient(cc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.seeder.Close() })
	st, err := s.seeder.AddTorrent(s.mi)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-st.Complete().On():
	case <-time.After(20 * time.Second):
		t.Fatal("the seeder did not verify its data")
	}
	port := s.seeder.LocalPort()
	s.trkMu.Lock()
	s.peers = append(net.ParseIP(loopback).To4(), byte(port>>8), byte(port))
	s.trkMu.Unlock()
	return s
}

func boolPtr(b bool) *bool { return &b }

func boolOpt(b bool) g.Option[bool] { return g.Some(b) }

// uploaded is how many data bytes the seeder has sent so far.
func (s *swarm) uploaded() int64 {
	st := s.seeder.Stats()
	return st.BytesWrittenData.Int64()
}

// torrentFile is the .torrent as a data URL.
func (s *swarm) dataURL() string {
	var buf bytes.Buffer
	if err := s.mi.Write(&buf); err != nil {
		s.t.Fatal(err)
	}
	return "data:application/x-bittorrent;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// magnet names the torrent and points straight at the seeder.
func (s *swarm) magnet() string {
	return fmt.Sprintf("magnet:?xt=urn:btih:%s&dn=%s&x.pe=%s:%d&tr=%s", s.mi.HashInfoBytes().HexString(), s.name, loopback, s.seeder.LocalPort(), url.QueryEscape(s.tracker.URL+"/announce"))
}

// ---- engine under test ----

func testConfig() Config {
	return Config{ListenHost: loopback, DisableDHT: true, DisablePEX: true, DisablePortForwarding: true, MetadataTimeout: 10 * time.Second}
}

func newEngine(t *testing.T, cfg Config, opts ...Option) *Engine {
	t.Helper()
	clients := netclient.New(netclient.Options{})
	t.Cleanup(func() { _ = clients.Close() })
	all := append([]Option{WithCheckpointInterval(10 * time.Millisecond), WithStallReset(2 * time.Second)}, opts...)
	e := New(engine.Env{Clients: clients}, cfg, all...)
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func testSettings(mutate ...func(*settings.Settings)) settings.Settings {
	s := settings.Defaults("")
	s.Proxy = settings.Some(settings.Proxy{Mode: settings.ProxyNone})
	s.Torrent.SeedKeep = settings.Some(false)
	s.Torrent.SeedRatio = settings.Some(0.0)
	s.Torrent.SeedTime = settings.Some(settings.Duration(0))
	for _, m := range mutate {
		m(&s)
	}
	return s
}

func withSeeding(ratio float64, d time.Duration, keep bool) func(*settings.Settings) {
	return func(s *settings.Settings) {
		s.Torrent.SeedRatio = settings.Some(ratio)
		s.Torrent.SeedTime = settings.Some(settings.Duration(d))
		s.Torrent.SeedKeep = settings.Some(keep)
	}
}

type recorder struct {
	mu sync.Mutex
	cp engine.Checkpoint
	st []byte
	n  int
}

func (r *recorder) Checkpoint(_ context.Context, cp engine.Checkpoint) error {
	r.mu.Lock()
	r.cp, r.n = cp, r.n+1
	if cp.EngineState != nil {
		r.st = cp.EngineState
	}
	r.mu.Unlock()
	return nil
}

func (r *recorder) last() (engine.Checkpoint, []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cp, r.st
}

func spec(dir, src, name string, s settings.Settings, cp engine.Checkpointer) engine.Spec {
	return engine.Spec{TaskID: "t1", Source: domain.Source{CurrentURL: src}, Path: filepath.Join(dir, name), Settings: s, Checkpointer: cp}
}

func wantKind(t *testing.T, err error, kind engine.Kind) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a %v error, got nil", kind)
	}
	if got := engine.Classify(err); got != kind {
		t.Fatalf("Classify(%v) = %v, want %v", err, got, kind)
	}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runWithin runs the download with a deadline.
func runWithin(t *testing.T, e *Engine, sp engine.Spec, d time.Duration) (engine.Download, error) {
	t.Helper()
	dl, err := e.NewDownload(sp)
	if err != nil {
		t.Fatalf("NewDownload: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return dl, dl.Run(ctx)
}

func jsonOf(v any) ([]byte, error)      { return jsonMarshal(v) }
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
