package ytdlp

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
)

// info is the part of the `yt-dlp -J` output the engine uses.
type info struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Ext   string `json:"ext"`
	Type  string `json:"_type"`

	FileSize       *float64 `json:"filesize"`
	FileSizeApprox *float64 `json:"filesize_approx"`
	// RequestedFormats are the formats yt-dlp merges (video and audio).
	RequestedFormats []format `json:"requested_formats"`
	// Formats are all the formats the site offers.
	Formats []format `json:"formats"`
}

type format struct {
	FileSize       *float64 `json:"filesize"`
	FileSizeApprox *float64 `json:"filesize_approx"`

	FormatID string  `json:"format_id"`
	Ext      string  `json:"ext"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	VCodec   string  `json:"vcodec"`
	ACodec   string  `json:"acodec"`
	TBR      float64 `json:"tbr"` // total bitrate, kbit/s
}

func (f format) size() (int64, bool) {
	switch {
	case f.FileSize != nil && *f.FileSize > 0:
		return int64(*f.FileSize), true
	case f.FileSizeApprox != nil && *f.FileSizeApprox > 0:
		return int64(*f.FileSizeApprox), true
	default:
		return 0, false
	}
}

// parseInfo reads the JSON of one video; a playlist is refused.
func parseInfo(data []byte) (*info, error) {
	var in info
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("ytdlp: cannot read the output of yt-dlp: %w", err)
	}
	if in.Type == "playlist" || in.Type == "multi_video" {
		return nil, errors.New("ytdlp: the URL is a playlist; playlists are not supported, give the URL of one video")
	}
	return &in, nil
}

// size is the size of what would be downloaded: the sum of the merged formats
// when all of them have a size, else the format's own size; -1 when unknown.
func (in *info) size() int64 {
	if len(in.RequestedFormats) > 0 {
		var total int64
		for _, f := range in.RequestedFormats {
			n, ok := f.size()
			if !ok {
				return -1
			}
			total += n
		}
		return total
	}
	return format{FileSize: in.FileSize, FileSizeApprox: in.FileSizeApprox}.sizeOrUnknown()
}

func (f format) sizeOrUnknown() int64 {
	if n, ok := f.size(); ok {
		return n
	}
	return -1
}

// fileName is the title with the container of the chosen format.
func (in *info) fileName() string {
	base := httpx.SanitizeFilename(strings.TrimSpace(in.Title))
	if base == "" || base == "download" {
		base = httpx.SanitizeFilename(in.ID)
	}
	if base == "" || base == "download" {
		base = "video"
	}
	ext := strings.Trim(strings.ToLower(in.Ext), ". ")
	if ext == "" || strings.ContainsAny(ext, `/\ `) {
		return base
	}
	return base + "." + ext
}

// pickFormats lists what the site offers for a picker, best first: one video
// format per height (the one with the highest bitrate) and the best audio-only
// format. A video-only format is selected together with the best audio (the
// id is a yt-dlp -f selector, which Source.MediaHint becomes), so downloading it
// still gives a file with sound.
func (in *info) pickFormats() []domain.Format {
	bestVideo := map[int]format{}
	var bestAudio *format
	for i := range in.Formats {
		f := in.Formats[i]
		if f.FormatID == "" {
			continue
		}
		hasVideo, hasAudio := f.VCodec != "" && f.VCodec != "none", f.ACodec != "" && f.ACodec != "none"
		switch {
		case hasVideo && f.Height > 0:
			if cur, ok := bestVideo[f.Height]; !ok || f.TBR > cur.TBR {
				bestVideo[f.Height] = f
			}
		case !hasVideo && hasAudio:
			if bestAudio == nil || f.TBR > bestAudio.TBR {
				bestAudio = &in.Formats[i]
			}
		}
	}
	heights := make([]int, 0, len(bestVideo))
	for h := range bestVideo {
		heights = append(heights, h)
	}
	slices.SortFunc(heights, func(a, b int) int { return b - a })
	out := make([]domain.Format, 0, len(heights)+1)
	for _, h := range heights {
		f := bestVideo[h]
		id := f.FormatID
		if f.ACodec == "" || f.ACodec == "none" {
			id = f.FormatID + "+bestaudio/" + f.FormatID
		}
		out = append(out, domain.Format{
			ID: id, Label: domain.FormatLabel(h, 0, false) + formatExt(f.Ext), Kind: domain.FormatVideo,
			Width: f.Width, Height: h, Bandwidth: int64(f.TBR * 1000), Codecs: f.VCodec, Container: f.Ext, Size: f.sizeOrUnknown(),
		})
	}
	if bestAudio != nil {
		out = append(out, domain.Format{
			ID: bestAudio.FormatID, Label: "audio only" + formatExt(bestAudio.Ext), Kind: domain.FormatAudio,
			Bandwidth: int64(bestAudio.TBR * 1000), Codecs: bestAudio.ACodec, Container: bestAudio.Ext, Size: bestAudio.sizeOrUnknown(),
		})
	}
	return out
}

func formatExt(ext string) string {
	if ext == "" {
		return ""
	}
	return " (" + ext + ")"
}
