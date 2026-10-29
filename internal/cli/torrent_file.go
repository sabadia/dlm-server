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

// uploadTorrentFile is the size above which a local .torrent file is sent as
// the body of add-torrent-file instead of as a data URL inside the JSON of
// create-task, whose body is limited to 1 MiB (base64 grows the file by a third).
const uploadTorrentFile = 512 << 10

// largeTorrentFile reads the local .torrent file named by arg when it is too
// big for a data URL; ok is false for anything else (a URL, a magnet link, a
// small file), which resolveTorrentFile handles.
func largeTorrentFile(arg string) (data []byte, ok bool, err error) {
	lower := strings.ToLower(arg)
	if !strings.HasSuffix(lower, ".torrent") || strings.Contains(arg, "://") ||
		strings.HasPrefix(lower, "magnet:") || strings.HasPrefix(lower, "data:") {
		return nil, false, nil
	}
	fi, statErr := os.Stat(arg)
	if statErr != nil {
		return nil, false, nil //nolint:nilerr // not a readable file: resolveTorrentFile reports it
	}
	if fi.IsDir() || fi.Size() <= uploadTorrentFile {
		return nil, false, nil // resolveTorrentFile reports it or sends it inline
	}
	if fi.Size() > maxTorrentFile {
		return nil, false, &usageError{err: fmt.Errorf("%s is larger than %d bytes", arg, maxTorrentFile)}
	}
	data, err = os.ReadFile(arg) //nolint:gosec // the user names the file to send
	if err != nil {
		return nil, false, &usageError{err: fmt.Errorf("read %s: %w", arg, err)}
	}
	return data, true, nil
}

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
