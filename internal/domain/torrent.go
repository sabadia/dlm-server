package domain

// TorrentState says what a torrent task is doing.
type TorrentState string

// Torrent states.
const (
	// TorrentMetadata: the file list is still being fetched from peers.
	TorrentMetadata TorrentState = "metadata"
	// TorrentDownloading: the selected files are being downloaded.
	TorrentDownloading TorrentState = "downloading"
	// TorrentSeeding: the selected files are complete and being uploaded.
	TorrentSeeding TorrentState = "seeding"
	// TorrentStopped: the task is not running; the numbers are those saved
	// by the last run.
	TorrentStopped TorrentState = "stopped"
)

// TorrentInfo describes a torrent task beyond its progress: what it is, how
// the swarm looks and how much has been shared.
type TorrentInfo struct {
	// InfoHash is the hex info hash (the v1 hash for hybrid torrents).
	InfoHash string `json:"infoHash,omitempty"`
	Name     string `json:"name,omitempty"`
	// Private torrents only use the trackers of their own file.
	Private bool         `json:"private"`
	State   TorrentState `json:"state"`
	// Downloaded and Uploaded are bytes transferred; Uploaded includes what
	// earlier runs of the task uploaded.
	Downloaded int64 `json:"downloaded"`
	Uploaded   int64 `json:"uploaded"`
	// Ratio is Uploaded divided by the selected size; 0 while unknown.
	Ratio float64 `json:"ratio"`
	// SeedingSeconds is the time spent seeding, over all runs.
	SeedingSeconds int64 `json:"seedingSeconds"`
	// Trackers are the announce URLs of the torrent and of the settings.
	Trackers []string `json:"trackers,omitempty"`
	// PeerCount is the number of connected peers, KnownPeers the number the
	// client has heard of, Seeders how many connected peers have every piece.
	PeerCount  int `json:"peerCount"`
	KnownPeers int `json:"knownPeers"`
	Seeders    int `json:"seeders"`
	// Peers lists the connected peers (at most MaxListedPeers).
	Peers []TorrentPeer `json:"peers,omitempty"`
}

// MaxListedPeers bounds TorrentInfo.Peers.
const MaxListedPeers = 100

// TorrentPeer is one connected peer.
type TorrentPeer struct {
	Address string `json:"address"`
	// Client is the peer's client name and version, when it said so.
	Client string `json:"client,omitempty"`
	// DownloadRate is bytes per second received from the peer.
	DownloadRate int64 `json:"downloadRate"`
}
