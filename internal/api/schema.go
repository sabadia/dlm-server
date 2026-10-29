package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/settings"
)

// nullable is the OpenAPI stand-in for settings.Optional[T]: the schema of
// T that also accepts null (null and an absent field both mean "unset /
// inherit"). Optional[T] has no exported fields, so without these aliases
// huma would document (and validate) every setting as an empty object.
type nullable[T any] struct{}

// Schema implements huma.SchemaProvider.
func (nullable[T]) Schema(r huma.Registry) *huma.Schema {
	s := *r.Schema(reflect.TypeFor[T](), false, "")
	s.Nullable = true
	return &s
}

func aliasOptional[T any](r huma.Registry) {
	r.RegisterTypeAlias(reflect.TypeFor[settings.Optional[T]](), reflect.TypeFor[nullable[T]]())
}

// registerSettingsSchemas makes huma describe every settings.Optional[T]
// used by settings.Settings as a nullable T. TestSettingsSchemaComplete
// fails when a new Optional type is added to Settings without an alias.
func registerSettingsSchemas(r huma.Registry) {
	aliasOptional[string](r)
	aliasOptional[int](r)
	aliasOptional[int64](r)
	aliasOptional[bool](r)
	aliasOptional[settings.Duration](r)
	aliasOptional[settings.Proxy](r)
	aliasOptional[[]settings.ProxyRule](r)
	aliasOptional[map[string]string](r)
	aliasOptional[settings.FileExistsPolicy](r)
	aliasOptional[settings.Checksum](r)
	aliasOptional[[]settings.RefreshStrategy](r)
	aliasOptional[settings.MismatchPolicy](r)
	aliasOptional[settings.BrowserUsage](r)
	aliasOptional[[]settings.PostAction](r)
	aliasOptional[settings.Schedule](r)
	aliasOptional[[]settings.SpeedProfile](r)
	aliasOptional[settings.Quota](r)
	aliasOptional[settings.QueueCompleteAction](r)
	aliasOptional[float64](r)
	aliasOptional[[]string](r)
}

// SettingsPatch is an RFC 7396 JSON merge patch of one settings layer, kept
// as raw JSON and applied by the service layer (settings.MergePatch): an
// absent field keeps its value, null unsets it, the groups retry and
// refresh merge recursively and every other value replaces the field.
type SettingsPatch []byte

// Schema implements huma.SchemaProvider.
func (SettingsPatch) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{
		Type: huma.TypeObject,
		Description: "RFC 7396 JSON merge patch of a Settings layer: absent fields are kept, null unsets a field " +
			"(it inherits from the next lower level again), retry and refresh merge recursively, every other value " +
			"replaces the field as a whole.",
		AdditionalProperties: true,
	}
}

// UnmarshalJSON keeps the raw JSON.
func (p *SettingsPatch) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("api: settings patch must be a JSON object")
	}
	*p = append((*p)[:0], data...)
	return nil
}

// MarshalJSON returns the raw JSON (null when empty).
func (p SettingsPatch) MarshalJSON() ([]byte, error) {
	if len(p) == 0 {
		return []byte("null"), nil
	}
	return json.RawMessage(p).MarshalJSON() //nolint:wrapcheck // RawMessage.MarshalJSON never fails
}
