package settings

import (
	"reflect"
	"slices"
	"strings"
	"sync"
)

// Effective is the result of resolving all layers for one task (or queue).
type Effective struct {
	// Settings holds the resolved values. Unset fields have no value at any
	// level (for example no checksum). Maps and slices are shared with the
	// input layers and must be treated as read-only.
	Settings Settings `json:"settings"`
	// Sources maps each set field's JSON path (e.g. "retry.maxAttempts") to
	// the level its value came from.
	Sources map[string]Level `json:"sources"`
}

// Layers are the inputs of Resolve. Queue and Task may be zero values.
type Layers struct {
	Defaults Settings
	Global   Settings
	Queue    Settings
	Task     Settings
}

// Resolve computes the effective settings: for every field the value of the
// highest level that sets it wins (Task > Queue > Global > Defaults).
func Resolve(l Layers) Effective {
	eff := Effective{Sources: make(map[string]Level)}
	out := reflect.ValueOf(&eff.Settings).Elem()
	// Nearest level first.
	ordered := []struct {
		level Level
		value reflect.Value
	}{
		{LevelTask, reflect.ValueOf(l.Task)},
		{LevelQueue, reflect.ValueOf(l.Queue)},
		{LevelGlobal, reflect.ValueOf(l.Global)},
		{LevelDefault, reflect.ValueOf(l.Defaults)},
	}
	for _, f := range fields() {
		for _, layer := range ordered {
			v := layer.value.FieldByIndex(f.index)
			if v.Interface().(optionalValue).isSet() { //nolint:errcheck,forcetypeassert // fields() only yields optionalValue types
				out.FieldByIndex(f.index).Set(v)
				eff.Sources[f.path] = layer.level
				break
			}
		}
	}
	// Proxy rules are a global default: an explicit queue or task proxy
	// overrides them entirely.
	if src, ok := eff.Sources["proxy"]; ok && (src == LevelQueue || src == LevelTask) {
		eff.Settings.ProxyRules = None[[]ProxyRule]()
		delete(eff.Sources, "proxyRules")
	}
	return eff
}

// Merge returns base with every field set in override applied on top.
func Merge(base, override Settings) Settings {
	out := base
	dst := reflect.ValueOf(&out).Elem()
	src := reflect.ValueOf(override)
	for _, f := range fields() {
		v := src.FieldByIndex(f.index)
		if v.Interface().(optionalValue).isSet() { //nolint:errcheck,forcetypeassert // fields() only yields optionalValue types
			dst.FieldByIndex(f.index).Set(v)
		}
	}
	return out
}

// SetPaths returns the JSON paths of all fields set in s, sorted.
func SetPaths(s Settings) []string {
	v := reflect.ValueOf(s)
	var paths []string
	for _, f := range fields() {
		if v.FieldByIndex(f.index).Interface().(optionalValue).isSet() { //nolint:errcheck,forcetypeassert // fields() only yields optionalValue types
			paths = append(paths, f.path)
		}
	}
	slices.Sort(paths)
	return paths
}

// field describes one Optional leaf of Settings.
type field struct {
	path   string  // JSON path, e.g. "retry.maxAttempts"
	index  []int   // reflect field index path
	levels []Level // allowed non-default levels; nil means all
}

// allowed reports whether level may set this field.
func (f field) allowed(level Level) bool {
	return level == LevelDefault || f.levels == nil || slices.Contains(f.levels, level)
}

var fields = sync.OnceValue(func() []field {
	return collectFields(reflect.TypeFor[Settings](), "", nil, nil)
})

var optionalType = reflect.TypeFor[optionalValue]()

func collectFields(t reflect.Type, prefix string, index []int, inherited []Level) []field {
	var out []field
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			panic("settings: field " + sf.Name + " needs a json name") // programming error caught by tests
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		idx := append(slices.Clone(index), i)
		levels := inherited
		if tag, ok := sf.Tag.Lookup("levels"); ok {
			levels = parseLevels(tag)
		}
		switch {
		case sf.Type.Implements(optionalType):
			out = append(out, field{path: path, index: idx, levels: levels})
		case sf.Type.Kind() == reflect.Struct:
			out = append(out, collectFields(sf.Type, path, idx, levels)...)
		default:
			panic("settings: field " + path + " must be Optional[T] or a group struct") // programming error caught by tests
		}
	}
	return out
}

func parseLevels(tag string) []Level {
	var levels []Level
	for name := range strings.SplitSeq(tag, ",") {
		var l Level
		if err := l.UnmarshalText([]byte(strings.TrimSpace(name))); err != nil {
			panic("settings: bad levels tag " + tag) // programming error caught by tests
		}
		levels = append(levels, l)
	}
	return levels
}
