package domain

import (
	"fmt"
	"strconv"
)

// Format kinds.
const (
	FormatVideo = "video" // video with its audio muxed in, or video only
	FormatAudio = "audio" // audio only
)

// Format is one quality a stream can be downloaded in: what a client offers in
// a picker. Passing ID as Source.MediaHint selects it.
type Format struct {
	// ID is the value for Source.MediaHint that selects this format.
	ID string `json:"id"`
	// Label is a short description for a person: "1080p", "720p, 2.1 Mbit/s",
	// "audio only".
	Label string `json:"label"`
	// Kind is video (a picture, with or without sound) or audio.
	Kind string `json:"kind" enum:"video,audio"`
	// Width and Height are the picture size in pixels; 0 when unknown.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// Bandwidth is bits per second; 0 when unknown.
	Bandwidth int64 `json:"bandwidth,omitempty"`
	// Codecs is the codec string the stream declares, if any.
	Codecs string `json:"codecs,omitempty"`
	// Container is the file extension of the result (mp4, webm, m4a), if known.
	Container string `json:"container,omitempty"`
	// Size is the number of bytes this format would download; -1 when unknown.
	Size int64 `json:"size"`
}

// FormatLabel describes a stream for a person from its height and bandwidth.
// multiple says that several streams share the height, so the bandwidth is
// shown too.
func FormatLabel(height int, bandwidth int64, multiple bool) string {
	switch {
	case height > 0 && (!multiple || bandwidth <= 0):
		return strconv.Itoa(height) + "p"
	case height > 0:
		return fmt.Sprintf("%dp, %s", height, bitrate(bandwidth))
	case bandwidth > 0:
		return bitrate(bandwidth)
	default:
		return "stream"
	}
}

func bitrate(bps int64) string {
	if bps >= 1_000_000 {
		return fmt.Sprintf("%.1f Mbit/s", float64(bps)/1_000_000)
	}
	return fmt.Sprintf("%d kbit/s", bps/1000)
}
