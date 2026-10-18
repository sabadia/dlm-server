package postproc

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
)

// fileEntry is an entry of a test archive.
type fileEntry struct {
	name string
	data string
	dir  bool
	link string // a symbolic link to this target
}

func zipOf(t *testing.T, entries []fileEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)}
		switch {
		case e.dir:
			h.Name = strings.TrimSuffix(e.name, "/") + "/"
			h.Method = zip.Store
		case e.link != "":
			h.SetMode(os.ModeSymlink | 0o777)
			e.data = e.link
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if !e.dir {
			_, _ = w.Write([]byte(e.data))
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarGzOf(t *testing.T, entries []fileEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, ModTime: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)}
		switch {
		case e.dir:
			h.Typeflag, h.Mode = tar.TypeDir, 0o755
		case e.link != "":
			h.Typeflag, h.Linkname = tar.TypeSymlink, e.link
		default:
			h.Typeflag, h.Size = tar.TypeReg, int64(len(e.data))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(e.data))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// jobFor returns a job for the file name in dir.
func jobFor(dir, name string) *Job {
	return &Job{Task: domain.Task{ID: "t", Dir: dir, Name: name}, Path: filepath.Join(dir, name)}
}

func runExtract(t *testing.T, dir, name string, params map[string]string) (*Job, error) {
	t.Helper()
	j := jobFor(dir, name)
	return j, Extract{}.Run(t.Context(), j, params)
}

// tree lists the files below dir (slash-separated, relative), sorted.
func tree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func write(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var sample = []fileEntry{
	{name: "docs/", dir: true},
	{name: "docs/a.txt", data: "alpha"},
	{name: "docs/deep/b.txt", data: "beta"},
	{name: "top.txt", data: "top"},
}

func TestExtractZipAndTarGz(t *testing.T) {
	for name, build := range map[string]func() ([]byte, string){
		"zip":    func() ([]byte, string) { return zipOf(t, sample), "pack.zip" },
		"tar.gz": func() ([]byte, string) { return tarGzOf(t, sample), "pack.tar.gz" },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			data, file := build()
			write(t, dir, file, data)
			j, err := runExtract(t, dir, file, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := tree(t, filepath.Join(dir, "pack"))
			want := []string{"docs/a.txt", "docs/deep/b.txt", "top.txt"}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("unpacked %v, want %v (into a folder named after the archive)", got, want)
			}
			if readFile(t, filepath.Join(dir, "pack", "docs", "a.txt")) != "alpha" {
				t.Error("content differs")
			}
			if fi, _ := os.Stat(filepath.Join(dir, "pack", "top.txt")); fi.ModTime().Year() != 2020 {
				t.Errorf("modification time = %v, want the archived one", fi.ModTime())
			}
			if len(j.Notes) != 1 || !strings.Contains(j.Notes[0], "3 files") {
				t.Errorf("notes = %v", j.Notes)
			}
			if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
				t.Error("the archive was removed although delete was not asked for")
			}
		})
	}
}

func TestExtractDirAndDeleteParameters(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "pack.zip", zipOf(t, sample))
	// "." unpacks beside the archive; delete removes it afterwards.
	if _, err := runExtract(t, dir, "pack.zip", map[string]string{"dir": ".", "delete": "true"}); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, dir); strings.Join(got, ",") != "docs/a.txt,docs/deep/b.txt,top.txt" {
		t.Errorf("tree = %v (the archive should be gone, its files beside it)", got)
	}
	// A relative dir is below the archive's folder, an absolute one is used as given.
	write(t, dir, "again.zip", zipOf(t, sample[:2]))
	if _, err := runExtract(t, dir, "again.zip", map[string]string{"dir": "out/sub"}); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(dir, "out", "sub", "docs", "a.txt")) != "alpha" {
		t.Error("relative dir not honoured")
	}
	abs := filepath.Join(t.TempDir(), "elsewhere")
	if _, err := runExtract(t, dir, "again.zip", map[string]string{"dir": abs}); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(abs, "docs", "a.txt")) != "alpha" {
		t.Error("absolute dir not honoured")
	}
}

