package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

// Source says where the value of a key came from.
type Source string

// Sources, from the weakest to the strongest.
const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceEnv     Source = "env"
	SourceFlag    Source = "flag"
)

// ValueType is the kind of a key's value.
type ValueType string

// Value types.
const (
	TypeString   ValueType = "string"
	TypeBool     ValueType = "bool"
	TypeInt      ValueType = "int"
	TypeDuration ValueType = "duration"
	TypeList     ValueType = "list"
)

// secretKeys lists the keys whose values must never be shown. None of the
// boot configuration is secret today (TLS keys are file paths); a key that
// holds a secret goes here and is redacted by Describe.
var secretKeys = map[string]bool{}

// readOnlyKeys cannot be changed through Patch: moving the configuration
// directory would hide the file that was just written.
var readOnlyKeys = map[string]bool{"home": true, "paths.config_dir": true}

// Entry describes one key of the boot configuration.
type Entry struct {
	Key  string    `json:"key"`
	Type ValueType `json:"type"`
	// Value is what the running daemon uses; Default what it would use
	// without any source.
	Value   any `json:"value"`
	Default any `json:"default"`
	// Source is where Value came from.
	Source Source `json:"source"`
	// RestartRequired is always true for boot configuration: a change takes
	// effect when the daemon starts again.
	RestartRequired bool `json:"restartRequired"`
	// ReadOnly keys cannot be changed with Patch.
	ReadOnly bool `json:"readOnly"`
	// Secret values are shown as "***".
	Secret bool `json:"secret"`
	// Pending is the value the config file gives, present only when it differs
	// from Value and nothing stronger (an environment variable or a flag)
	// overrides it: it applies at the next start.
	Pending any `json:"pending,omitempty"`
}

// View is the boot configuration of the running daemon.
type View struct {
	// File is the config file the daemon reads (it may not exist yet).
	File       string `json:"file"`
	FileExists bool   `json:"fileExists"`
	// FileError says why the file cannot be applied (it does not parse or
	// holds an invalid value); the entries then show no pending values.
	FileError string `json:"fileError,omitempty"`
	// RestartPending is true when any entry has a pending value.
	RestartPending bool    `json:"restartPending"`
	Entries        []Entry `json:"entries"`
}

// File returns the config file Load consulted (it may not exist).
func (c *Config) File() string { return c.configFile }

// flatten returns every key of cfg with a JSON-friendly value (durations as
// text, integers as int64).
func flatten(cfg *Config) (map[string]any, error) {
	k := koanf.New(".")
	if err := k.Load(structs.Provider(*cfg, "koanf"), nil); err != nil {
		return nil, fmt.Errorf("config: describe: %w", err)
	}
	out := map[string]any{}
	for key, v := range k.All() {
		out[key] = normalise(v)
	}
	return out, nil
}

func normalise(v any) any {
	switch x := v.(type) {
	case time.Duration:
		return x.String()
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case []string:
		return slices.Clone(x)
	case nil:
		return nil
	default:
		return v
	}
}

func typeOf(v any) ValueType {
	switch reflect.TypeOf(v).Kind() {
	case reflect.Bool:
		return TypeBool
	case reflect.Slice:
		return TypeList
	case reflect.Int, reflect.Int32, reflect.Int64:
		return TypeInt
	default:
		return TypeString
	}
}

// keyTypes returns the type of every key, found from the defaults (durations
// are told apart by the declared Go type).
func keyTypes() (map[string]ValueType, error) {
	d := Default()
	k := koanf.New(".")
	if err := k.Load(structs.Provider(d, "koanf"), nil); err != nil {
		return nil, fmt.Errorf("config: describe: %w", err)
	}
	out := map[string]ValueType{}
	for key, v := range k.All() {
		if _, ok := v.(time.Duration); ok {
			out[key] = TypeDuration
			continue
		}
		if v == nil {
			out[key] = TypeList // nil slices of the defaults
			continue
		}
		out[key] = typeOf(v)
	}
	return out, nil
}

