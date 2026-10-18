// Package ytdlp downloads media from the sites yt-dlp supports (video and
// audio pages, with the extraction of the real media URLs), by running the
// yt-dlp executable as a separate process.
//
// The engine takes a source only when its ResolverKind is "ytdlp": yt-dlp
// accepts nearly any web page, so choosing it automatically would claim plain
// file links. Resolve runs `yt-dlp -J` and reads the title, the container and
// the size of the format that would be downloaded; Source.MediaHint is a
// yt-dlp format selector (-f) and defaults to the best video and audio. A
// download runs yt-dlp with a progress template and parses its progress lines;
// ffmpeg (for merging video and audio) comes from the same locator the stream
// engines use.
//
// Output goes to a work directory next to the final path (<path>.part) that
// yt-dlp resumes from, and the finished file is moved to the path. Pausing
// interrupts the process; the partial files stay for the next run. Cookies
// from the cookie store are handed over in a temporary Netscape file (mode
// 0600) that is removed after the run. Playlists are not supported: pass the
// URL of one video.
//
// Limits: the task's speed limit setting is passed to yt-dlp as --limit-rate,
// so the limits of the queue and of the whole daemon do not apply to it.
package ytdlp