func TestExtractIsSafe(t *testing.T) {
	dir := t.TempDir()
	evil := []fileEntry{
		{name: "../escaped.txt", data: "x"},
		{name: "/abs.txt", data: "x"},
		{name: "a/../../escaped2.txt", data: "x"},
		{name: "ok/fine.txt", data: "fine"},
		{name: "link", link: "/etc/passwd"},
		{name: "ok/clash.txt", data: "from the archive"},
		{name: "we:ird*.txt", data: "odd"},
	}
	for _, c := range []struct {
		name, stem string
		data       []byte
	}{{"evil.zip", "evil", zipOf(t, evil)}, {"evil.tar.gz", "evil", tarGzOf(t, evil)}} {
		work := filepath.Join(dir, c.name+".d") // a folder of its own per archive
		if err := os.MkdirAll(filepath.Join(work, c.stem, "ok"), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(work, c.stem, "ok"), "clash.txt", []byte("mine"))
		write(t, work, c.name, c.data)
		j, err := runExtract(t, work, c.name, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := tree(t, filepath.Join(work, c.stem))
		for _, f := range got {
			if strings.Contains(f, "..") || strings.HasPrefix(f, "/") {
				t.Errorf("%s: unsafe path %q", c.name, f)
			}
		}
		if want := "ok/clash.txt,ok/fine.txt,we_ird_.txt"; strings.Join(got, ",") != want {
			t.Errorf("%s: unpacked %v, want %s", c.name, got, want)
		}
		if readFile(t, filepath.Join(work, c.stem, "ok", "clash.txt")) != "mine" {
			t.Errorf("%s: an existing file was overwritten", c.name)
		}
		for _, escaped := range []string{filepath.Join(work, "escaped.txt"), filepath.Join(dir, "escaped.txt"), filepath.Join(work, "escaped2.txt")} {
			if _, err := os.Stat(escaped); err == nil {
				t.Errorf("%s: an entry escaped to %s", c.name, escaped)
			}
		}
		if _, err := os.Lstat(filepath.Join(work, c.stem, "link")); err == nil {
			t.Errorf("%s: a symbolic link was created", c.name)
		}
		if len(j.Notes) != 1 || !strings.Contains(j.Notes[0], "skipped") {
			t.Errorf("%s: notes = %v, want a mention of the skipped entries", c.name, j.Notes)
		}
	}
	if _, ok := safeJoin("/d", "a/./b/../c.txt"); !ok {
		t.Error("a harmless name with dots was refused")
	}
}

func TestExtractLeavesOtherFilesAlone(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "notes.txt", []byte("hello"))
	write(t, dir, "single.gz", func() []byte {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, _ = w.Write([]byte("just one compressed file"))
		_ = w.Close()
		return b.Bytes()
	}())
	write(t, dir, "fake.zip", []byte("this is not a zip"))
	for _, name := range []string{"notes.txt", "single.gz"} {
		j, err := runExtract(t, dir, name, nil)
		if err != nil || len(j.Notes) != 0 {
			t.Errorf("%s: err %v notes %v, want nothing to do", name, err, j.Notes)
		}
	}
	if _, err := runExtract(t, dir, "fake.zip", nil); err == nil {
		t.Error("a broken zip is an error")
	}
	// A directory (a torrent) and a missing file are not archives either.
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"folder", "missing.zip"} {
		if j, err := runExtract(t, dir, name, nil); err != nil || len(j.Notes) != 0 {
			t.Errorf("%s: err %v notes %v", name, err, j.Notes)
		}
	}
	if got := tree(t, dir); strings.Join(got, ",") != "fake.zip,notes.txt,single.gz" {
		t.Errorf("tree = %v: nothing may have been created", got)
	}
}

