package postproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mholt/archives"

	"github.com/sabadia/dlm-server/internal/engine/httpx"
)

// Limits of an extraction: an archive can unpack to far more than it weighs.
const (
	// MaxExtractBytes bounds the bytes one archive may unpack to.
	MaxExtractBytes int64 = 64 << 30
	// MaxExtractEntries bounds the entries of one archive.
	MaxExtractEntries = 1_000_000
)

// errNotArchive means the file is not an archive that can be unpacked.
var errNotArchive = errors.New("not an archive")

// Extract is the post action that unpacks a downloaded archive (parameters:
// dir, delete, password). Formats: zip, rar (including multi-volume sets),
// 7z, tar and the compressed tar forms. A file that is not an archive is left
// alone. Entries that would land outside the destination, links and special
// files are skipped, and files that already exist are not overwritten.
//
// A volume of a rar set that is downloaded before the others does nothing
// yet; the volume that completes the set unpacks all of them.
type Extract struct{}

// Run implements Action.
func (Extract) Run(ctx context.Context, j *Job, params map[string]string) error {
	fi, err := os.Stat(j.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil // nothing there to unpack
	case err != nil:
		return fmt.Errorf("stat %q: %w", j.Path, err)
	case !fi.Mode().IsRegular():
		return nil // a directory (a torrent) is not an archive
	}
	set := volumesOf(j.Path)
	dest := params["dir"]
	switch dest {
	case "":
		dest = filepath.Join(filepath.Dir(set.first), set.stem)
	case ".":
		dest = filepath.Dir(set.first)
	default:
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(set.first), filepath.FromSlash(dest))
		}
	}
	res, err := extractArchive(ctx, set, dest, params["password"])
	switch {
	case errors.Is(err, errNotArchive):
		return nil
	case errors.Is(err, errIncompleteSet):
		j.Note("extract: waiting for the other parts of %s", filepath.Base(set.first))
		return nil
	case err != nil:
		return err
	}
	j.Note("extract: %d files unpacked to %s%s", res.files, dest, res.skippedNote())
	if params["delete"] == "true" {
		for _, m := range set.members {
			if err := os.Remove(m); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove %q: %w", m, err)
			}
		}
	}
	return nil
}

// extractResult counts what an extraction did.
type extractResult struct {
	files, dirs, skipped int
	bytes                int64
}

func (r extractResult) skippedNote() string {
	if r.skipped == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d entries skipped: existing files, links or unsafe names)", r.skipped)
}

var errIncompleteSet = errors.New("volumes of the archive are missing")

// volumeSet is an archive and, for a multi-volume rar, its other volumes.
type volumeSet struct {
	first   string   // the volume to open (the archive itself when single)
	members []string // every volume of the set present in the directory
	stem    string   // the archive's name without its extension(s)
	rar     bool     // a rar set (opened by name so the decoder finds the volumes)
}

var (
	rarPartRE = regexp.MustCompile(`(?i)^(.*)\.part(0*)(\d+)\.rar$`)
	rarOldRE  = regexp.MustCompile(`(?i)^(.*)\.r(\d{2,3})$`)
)

// volumesOf finds the volumes of the multi-volume rar set a file belongs to
// (name.partNN.rar, or name.rar with name.r00, name.r01, …); any other file is
// a set of its own.
func volumesOf(p string) volumeSet {
	dir, name := filepath.Split(p)
	single := volumeSet{first: p, members: []string{p}, stem: stemOf(name)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return single
	}
	if m := rarPartRE.FindStringSubmatch(name); m != nil {
		base := m[1]
		width := len(m[2]) + len(m[3]) // zero padding plus digits: part01 -> 2
		var members []string
		first := ""
		for _, e := range entries {
			if mm := rarPartRE.FindStringSubmatch(e.Name()); mm != nil && strings.EqualFold(mm[1], base) {
				members = append(members, filepath.Join(dir, e.Name()))
				if len(mm[2])+len(mm[3]) == width && strings.TrimLeft(mm[2]+mm[3], "0") == "1" {
					first = filepath.Join(dir, e.Name())
				}
			}
		}
		if first == "" { // the first volume has not arrived
			first = filepath.Join(dir, fmt.Sprintf("%s.part%0*d.rar", base, width, 1))
		}
		return volumeSet{first: first, members: members, stem: base, rar: true}
	}
	base := ""
	switch {
	case rarOldRE.MatchString(name):
		base = rarOldRE.FindStringSubmatch(name)[1]
	case strings.EqualFold(filepath.Ext(name), ".rar"):
		base = strings.TrimSuffix(name, filepath.Ext(name))
	default:
		return single
	}
	set := volumeSet{stem: base, rar: true}
	for _, e := range entries {
		n := e.Name()
		switch {
		case strings.EqualFold(n, base+".rar"):
			set.first = filepath.Join(dir, n)
			set.members = append(set.members, set.first)
		case rarOldRE.MatchString(n) && strings.EqualFold(rarOldRE.FindStringSubmatch(n)[1], base):
			set.members = append(set.members, filepath.Join(dir, n))
		}
	}
	if set.first == "" { // only the .rNN volumes are here so far
		set.first = filepath.Join(dir, base+".rar")
	}
	return set
}

// stemOf is a file name without its extension, treating .tar.gz and the
// like as one extension.
func stemOf(name string) string {
	lower := strings.ToLower(name)
	for _, e := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst", ".tar.lz4", ".tar.br", ".tar.sz", ".tar.lz", ".tar.z"} {
		if strings.HasSuffix(lower, e) {
			return name[:len(name)-len(e)]
		}
	}
	if ext := filepath.Ext(name); ext != "" && ext != name {
		return strings.TrimSuffix(name, ext)
	}
	return name
}

