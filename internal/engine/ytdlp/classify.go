package ytdlp

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
)

// classifyFailure turns the stderr of a failed yt-dlp run into an
// *engine.Error. yt-dlp has no machine-readable errors; its "ERROR:" lines
// are matched by what they say:
//
//	disk full                                   → fatal (disk_full)
//	HTTP 403, 404, 410, "has been removed"      → link_expired
//	HTTP 429 and 5xx, timeouts, resets, DNS     → transient (network)
//	anything else (unsupported URL, private …)  → fatal
func classifyFailure(stderr string, runErr error) error {
	msg := lastError(stderr)
	if msg == "" {
		msg = runErr.Error()
	}
	err := fmt.Errorf("ytdlp: %s", msg)
	low := strings.ToLower(msg)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(low, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("no space left on device", "disk full", "not enough space"):
		return engine.Fatal(domain.ErrorCodeDiskFull, err)
	case has("http error 403", "http error 404", "http error 410", "has been removed", "no longer available"):
		return engine.LinkExpired(err)
	case has("http error 429", "http error 5", "timed out", "timeout", "connection reset", "connection refused",
		"temporary failure", "network is unreachable", "remote end closed", "incompleteread", "unable to download", "eof occurred"):
		return engine.Transient(domain.ErrorCodeNetwork, err)
	default:
		return engine.Fatal(domain.ErrorCodeFatal, err)
	}
}

// lastError returns the text of the last "ERROR:" line of yt-dlp's stderr.
func lastError(stderr string) string {
	lines := strings.Split(strings.ReplaceAll(stderr, "\r", ""), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), "ERROR:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

var errNoOutput = errors.New("ytdlp: yt-dlp finished without reporting the file it made")
