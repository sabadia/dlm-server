// Package segmented downloads streams that consist of many small files: it
// fetches the segments of one or more tracks with a pool of workers, decrypts
// AES-128 segments, keeps a resume journal, follows live playlists, and merges
// the pieces with ffmpeg. The protocol engines (hls, dash) supply a Frontend,
// which reads the stream description (a playlist, a manifest) and turns it into
// tracks of segments.
package segmented

// ByteRange selects part of a resource.
type ByteRange struct{ Offset, Length int64 }

// FileRef is a URL with an optional byte range: an init section or a segment.
type FileRef struct {
	URL   string
	Range *ByteRange
}

// Key is the AES-128 encryption of a segment.
type Key struct {
	Method string // "AES-128"
	URL    string // absolute key URL
	// IV is the 16-byte initialisation vector; nil means: derive it from the
	// segment's sequence number.
	IV []byte
}

// Segment is one piece of a track.
type Segment struct {
	// Seq identifies the segment within its track across playlist reloads
	// (HLS media sequence number, DASH segment number).
	Seq      uint64
	Ref      FileRef
	Duration float64 // seconds
	Key      *Key    // nil when clear
	// Init is the fMP4 initialisation section the segment needs, nil for
	// self-contained segments (MPEG-TS).
	Init *FileRef
	Disc bool // discontinuity before the segment
}

// Media is the segment list of one track at one moment.
type Media struct {
	Segments []Segment
	// Closed is true for a finished stream (HLS ENDLIST, DASH static); an open
	// one is live and is reloaded until it closes.
	Closed bool
	// Target is the target segment duration in seconds (live reload pace).
	Target float64
}

// Duration is the total duration of the segments, in seconds.
func (m *Media) Duration() float64 {
	var d float64
	for _, s := range m.Segments {
		d += s.Duration
	}
	return d
}
