package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// ErrInvalidPatch is matched (errors.Is) by every error MergePatch returns.
// The settings package cannot import internal/domain (domain depends on
// settings), so the service layer translates it to domain.ErrInvalidArgument.
var ErrInvalidPatch = errors.New("settings: invalid merge patch")

// MergePatch applies an RFC 7396 JSON Merge Patch to the JSON form of one
// settings layer and returns the result:
//
//   - a field absent from the patch keeps its value;
//   - null unsets the field (it inherits from the next lower level again);
//   - settings groups (retry, refresh) merge recursively, so
//     {"retry":{"maxAttempts":null}} unsets only retry.maxAttempts and
//     {"retry":null} unsets the whole group;
//   - every other value replaces the field as a whole: maps (headers),
//     arrays (proxyRules, postActions, refresh.strategies) and object-valued
//     settings (proxy, checksum, schedule) are never merged key by key.
//
// The patch must be a JSON object. Unknown fields are rejected. The result is
// not validated for any level; callers validate it with Settings.Validate.
// Errors match ErrInvalidPatch.
func MergePatch(base Settings, patch []byte) (Settings, error) {
	patchDoc, err := decodeObject(patch)
	if err != nil {
		return Settings{}, fmt.Errorf("%w: %w", ErrInvalidPatch, err)
	}
	baseJSON, err := json.Marshal(base)
	if err != nil {
		return Settings{}, fmt.Errorf("settings: merge patch: encode base: %w", err)
	}
	baseDoc, err := decodeObject(baseJSON)
	if err != nil {
		return Settings{}, fmt.Errorf("settings: merge patch: decode base: %w", err)
	}
	merged, err := json.Marshal(mergeObject(baseDoc, patchDoc, ""))
	if err != nil {
		return Settings{}, fmt.Errorf("%w: %w", ErrInvalidPatch, err)
	}
	dec := json.NewDecoder(bytes.NewReader(merged))
	dec.DisallowUnknownFields()
	var out Settings
	if err := dec.Decode(&out); err != nil {
		return Settings{}, fmt.Errorf("%w: %w", ErrInvalidPatch, err)
	}
	return out, nil
}

// decodeObject decodes a single JSON object, keeping numbers exact.
func decodeObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("malformed JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the JSON object")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("patch must be a JSON object")
	}
	return obj, nil
}

// mergeObject applies patch to target (RFC 7396 MergePatch). prefix is the
// JSON path of target; only settings groups are merged recursively.
func mergeObject(target, patch map[string]any, prefix string) map[string]any {
	if target == nil {
		target = make(map[string]any)
	}
	for k, pv := range patch {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if pv == nil {
			delete(target, k)
			continue
		}
		pobj, isObj := pv.(map[string]any)
		if !isObj || !isGroupPath(path) {
			target[k] = pv
			continue
		}
		tobj, _ := target[k].(map[string]any) //nolint:errcheck // a missing or non-object target starts empty (RFC 7396)
		target[k] = mergeObject(tobj, pobj, path)
	}
	return target
}

// isGroupPath reports whether path names a settings group (a struct of
// Optional fields such as "retry") rather than a leaf setting.
func isGroupPath(path string) bool {
	prefix := path + "."
	return slices.ContainsFunc(fields(), func(f field) bool { return strings.HasPrefix(f.path, prefix) })
}
