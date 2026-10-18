// Package ytdlp finds the yt-dlp executable the ytdlp engine runs, or
// downloads it.
//
// yt-dlp is only ever run as a separate process. Locator looks, in order, at
// the configured path, at yt-dlp on PATH, at the cache of downloaded builds,
// and (when allowed) downloads the pinned standalone release for the platform,
// checking its SHA-256 before it is executed. The pin is a release of yt-dlp
// that is not kept current: sites change quickly, so a newer build is best
// installed by the user and named in the configuration.
package ytdlp