func TestExtractMultiVolumeRar(t *testing.T) {
	dir := t.TempDir()
	copyTestdata := func(name string) {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		write(t, dir, name, b)
	}
	// Only the second volume has arrived: nothing happens yet, and it is not an error.
	copyTestdata("test.part02.rar")
	j, err := runExtract(t, dir, "test.part02.rar", nil)
	if err != nil || len(j.Notes) != 1 || !strings.Contains(j.Notes[0], "waiting") {
		t.Fatalf("incomplete set: err %v notes %v", err, j.Notes)
	}
	if _, err := os.Stat(filepath.Join(dir, "test")); err == nil {
		t.Fatal("something was unpacked from an incomplete set")
	}
	// The first volume completes the set: either volume triggers the unpacking.
	copyTestdata("test.part01.rar")
	j, err = runExtract(t, dir, "test.part02.rar", map[string]string{"delete": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "test", "test.txt")); len(got) != 8895 {
		t.Errorf("test.txt has %d bytes, want 8895", len(got))
	}
	if len(j.Notes) != 1 || !strings.Contains(j.Notes[0], "1 files") {
		t.Errorf("notes = %v", j.Notes)
	}
	// delete removed every volume of the set.
	for _, n := range []string{"test.part01.rar", "test.part02.rar"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			t.Errorf("%s was not deleted", n)
		}
	}
}

func TestVolumesOf(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.part1.rar", "a.part2.rar", "b.rar", "b.r00", "b.r01", "c.zip", "Z.PART01.RAR", "other.txt"} {
		write(t, dir, n, nil)
	}
	p := func(n string) string { return filepath.Join(dir, n) }
	base := func(paths []string) string {
		out := make([]string, len(paths))
		for i, x := range paths {
			out[i] = filepath.Base(x)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	for in, want := range map[string]struct{ first, members, stem string }{
		"a.part2.rar":   {"a.part1.rar", "a.part1.rar,a.part2.rar", "a"},
		"a.part1.rar":   {"a.part1.rar", "a.part1.rar,a.part2.rar", "a"},
		"b.r01":         {"b.rar", "b.r00,b.r01,b.rar", "b"},
		"b.rar":         {"b.rar", "b.r00,b.r01,b.rar", "b"},
		"c.zip":         {"c.zip", "c.zip", "c"},
		"other.txt":     {"other.txt", "other.txt", "other"},
		"new.part3.rar": {"new.part1.rar", "new.part3.rar", "new"},
	} {
		if in == "new.part3.rar" {
			write(t, dir, in, nil)
		}
		set := volumesOf(p(in))
		if filepath.Base(set.first) != want.first || base(set.members) != want.members || set.stem != want.stem {
			t.Errorf("volumesOf(%s) = first %s members %s stem %s; want %+v", in, filepath.Base(set.first), base(set.members), set.stem, want)
		}
	}
	for in, want := range map[string]string{"x.tar.gz": "x", "x.zip": "x", "x.tar.xz": "x", "plain": "plain", "x.y.zip": "x.y"} {
		if got := stemOf(in); got != want {
			t.Errorf("stemOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtract7zWithAndWithoutPassword(t *testing.T) {
	sz, err := exec.LookPath("7zz")
	if err != nil {
		if sz, err = exec.LookPath("7z"); err != nil {
			t.Skip("no 7-Zip to make a test archive with")
		}
	}
	for name, c := range map[string]struct {
		args []string
		pass string
		fail bool
	}{
		"plain":          {nil, "", false},
		"password":       {[]string{"-psecret", "-mhe=on"}, "secret", false},
		"wrong password": {[]string{"-psecret", "-mhe=on"}, "nope", true},
		"no password":    {[]string{"-psecret", "-mhe=on"}, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			write(t, src, "one.txt", []byte("seven zip"))
			dir := t.TempDir()
			args := append([]string{"a", "-bd", "-y"}, c.args...)
			args = append(args, filepath.Join(dir, "pack.7z"), filepath.Join(src, "one.txt"))
			if out, err := exec.Command(sz, args...).CombinedOutput(); err != nil { //nolint:gosec // a test tool
				t.Fatalf("7z: %v: %s", err, out)
			}
			params := map[string]string{}
			if c.pass != "" {
				params["password"] = c.pass
			}
			_, err := runExtract(t, dir, "pack.7z", params)
			if c.fail {
				if err == nil {
					t.Fatal("want an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if readFile(t, filepath.Join(dir, "pack", "one.txt")) != "seven zip" {
				t.Error("content differs")
			}
		})
	}
}

func TestExtractStopsWithTheContext(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "pack.zip", zipOf(t, sample))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := Extract{}.Run(ctx, jobFor(dir, "pack.zip"), nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got := tree(t, dir); len(got) != 1 {
		t.Errorf("tree = %v", got)
	}
}
