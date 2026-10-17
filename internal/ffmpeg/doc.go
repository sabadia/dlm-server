// Package ffmpeg finds the ffmpeg executable the stream engines use to
// remux and merge media (HLS, DASH), or downloads one.
//
// ffmpeg is only ever run as a separate process, so nothing of it is linked
// into dlm. Locator looks, in order, at the configured path, at ffmpeg on
// PATH, at the cache of downloaded builds, and (when allowed) downloads a
// pinned static build, checking its SHA-256 before it is unpacked and
// executed.
package ffmpeg