// extractArchive unpacks the archive (set.first) into dest.
func extractArchive(ctx context.Context, set volumeSet, dest, password string) (extractResult, error) {
	var res extractResult
	if _, err := os.Stat(set.first); err != nil {
		if set.rar && errors.Is(err, fs.ErrNotExist) {
			return res, errIncompleteSet
		}
		return res, err //nolint:wrapcheck // the file's own error
	}
	f, err := os.Open(set.first) //nolint:gosec // the task's own file
	if err != nil {
		return res, fmt.Errorf("open %q: %w", set.first, err)
	}
	defer func() { _ = f.Close() }() // read-only
	format, stream, err := archives.Identify(ctx, filepath.Base(set.first), f)
	if err != nil {
		if errors.Is(err, archives.NoMatch) {
			return res, errNotArchive
		}
		return res, fmt.Errorf("identify %q: %w", set.first, err)
	}
	ex, ok := format.(archives.Extractor)
	if !ok {
		return res, errNotArchive // a single compressed file, not an archive
	}
	switch fm := format.(type) {
	case archives.Rar:
		fm.Password = password
		fm.Name, fm.FS = filepath.Base(set.first), os.DirFS(filepath.Dir(set.first))
		ex = fm
	case archives.SevenZip:
		fm.Password = password
		ex = fm
	default:
	}
	if err := os.MkdirAll(dest, 0o755); err != nil { //nolint:gosec // beside the archive, like the downloads
		return res, fmt.Errorf("create %q: %w", dest, err)
	}
	err = ex.Extract(ctx, stream, func(ctx context.Context, fi archives.FileInfo) error {
		return res.entry(ctx, dest, fi)
	})
	if err != nil {
		if set.rar && errors.Is(err, fs.ErrNotExist) {
			return res, errIncompleteSet
		}
		return res, fmt.Errorf("unpack %q: %w", filepath.Base(set.first), friendly(err))
	}
	return res, nil
}

// friendly turns the errors of encrypted archives into a message that says
// what to do.
func friendly(err error) error {
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "password") || strings.Contains(low, "encrypt") {
		return fmt.Errorf("the archive is encrypted and the password is missing or wrong (set the password parameter): %w", err)
	}
	return err
}

// entry unpacks one entry of an archive below dest.
func (r *extractResult) entry(ctx context.Context, dest string, fi archives.FileInfo) error {
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // cancellation is reported as is
	}
	if r.files+r.dirs+r.skipped >= MaxExtractEntries {
		return fmt.Errorf("the archive has more than %d entries", MaxExtractEntries)
	}
	target, ok := safeJoin(dest, fi.NameInArchive)
	if !ok {
		r.skipped++
		return nil
	}
	switch {
	case fi.IsDir():
		if target == dest {
			return nil
		}
		if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // beside the archive, like the downloads
			return fmt.Errorf("create %q: %w", target, err)
		}
		r.dirs++
		return nil
	case !fi.Mode().IsRegular() || fi.LinkTarget != "":
		r.skipped++ // links and devices are not unpacked
		return nil
	}
	if fi.Size() > 0 && r.bytes+fi.Size() > MaxExtractBytes {
		return fmt.Errorf("the archive unpacks to more than %d bytes", MaxExtractBytes)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // beside the archive, like the downloads
		return fmt.Errorf("create %q: %w", filepath.Dir(target), err)
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // a new file below dest
	if errors.Is(err, fs.ErrExist) {
		r.skipped++ // never overwrite
		return nil
	}
	if err != nil {
		return fmt.Errorf("create %q: %w", target, err)
	}
	in, err := fi.Open()
	if err != nil {
		_ = out.Close()
		_ = os.Remove(target)
		return fmt.Errorf("open %q in the archive: %w", fi.NameInArchive, err)
	}
	budget := MaxExtractBytes - r.bytes
	n, err := io.Copy(out, io.LimitReader(in, budget+1))
	_ = in.Close() // read-only
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > budget {
		err = fmt.Errorf("the archive unpacks to more than %d bytes", MaxExtractBytes)
	}
	if err != nil {
		_ = os.Remove(target) // never leave a partial file behind
		return fmt.Errorf("write %q: %w", target, err)
	}
	r.bytes += n
	r.files++
	if mt := fi.ModTime(); !mt.IsZero() {
		_ = os.Chtimes(target, mt, mt) // best effort: a cosmetic detail
	}
	return nil
}

// safeJoin returns the place of an archive entry below dest, or false when
// the name would leave dest (absolute, "..") or is empty. Every part of the
// name is made valid on every OS, as for downloaded file names.
func safeJoin(dest, name string) (string, bool) {
	name = strings.ReplaceAll(name, "\\", "/")
	if name == "" || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") || filepath.VolumeName(name) != "" {
		return "", false
	}
	clean := path.Clean(name)
	if clean == "." {
		return dest, true
	}
	parts := strings.Split(clean, "/")
	for i, p := range parts {
		if p == ".." || p == "" {
			return "", false
		}
		s := httpx.SanitizeFilename(p)
		if s == "" {
			return "", false
		}
		parts[i] = s
	}
	target := filepath.Join(dest, filepath.Join(parts...))
	if rel, err := filepath.Rel(dest, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return target, true
}
