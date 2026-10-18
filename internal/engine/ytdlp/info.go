package ytdlp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
}

type format struct {
	FileSize       *float64 `json:"filesize"`
	FileSizeApprox *float64 `json:"filesize_approx"`
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
