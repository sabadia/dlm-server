// Package dash downloads MPEG-DASH streams (static MPD manifests). It reads the
// manifest, picks a video representation (Source.MediaHint) and an audio
// representation, and lets the shared segmented downloader fetch the segments
// and mux them with ffmpeg. Live (dynamic) manifests and DRM-protected streams
// are refused.
package dash
