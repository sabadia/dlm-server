package hls

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
)

// containerFor maps the destination's extension to an ffmpeg muxer and whether
// it needs "+faststart".
func containerFor(path string) (format string, faststart bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mkv":
		return "matroska", false
	case ".ts":
		return "mpegts", false
	case ".m4a":
		return "ipod", true
	case ".mov":
		return "mov", true
	case ".webm":
		return "webm", false
	default: // .mp4, .m4v and anything else
		return "mp4", true
	}
}

// needsFFmpeg reports whether the result cannot be produced by plain
// concatenation: only a single MPEG-TS track saved as .ts can.
func (d *Download) needsFFmpeg(p *plan) bool {
	if len(p.tracks) != 1 || !strings.EqualFold(filepath.Ext(d.spec.Path), ".ts") {
		return true
	}
	for _, s := range p.tracks[0].snapshot() {
		if s.init != nil {
			return true
		}
	}
	return false
}

func (d *Download) locateFFmpeg(ctx context.Context) (string, error) {
	if d.e.ff == nil {
		return "", engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w: no ffmpeg is configured", ErrFFmpeg))
	}
	b, err := d.e.ff.Locate(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err() //nolint:wrapcheck // cancellation is reported as is
		}
		return "", engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w: %w", ErrFFmpeg, err))
	}
	return b.Path, nil
}

// merge joins the downloaded pieces into <path>.part and renames it into
// place. Every track is first concatenated (each fMP4 init section before the
// segments it belongs to); ffmpeg then remuxes one track, or combines the
// video and the audio track, without re-encoding.
func (d *Download) merge(ctx context.Context, p *plan, ffmpegPath string) error {
	if err := d.ensureAllInits(ctx, p); err != nil {
		return err
	}
	var inputs []string
	for _, t := range p.tracks {
		in := filepath.Join(d.work, fmt.Sprintf("track%d.bin", t.idx))
		if err := d.concat(t, in); err != nil {
			return err
		}
		inputs = append(inputs, in)
	}
	_ = os.Remove(d.part) // a leftover of an earlier attempt
	if ffmpegPath == "" {
		if err := os.Rename(inputs[0], d.part); err != nil {
			return d.fileError("rename", d.part, err)
		}
	} else if err := d.e.opts.exec(ctx, ffmpegPath, d.ffmpegArgs(inputs)); err != nil {
		if ctx.Err() != nil {
			return ctx.Err() //nolint:wrapcheck // cancellation is reported as is
		}
		_ = os.Remove(d.part) // an incomplete output
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("hls: merge with ffmpeg: %w", err))
	}
	if err := os.Rename(d.part, d.spec.Path); err != nil {
		return d.fileError("rename", d.spec.Path, err)
	}
	return nil
}

// ensureAllInits makes sure every init section the segments need is on disk
// and known (a resumed run that had nothing left to fetch has not read them).
func (d *Download) ensureAllInits(ctx context.Context, p *plan) error {
	for _, t := range p.tracks {
		for _, s := range t.snapshot() {
			if s.init == nil || !d.isDone(t.idx, s.seq) {
				continue
			}
			if err := d.ensureInit(ctx, *s.init); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Download) ffmpegArgs(inputs []string) []string {
	args := []string{"-y", "-hide_banner", "-loglevel", "error", "-nostdin"}
	for _, in := range inputs {
		args = append(args, "-i", in)
	}
	// Optional maps: a data stream the container cannot hold (timed ID3 in
	// MPEG-TS) is left out instead of failing the merge.
	args = append(args, "-map", "0:v?", "-map", "0:a?")
	if len(inputs) > 1 {
		args = append(args, "-map", "1:a?")
	}
	format, faststart := containerFor(d.spec.Path)
	args = append(args, "-c", "copy", "-f", format)
	if faststart {
		args = append(args, "-movflags", "+faststart")
	}
	return append(args, d.part)
}

// concat writes the init sections and downloaded segments of t, in playlist
// order, to dest. An init section is repeated only when it changes.
func (d *Download) concat(t *track, dest string) error {
	out, err := os.Create(dest) //nolint:gosec // inside our work directory
	if err != nil {
		return d.fileError("create", dest, err)
	}
	var lastInit string
	copied := 0
	for _, s := range t.snapshot() {
		if !d.isDone(t.idx, s.seq) {
			continue // a live stream cut short, or a gap
		}
		if s.init != nil {
			if id := initID(*s.init); id != lastInit {
				if err := appendFile(out, d.inits[id]); err != nil {
					_ = out.Close() // the append failed; the error is what matters
					return d.fileError("append", d.inits[id], err)
				}
				lastInit = id
			}
		}
		if err := appendFile(out, d.segPath(t.idx, s.seq)); err != nil {
			_ = out.Close() // the append failed; the error is what matters
			return d.fileError("append", d.segPath(t.idx, s.seq), err)
		}
		copied++
	}
	if err := out.Close(); err != nil {
		return d.fileError("write", dest, err)
	}
	if copied == 0 {
		return engine.Fatal(domain.ErrorCodeFatal, ErrNoSegments)
	}
	return nil
}

func appendFile(dst io.Writer, path string) error {
	if path == "" {
		return errors.New("missing file")
	}
	f, err := os.Open(path) //nolint:gosec // inside our work directory
	if err != nil {
		return err //nolint:wrapcheck // wrapped by the caller with the path
	}
	defer func() { _ = f.Close() }() // read-only
	if _, err := io.Copy(dst, f); err != nil {
		return err //nolint:wrapcheck // wrapped by the caller with the path
	}
	return nil
}
