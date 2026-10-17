package cli

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveTorrentFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.torrent")
	if err := os.WriteFile(file, []byte("d4:infod4:name1:aee"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveTorrentFile(file)
	if err != nil {
		t.Fatal(err)
	}
	enc, ok := strings.CutPrefix(got, "data:application/x-bittorrent;base64,")
	if !ok {
		t.Fatalf("got %q", got)
	}
	if raw, err := base64.StdEncoding.DecodeString(enc); err != nil || string(raw) != "d4:infod4:name1:aee" {
		t.Errorf("payload = %q, %v", raw, err)
	}
	// Everything that is not a local .torrent path is left alone.
	for _, in := range []string{
		"https://example.com/a.torrent", "magnet:?xt=urn:btih:abc", "MAGNET:?xt=urn:btih:abc",
		"data:application/x-bittorrent;base64,AAAA", "file.iso", "https://example.com/x",
	} {
		if out, err := resolveTorrentFile(in); err != nil || out != in {
			t.Errorf("resolveTorrentFile(%q) = %q, %v", in, out, err)
		}
	}
	for _, in := range []string{filepath.Join(dir, "missing.torrent"), filepath.Join(dir, "dir.torrent")} {
		if in != file && strings.HasSuffix(in, "dir.torrent") {
			if err := os.Mkdir(in, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		_, err := resolveTorrentFile(in)
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("resolveTorrentFile(%q) err = %v, want a usage error", in, err)
		}
	}
}
