package bt

import (
	"encoding/hex"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/settings"
)

// liveTorrent is what a running download tells about its torrent.
type liveTorrent struct {
	t       *torrent.Torrent
	hash    metainfo.Hash
	name    string
	private bool
	// trackers are the announce URLs of the torrent file and the settings.
	trackers []string
	// baseUploaded and baseSeedMillis are what earlier runs shared.
	baseUploaded   int64
	baseSeedMillis int64
	// size is the number of bytes of the selected files.
	size int64
	// seedingSince is the start of seeding in Unix nanoseconds, 0 before.
	seedingSince atomic.Int64
}

var (
	_ engine.Inspectable = (*Download)(nil)
	_ engine.Describer   = (*Engine)(nil)
)

// Torrent implements engine.Inspectable.
func (d *Download) Torrent() (*domain.TorrentInfo, bool) {
	lt := d.live.Load()
	if lt == nil {
		return nil, false
	}
	stats := lt.t.Stats()
	uploaded := lt.baseUploaded + stats.BytesWrittenData.Int64()
	seedMillis := lt.baseSeedMillis
	state := domain.TorrentDownloading
	switch {
	case lt.t.Info() == nil:
		state = domain.TorrentMetadata
	case lt.seedingSince.Load() != 0:
		state = domain.TorrentSeeding
		seedMillis += d.e.env.Clock().Sub(time.Unix(0, lt.seedingSince.Load())).Milliseconds()
	}
	info := &domain.TorrentInfo{
		InfoHash: hex.EncodeToString(lt.hash[:]), Name: lt.name, Private: lt.private, State: state,
		Downloaded: d.downloaded.Load(), Uploaded: uploaded, Ratio: ratio(uploaded, lt.size),
		SeedingSeconds: seedMillis / 1000, Trackers: lt.trackers,
		PeerCount: stats.ActivePeers, KnownPeers: stats.TotalPeers, Seeders: stats.ConnectedSeeders,
	}
	for _, pc := range lt.t.PeerConns() {
		if len(info.Peers) >= domain.MaxListedPeers {
			break
		}
		p := domain.TorrentPeer{DownloadRate: int64(pc.Stats().DownloadRate)}
		if pc.RemoteAddr != nil {
			p.Address = pc.RemoteAddr.String()
		}
		if v, ok := pc.PeerClientName.Load().(string); ok {
			p.Client = v
		}
		info.Peers = append(info.Peers, p)
	}
	return info, true
}

// Describe implements engine.Describer: a stopped task shows what its last
// run saved.
func (e *Engine) Describe(stateBytes []byte, res *domain.Resource, set settings.Settings) (*domain.TorrentInfo, bool) {
	st, ok := decodeState(stateBytes)
	if !ok {
		return nil, false
	}
	var size int64
	if res != nil {
		size = res.SelectedSize()
	}
	info := &domain.TorrentInfo{
		State: domain.TorrentStopped, Uploaded: st.Uploaded, Ratio: ratio(st.Uploaded, size),
		SeedingSeconds: st.SeedMillis / 1000,
	}
	if len(st.Metainfo) > 0 {
		if mi, err := metainfo.Load(strings.NewReader(string(st.Metainfo))); err == nil {
			hash := mi.HashInfoBytes()
			info.InfoHash = hex.EncodeToString(hash[:])
			info.Trackers = trackersOf(mi, set.Torrent)
			if ti, err := mi.UnmarshalInfo(); err == nil {
				info.Name = ti.BestName()
				info.Private = ti.Private != nil && *ti.Private
			}
		}
	}
	return info, true
}

// trackersOf lists the announce URLs of a torrent file followed by the extra
// trackers of the settings (which private torrents ignore).
func trackersOf(mi *metainfo.MetaInfo, t settings.TorrentSettings) []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	if mi == nil {
		return out
	}
	for _, tier := range mi.UpvertedAnnounceList() {
		for _, u := range tier {
			add(u)
		}
	}
	private := false
	if ti, err := mi.UnmarshalInfo(); err == nil && ti.Private != nil {
		private = *ti.Private
	}
	if !private {
		for _, u := range t.Trackers.OrElse(nil) {
			add(u)
		}
	}
	return out
}

// ratio is uploaded over size, 0 while the size is unknown.
func ratio(uploaded, size int64) float64 {
	if size <= 0 {
		return 0
	}
	return float64(uploaded) / float64(size)
}
