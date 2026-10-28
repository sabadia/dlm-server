package settings

import (
	"reflect"
	"slices"
)

// FieldSchema describes one settings field for generic clients (forms,
// validators). It complements the OpenAPI schema with presentation and
// validation hints that JSON Schema cannot express.
type FieldSchema struct {
	// Path is the dotted JSON path, e.g. "retry.maxAttempts".
	Path string `json:"path" doc:"Dotted JSON path of the field"`
	// Type is one of boolean, integer, number, string, duration, array,
	// object.
	Type string `json:"type" enum:"boolean,integer,number,string,duration,array,object" doc:"Value type; a duration is a Go duration string such as 1m30s"`
	// Group is the name of the section the field belongs to.
	Group string `json:"group" doc:"Section the field belongs to"`
	// Description is a one-line explanation.
	Description string `json:"description" doc:"What the field does"`
	// Unit is the unit of numeric values, when there is one.
	Unit string `json:"unit,omitempty" doc:"Unit of numeric values, e.g. bytes/s or seconds"`
	// Min and Max are the inclusive numeric bounds enforced by validation.
	Min *float64 `json:"min,omitempty" doc:"Smallest accepted value"`
	Max *float64 `json:"max,omitempty" doc:"Largest accepted value"`
	// Enum lists the accepted values of a closed set (or, for arrays, of
	// each element).
	Enum []string `json:"enum,omitempty" doc:"Accepted values of a closed set (of each element for arrays)"`
	// Levels lists the levels that may set the field.
	Levels []Level `json:"levels" doc:"Levels that may set the field"`
	// Secret marks values that must be hidden in UIs and logs.
	Secret bool `json:"secret,omitempty" doc:"The value may hold credentials and must be hidden"`
	// RestartRequired marks fields that only take effect after a daemon
	// restart. No settings field does today; the hint exists for the boot
	// configuration schema.
	RestartRequired bool `json:"restartRequired,omitempty" doc:"Changes only take effect after a daemon restart"`
}

// Schema is the machine-readable description of every settings field.
type Schema struct {
	Fields []FieldSchema `json:"fields" doc:"One entry per settings field, in display order"`
}

// fieldMeta is the hand-written part of a FieldSchema; Type and Levels are
// derived from the Settings struct by reflection.
type fieldMeta struct {
	group  string
	desc   string
	unit   string
	min    *float64
	max    *float64
	enum   []string
	secret bool
}

func bound(v float64) *float64 { return &v }

var allProxyModes = []string{string(ProxyNone), string(ProxySystem), string(ProxyCustom)}

