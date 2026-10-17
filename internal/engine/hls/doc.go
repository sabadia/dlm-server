// Package hls is the engine for HLS streams (playlists ending in .m3u8).
//
// # What it does
//
// Resolve and Run read the playlist at the source URL. A master playlist is
// reduced to one stream by Source.MediaHint ("best" by default, "worst", a
// height such as "720p", or a pattern matched against the stream URL and name)
// and, when that stream plays with a separate audio rendition, to its audio
// playlist as well. Every segment is then fetched by a pool of workers
// (settings maxConnections), decrypted when the playlist uses AES-128 (explicit
// or sequence-number IVs, key changes, METHOD=NONE) and stored in <path>.hls.
// fMP4 streams (EXT-X-MAP init sections) and byte-range segments are handled.
// SAMPLE-AES and other DRM are refused.
//
// When all segments are there, ffmpeg remuxes them without re-encoding into
// <path>.part, which is renamed to the destination; the container follows the
// destination's extension (.mp4 by default). A single MPEG-TS stream saved as
// .ts is simply concatenated and needs no ffmpeg.
//
// # Resume and expired links
//
// The set of finished segments is the engine state (a journal) and is written
// with every checkpoint. The playlist is read again on every run, so a task
// resumed with a refreshed link carries on with the new URLs; if a finished
// stream has another number or length of segments than when the download
// started, refresh.onMismatch decides (restart, or a link-expired error so the
// refresh layer can look for the right link). When a segment or key link
// expires during a run, the engine reloads the playlist once and retries the
// segment with the URL found there before reporting link_expired.
//
// # Live streams
//
// A playlist without ENDLIST is recorded: the playlist is reloaded about every
// half target duration and new segments are fetched until ENDLIST appears or
// nothing new has arrived for three target durations (at least 30 s). Pausing
// keeps what was recorded; there is no way yet to end a recording of a stream
// that never stops except letting it stall.
//
// # Progress
//
// Segment sizes are unknown until fetched, so Progress.Size is an estimate
// (bytes so far scaled to the total duration) while downloading, -1 for a live
// stream, and the size of the result when finished.
package hls
