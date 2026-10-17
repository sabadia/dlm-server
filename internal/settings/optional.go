package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Optional holds a value that may be unset. An unset value means "inherit
// from the next level". In JSON, an absent field and null both mean unset;
// use the `omitzero` tag option so unset fields are omitted when encoding.
type Optional[T any] struct {
	value T
	set   bool
}

// Some returns an Optional holding v.
func Some[T any](v T) Optional[T] { return Optional[T]{value: v, set: true} }

// None returns an unset Optional.
func None[T any]() Optional[T] { return Optional[T]{} }

// Get returns the value and whether it is set.
func (o Optional[T]) Get() (T, bool) { return o.value, o.set }

// Value returns the value, or the zero value of T when unset.
func (o Optional[T]) Value() T { return o.value }

// IsSet reports whether a value is present.
func (o Optional[T]) IsSet() bool { return o.set }

// IsZero reports whether the value is unset. It makes `omitzero` omit unset
// fields when marshalling.
func (o Optional[T]) IsZero() bool { return !o.set }

// OrElse returns the value, or def when unset.
func (o Optional[T]) OrElse(def T) T {
	if o.set {
		return o.value
	}
	return def
}

// MarshalJSON encodes the value, or null when unset.
func (o Optional[T]) MarshalJSON() ([]byte, error) {
	if !o.set {
		return []byte("null"), nil
	}
	b, err := json.Marshal(o.value)
	if err != nil {
		return nil, fmt.Errorf("settings: marshal optional: %w", err)
	}
	return b, nil
}

// UnmarshalJSON decodes a value; null leaves the Optional unset.
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*o = Optional[T]{}
		return nil
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("settings: unmarshal optional: %w", err)
	}
	*o = Some(v)
	return nil
}

// isSet implements optionalValue for reflection-based resolution.
func (o Optional[T]) isSet() bool { return o.set }

// optionalValue is implemented by every Optional[T].
type optionalValue interface{ isSet() bool }

// Duration is a time.Duration encoded in JSON as a Go duration string
// such as "1m30s".
type Duration time.Duration

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String formats the duration like time.Duration.
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("settings: invalid duration %q: %w", text, err)
	}
	*d = Duration(v)
	return nil
}
