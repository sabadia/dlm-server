// Package bt downloads torrents: magnet links, .torrent files given by URL,
// and .torrent files embedded in the source as a data URL
// ("data:application/x-bittorrent;base64,<data>").
//
// One BitTorrent client (anacrolix/torrent) is owned by the Engine and shared
// by all torrent tasks; it is created when the first torrent needs it and
// closed by Engine.Close. Peer traffic uses the engine's own sockets: the
// proxy settings only apply to fetching a .torrent from a URL.
//
// Resolve reads the torrent's metadata (a magnet link has to fetch it from
// peers first) and lists its files. A download writes into the final location
// right away: a single-file torrent is the task's path, a multi-file torrent
// the directory at the task's path. Files that are not finished carry a .part
// suffix, so a restart trusts the finished files without hashing them and
// verifies only what is partial. Files with Selected=false in the task's
// resource are skipped.
//
// When the selected files are complete the task seeds according to the
// torrent settings (seedKeep, seedRatio, seedTime); it stays running while it
// seeds, and pausing it stops seeding. The uploaded bytes and the seeding
// time are kept in the engine state, so the limits hold across restarts.
//
// The rate limiter of the task bounds the download by slowing down writes;
// the upload of all torrents together is bounded by Config.UploadLimit.
package bt
