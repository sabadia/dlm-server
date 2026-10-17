package cli

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// maxTorrentFile bounds a local .torrent file sent to the daemon (the daemon
// refuses larger ones).
const maxTorrentFile = 16 << 20

// resolveTorrentFile turns the path of a local .torrent file into the data
// URL the BitTorrent engine understands; anything else (a URL, a magnet
// link) is returned unchanged.
func resolveTorrentFile(arg string) (string, error) {
	lower := strings.ToLower(arg)
	if !strings.HasSuffix(lower, ".torrent") || strings.Contains(arg, "://") ||
		strings.HasPrefix(lower, "magnet:") || strings.HasPrefix(lower, "data:") {
		return arg, nil
	}
	fi, err := os.Stat(arg)
	switch {
	case err != nil:
		return "", &usageError{err: fmt.Errorf("torrent file %q: %w", arg, errors.Unwrap(err))}
	case fi.IsDir():
		return "", &usageError{err: fmt.Errorf("%s is a directory, not a .torrent file", arg)}
	case fi.Size() > maxTorrentFile:
		return "", &usageError{err: fmt.Errorf("%s is larger than %d bytes", arg, maxTorrentFile)}
	}
	data, err := os.ReadFile(arg) //nolint:gosec // the user names the file to send
	if err != nil {
		return "", &usageError{err: fmt.Errorf("read %s: %w", arg, err)}
	}
	return "data:application/x-bittorrent;base64," + base64.StdEncoding.EncodeToString(data), nil
}