// layer builds the configuration the file alone would give (defaults and the
// file, no environment or flags). The home and directories the file does not
// set are those of the running daemon, which an environment variable or a
// flag may have chosen: the file is read from there.
func layer(path string, running *Config) (*Config, error) {
	k := koanf.New(".")
	if err := k.Load(structs.Provider(Default(), "koanf"), nil); err != nil {
		return nil, fmt.Errorf("config: defaults: %w", err)
	}
	fileK := koanf.New(".")
	if err := loadFile(fileK, path, false); err != nil {
		return nil, err
	}
	if err := k.Merge(fileK); err != nil {
		return nil, fmt.Errorf("config: merge file: %w", err)
	}
	cfg := &Config{}
	if err := k.Unmarshal("", cfg); err != nil {
		return nil, fmt.Errorf("config: decode: %w", err)
	}
	if running != nil {
		if !fileK.Exists("home") {
			cfg.Home = running.Home
		}
		for key, dst := range map[string]*string{
			"paths.config_dir": &cfg.Paths.ConfigDir, "paths.data_dir": &cfg.Paths.DataDir,
			"paths.cache_dir": &cfg.Paths.CacheDir, "paths.log_dir": &cfg.Paths.LogDir,
		} {
			if !fileK.Exists(key) {
				*dst = map[string]string{
					"paths.config_dir": running.Paths.ConfigDir, "paths.data_dir": running.Paths.DataDir,
					"paths.cache_dir": running.Paths.CacheDir, "paths.log_dir": running.Paths.LogDir,
				}[key]
			}
		}
	}
	if err := cfg.resolvePaths(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Describe returns the running configuration with the source of every value
// and what the config file would change at the next start.
func (c *Config) Describe() (*View, error) {
	running, err := flatten(c)
	if err != nil {
		return nil, err
	}
	d := Default()
	defaults, err := flatten(&d)
	if err != nil {
		return nil, err
	}
	types, err := keyTypes()
	if err != nil {
		return nil, err
	}
	v := &View{File: c.configFile}
	if v.File != "" {
		if _, err := os.Stat(v.File); err == nil {
			v.FileExists = true
		}
	}
	var pending map[string]any
	if v.File != "" {
		if fileCfg, err := layer(v.File, c); err != nil {
			v.FileError = err.Error()
		} else if pending, err = flatten(fileCfg); err != nil {
			return nil, err
		}
	}
	keys := make([]string, 0, len(running))
	for k := range running {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, key := range keys {
		src := c.sources[key]
		if src == "" {
			src = SourceDefault
		}
		e := Entry{
			Key: key, Type: types[key], Value: running[key], Default: defaults[key], Source: src,
			RestartRequired: true, ReadOnly: readOnlyKeys[key], Secret: secretKeys[key],
		}
		if pending != nil && (src == SourceDefault || src == SourceFile) && !equalValues(pending[key], running[key]) {
			e.Pending = pending[key]
			v.RestartPending = true
		}
		if e.Secret {
			e.Value, e.Default, e.Pending = redacted(e.Value), redacted(e.Default), redacted(e.Pending)
		}
		v.Entries = append(v.Entries, e)
	}
	return v, nil
}

func redacted(v any) any {
	if v == nil || v == "" {
		return v
	}
	return "***"
}

func equalValues(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}

// PatchError says why a change to the config file was refused.
type PatchError struct {
	// Key is the offending key, empty for a problem with the result as a whole.
	Key string
	// Code is config.unknown_key, config.read_only, config.bad_value or
	// config.invalid.
	Code    string
	Message string
}

func (e *PatchError) Error() string { return e.Message }

// Patch changes keys in the config file: a value sets the key, nil removes it
// again (the default applies). The result must load and validate as a whole,
// otherwise nothing is written. Other keys of the file are kept; its comments
// are not. The running daemon is not touched: the change applies at the next
// start. Keys that an environment variable or a flag overrides are written
// all the same.
func (c *Config) Patch(changes map[string]any) error {
	path := c.configFile
	if path == "" {
		return &PatchError{Code: "config.invalid", Message: "config: there is no config file to write"}
	}
	types, err := keyTypes()
	if err != nil {
		return err
	}
	fileK := koanf.New(".")
	if err := loadFile(fileK, path, false); err != nil {
		return &PatchError{Code: "config.invalid", Message: err.Error()}
	}
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, key := range keys {
		typ, known := types[key]
		switch {
		case !known:
			return &PatchError{Key: key, Code: "config.unknown_key", Message: fmt.Sprintf("config: unknown key %q", key)}
		case readOnlyKeys[key]:
			return &PatchError{Key: key, Code: "config.read_only", Message: fmt.Sprintf("config: %q cannot be changed here", key)}
		}
		if changes[key] == nil {
			fileK.Delete(key)
			continue
		}
		val, err := coerce(typ, changes[key])
		if err != nil {
			return &PatchError{Key: key, Code: "config.bad_value", Message: fmt.Sprintf("config: %s: %v", key, err)}
		}
		if err := fileK.Set(key, val); err != nil {
			return &PatchError{Key: key, Code: "config.bad_value", Message: fmt.Sprintf("config: %s: %v", key, err)}
		}
	}
	data, err := fileK.Marshal(yaml.Parser())
	if err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	// The result must be loadable: check it from a scratch file before the
	// real one is replaced.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: create %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("config: write: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: write: %w", err)
	}
	if _, err := layer(tmp.Name(), c); err != nil {
		return &PatchError{Code: "config.invalid", Message: err.Error()}
	}
	mode := fs.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return fmt.Errorf("config: write: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("config: replace %q: %w", path, err)
	}
	return nil
}

// coerce turns a decoded JSON value into what the key's type needs.
func coerce(typ ValueType, v any) (any, error) {
	switch typ {
	case TypeString:
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("want a string")
		}
		return s, nil
	case TypeBool:
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("want true or false")
		}
		return b, nil
	case TypeInt:
		switch n := v.(type) {
		case int:
			return int64(n), nil
		case int64:
			return n, nil
		case float64:
			if n != math.Trunc(n) || math.Abs(n) > 1<<53 {
				return nil, errors.New("want a whole number")
			}
			return int64(n), nil
		default:
			return nil, errors.New("want a whole number")
		}
	case TypeDuration:
		s, ok := v.(string)
		if !ok {
			return nil, errors.New(`want a duration such as "30s" or "5m"`)
		}
		if _, err := time.ParseDuration(strings.TrimSpace(s)); err != nil {
			return nil, fmt.Errorf("invalid duration %q", s)
		}
		return strings.TrimSpace(s), nil
	case TypeList:
		var out []string
		switch l := v.(type) {
		case []string:
			out = slices.Clone(l)
		case []any:
			for _, item := range l {
				s, ok := item.(string)
				if !ok {
					return nil, errors.New("want a list of strings")
				}
				out = append(out, s)
			}
		default:
			return nil, errors.New("want a list of strings")
		}
		if out == nil {
			out = []string{}
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported type %q", typ)
}
