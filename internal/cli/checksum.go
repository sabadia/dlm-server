package cli

import (
	"fmt"
	"strings"

	"github.com/sabadia/dlm-server/internal/settings"
)

// parseChecksum reads ALGORITHM:HEX.
func parseChecksum(s string) (settings.Checksum, error) {
	alg, hex, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || alg == "" || hex == "" {
		return settings.Checksum{}, fmt.Errorf("--checksum must be ALGORITHM:HEX, for example sha256:9f86d0…")
	}
	return settings.Checksum{Algorithm: settings.ChecksumAlgorithm(strings.ToLower(alg)), Expected: strings.ToLower(hex)}, nil
}
