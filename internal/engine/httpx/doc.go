// Package httpx implements the HTTP(S) engine ("http"): probing, filename
// detection, error classification and the segmented downloader.
//
// # Engine
//
// Engine.Resolve probes the URL (Probe) and keeps the probe response open as
// the engine.Probe attachment. Engine.NewDownload validates the Spec without
// doing I/O and takes that response over, so the first connection continues
// reading it from offset 0 instead of issuing a second request: one-time
// URLs keep working. When neither Spec.Resource nor Spec.Probe is given,
// Run probes itself.
//
// # Files
//
// Data is written to Path+".part" (parent directories are created with mode
// 0755). On success the file is fsynced, closed and atomically renamed to
// Path. The engine never overwrites: if Path exists (at start or before the
// rename) Run fails with a fatal ErrDestinationExists; the file-exists
// policy is applied upstream. Nothing else is removed. When the size is
// known, free space for the remaining bytes is checked first
// (platform.FreeSpace; fatal disk_full when insufficient, skipped when
// unknown) and a fresh file is preallocated (platform.Preallocate). Write
// errors caused by a full disk are fatal disk_full errors.
//
// # Modes
//
// Ranged mode is used when the resource supports ranges and its size is
// known and positive; otherwise the download is sequential.
//
// Ranged: the file is a set of segments (domain.Segment, Done counted from
// Start). A fresh download starts with the single segment [0, size).
// Restored segments are used only when they exactly cover [0, size) without
// gaps or overlaps and the .part file exists with length size; otherwise the
// download starts over. A restored download keeps the fingerprint its bytes
// were downloaded with (a probe's newer fingerprint is only adopted on a
// fresh start), so If-Range detects a changed file.
//
// Sequential: one connection streams the whole file from offset 0. Resume is
// impossible, so a new run truncates the .part file and every retry starts
// again from 0. When the size is unknown it becomes the number of bytes
// received; when it is known, a short body is a transient error and a longer
// one a fatal ErrRangeMismatch.
//
// # Connections, splitting and work stealing
//
// A ranged run starts with one connection; when a fresh probe response is
// attached and the first segment starts at 0, that connection streams it
// (up to the probe's Content-Range end) and only requests the rest. Every
// slowStartInterval (default 500ms) the controller doubles the number of
// connections, capped at settings.MaxConnections, provided bytes arrived
// since the previous tick. A new connection takes an unowned incomplete
// segment, else splits the segment with the most remaining bytes in half
// when that remainder is at least the minimum split size (default 1 MiB).
// A connection that finishes its segment steals work the same way and exits
// when there is none. Splitting only shrinks the owner's end under the
// table mutex and never hands out bytes the owner has already claimed; the
// owner re-reads its end before every write, writes exactly up to it and
// closes its response. Writes use File.WriteAt on non-overlapping ranges,
// with no lock held during I/O.
//
// # Requests and validation
//
// Requests go to Source.RedirectURL when known, else CurrentURL, with the
// same method, body, headers, User-Agent and Referer as the probe. Each
// carries "Range: bytes=START-END" and, when the fingerprint has one,
// If-Range with the strong ETag (or the Last-Modified date). A 206 must
// have a Content-Range with exactly the requested start, an end not beyond
// the requested end and the known total, else the download fails with a
// fatal ErrRangeMismatch. A shorter range is accepted and the remainder
// requested again. A 200 means the validator failed (the file changed) or
// ranges are no longer honoured, and settings.Refresh.OnMismatch decides:
// "restart" switches to a fresh sequential download from 0 that streams
// this 200 response (adopting its size and validators); "fail" and "ask"
// fail with ErrRangeIgnored classified as link_expired (the content behind
// the link changed, so the refresh layer may obtain the right link). Other statuses become *StatusError and are
// classified by Classify (for example 403 is link_expired, 503 transient
// with Retry-After).
//
// # Retries and stalls
//
// Each connection retries its own transient failures with
// retry.FromSettings(settings.Retry); bytes written since the previous
// failure reset the retry budget. A link-expired or fatal error on any
// connection cancels the whole download and is returned; a connection that
// exhausts its retries makes Run return that transient error, for the task
// runner to decide. A per-stream watchdog timer is armed around each
// network read (and the wait for response headers); if no data arrives
// within the stall timeout (default 30s) the request is aborted and the
// failure is a transient ErrStalled. Time spent in the rate limiter or on
// disk does not count.
//
// # Rate limiting, progress and checkpoints
//
// Every response body is wrapped with ratelimit.Reader and Spec.Limiter.
// Progress is kept in atomics. Spec.Checkpointer receives a consistent
// snapshot (segments, bytes written, size and, once changed, the Source with
// the redirect URL and fingerprint) at most once per checkpoint interval
// (default 1s) and once before Run returns, using a short detached context
// if ctx was cancelled. No checkpoint is written before the .part file
// state is established, so a run that fails early leaves the stored state
// alone. Checkpointer errors are logged and never fail the download.
// Sequential checkpoints describe a single segment [0, size) when the size
// is known. Spec.EngineState is not used: the segments carry all resume
// state.
//
// # Cancellation
//
// Cancelling ctx stops all connections promptly; Run waits for every
// goroutine it started, writes the final checkpoint and returns ctx.Err().
package httpx