// fieldMetas holds the presentation hints per field path. A test keeps it in
// sync with Settings (every field needs an entry).
var fieldMetas = map[string]fieldMeta{
	"downloadDir":      {group: "general", desc: "Directory files are saved to"},
	"filenameTemplate": {group: "general", desc: "Template for generated file names"},
	"autoCategorize":   {group: "general", desc: "Sort finished downloads into category folders"},
	"fileExistsPolicy": {
		group: "general", desc: "What to do when the target file exists",
		enum: []string{string(FileExistsRename), string(FileExistsOverwrite), string(FileExistsSkip), string(FileExistsResume), string(FileExistsAsk)},
	},
	"checksum": {group: "general", desc: "Expected hash of the downloaded file"},

	"maxConnections":          {group: "speed", desc: "Parallel connections per task", min: bound(1), max: bound(MaxConnectionsLimit)},
	"maxConcurrentDownloads":  {group: "speed", desc: "Tasks a queue runs at once", min: bound(1), max: bound(MaxConcurrentDownloadsLimit)},
	"maxActiveDownloadsTotal": {group: "speed", desc: "Running tasks across all queues; 0 means no cap", min: bound(0), max: bound(10_000)},
	"speedLimit":              {group: "speed", desc: "Speed limit; 0 means unlimited", unit: "bytes/s", min: bound(0)},
	"speedProfiles":           {group: "speed", desc: "Time-of-day speed limits"},
	"quota":                   {group: "speed", desc: "Bytes allowed per period"},

	"retry.maxAttempts":       {group: "retry", desc: "Retries after the first failure", min: bound(0), max: bound(MaxRetryAttemptsLimit)},
	"retry.initialBackoff":    {group: "retry", desc: "Delay before the first retry", unit: "duration"},
	"retry.maxBackoff":        {group: "retry", desc: "Upper bound of the retry delay", unit: "duration"},
	"retry.respectRetryAfter": {group: "retry", desc: "Honour the server's Retry-After header"},

	"proxy":      {group: "network", desc: "Outbound proxy", enum: allProxyModes},
	"proxyRules": {group: "network", desc: "Route matching hosts through a specific proxy"},
	"userAgent":  {group: "network", desc: "User-Agent request header"},
	"headers":    {group: "network", desc: "Extra request headers (may carry credentials)", secret: true},
	"tlsVerify":  {group: "network", desc: "Verify TLS certificates"},

	"refresh.auto":         {group: "refresh", desc: "Refresh expired links automatically"},
	"refresh.strategies":   {group: "refresh", desc: "Strategies tried in order", enum: []string{string(RefreshOriginRedirect), string(RefreshPageReload), string(RefreshExtensionCapture), string(RefreshPlugin), string(RefreshManual)}},
	"refresh.maxAttempts":  {group: "refresh", desc: "Refresh attempts per failure", min: bound(0), max: bound(MaxRefreshAttemptsLimit)},
	"refresh.awaitTimeout": {group: "refresh", desc: "How long to wait for a captured link", unit: "duration"},
	"refresh.onMismatch":   {group: "refresh", desc: "What to do when the refreshed link is a different file", enum: []string{string(MismatchRestart), string(MismatchFail), string(MismatchAsk)}},

	"torrent.seedKeep":  {group: "torrent", desc: "Keep seeding until paused or deleted"},
	"torrent.seedRatio": {group: "torrent", desc: "Stop seeding at this upload/download ratio; 0 means no limit", min: bound(0), max: bound(MaxSeedRatio)},
	"torrent.seedTime":  {group: "torrent", desc: "Stop seeding this long after completion; 0 means no limit", unit: "duration", min: bound(0)},
	"torrent.trackers":  {group: "torrent", desc: "Extra tracker announce URLs"},

	"browserUsage": {group: "browser", desc: "When the internal headless browser is used", enum: []string{string(BrowserNever), string(BrowserAuto), string(BrowserAlways)}},
	"postActions":  {group: "post-processing", desc: "Actions run after a download completes (a command runs a program)"},

	"schedule":        {group: "queue", desc: "When the queue runs"},
	"onQueueComplete": {group: "queue", desc: "Action run when the queue runs out of work", enum: []string{string(QueueActionNone), string(QueueActionStop), string(QueueActionSleep), string(QueueActionShutdown), string(QueueActionCommand)}},
}

// allLevels is the level set of a field without a levels restriction.
var allLevels = []Level{LevelDefault, LevelGlobal, LevelQueue, LevelTask}

// DescribeSchema returns the schema of every settings field in struct order.
func DescribeSchema() Schema {
	fs := fields()
	out := Schema{Fields: make([]FieldSchema, 0, len(fs))}
	st := reflect.TypeFor[Settings]()
	for _, f := range fs {
		meta := fieldMetas[f.path]
		levels := allLevels
		if f.levels != nil {
			levels = append([]Level{LevelDefault}, f.levels...)
		}
		out.Fields = append(out.Fields, FieldSchema{
			Path:        f.path,
			Type:        schemaType(valueType(st.FieldByIndex(f.index).Type)),
			Group:       meta.group,
			Description: meta.desc,
			Unit:        meta.unit,
			Min:         meta.min,
			Max:         meta.max,
			Enum:        slices.Clone(meta.enum),
			Levels:      slices.Clone(levels),
			Secret:      meta.secret,
		})
	}
	return out
}

// valueType returns T of an Optional[T].
func valueType(optional reflect.Type) reflect.Type { return optional.Field(0).Type }

var durationType = reflect.TypeFor[Duration]()

func schemaType(t reflect.Type) string {
	switch {
	case t == durationType:
		return "duration"
	case t.Kind() == reflect.Bool:
		return "boolean"
	case t.Kind() >= reflect.Int && t.Kind() <= reflect.Uint64:
		return "integer"
	case t.Kind() == reflect.Float32 || t.Kind() == reflect.Float64:
		return "number"
	case t.Kind() == reflect.String:
		return "string"
	case t.Kind() == reflect.Slice || t.Kind() == reflect.Array:
		return "array"
	default:
		return "object"
	}
}
