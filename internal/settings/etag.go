package settings

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ETag returns the strong entity tag of a settings layer: a quoted prefix of
// the SHA-256 of its canonical JSON (struct field order, sorted map keys). Two
// layers with the same set fields and values have the same tag.
func ETag(s Settings) string {
	b, err := json.Marshal(s)
	if err != nil {
		// Settings only holds JSON-encodable values; fall back to a tag that
		// never matches a stored one rather than panicking.
		return `"unknown"`
	}
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:12]) + `"`
}
