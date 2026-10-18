package settings

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
)

// Level identifies where a setting value is defined. Higher levels override
// lower ones: Task > Queue > Global > Default.
type Level int

// Settings levels, from lowest to highest precedence.
const (
	LevelDefault Level = iota
	LevelGlobal
	LevelQueue
	LevelTask
)

var levelNames = [...]string{"default", "global", "queue", "task"}

// String returns the lower-case level name.
func (l Level) String() string {
	if l < LevelDefault || l > LevelTask {
		return fmt.Sprintf("level(%d)", int(l))
	}
	return levelNames[l]
}

// MarshalText implements encoding.TextMarshaler.
func (l Level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (l *Level) UnmarshalText(text []byte) error {
	i := slices.Index(levelNames[:], string(text))
	if i < 0 {
		return fmt.Errorf("settings: unknown level %q", text)
	}
	*l = Level(i)
	return nil
}

// Settings is the layered download configuration. The same struct is used for
// defaults, the global layer, each queue's overrides and each task's
// overrides; unset fields inherit from the next lower level.
//
// The `levels` struct tag restricts which layers may set a field (defaults
// may always set it). Fields without the tag are allowed at every level.
type Settings struct {
	// DownloadDir is the directory files are saved to.
	DownloadDir Optional[string] `json:"downloadDir,omitzero"`
	// FilenameTemplate overrides the generated file name (placeholders are
	// expanded by the engine).
	FilenameTemplate Optional[string] `json:"filenameTemplate,omitzero"`
	// MaxConnections is the maximum number of parallel connections per task.
	MaxConnections Optional[int] `json:"maxConnections,omitzero"`
	// MaxConcurrentDownloads is the number of tasks a queue runs at once.
	// At the global level it is the default for queues.
	MaxConcurrentDownloads Optional[int] `json:"maxConcurrentDownloads,omitzero" levels:"global,queue"`
	// MaxActiveDownloadsTotal caps running tasks across all queues; 0 means
	// no global cap.
	MaxActiveDownloadsTotal Optional[int] `json:"maxActiveDownloadsTotal,omitzero" levels:"global"`
	// SpeedLimit is in bytes per second; 0 means unlimited. Limits nest:
	// a task is bounded by its own, its queue's and the global limit.
	SpeedLimit Optional[int64] `json:"speedLimit,omitzero"`
	// SpeedProfiles are time-of-day speed limits: while one of a
	// profile's windows is active its SpeedLimit replaces the level's
	// SpeedLimit (the first active profile in list order wins). Global
	// profiles apply to the global limit, queue profiles to that queue's
	// own limit; like SpeedLimit, a queue's profiles are not inherited
	// from the global layer.
	SpeedProfiles Optional[[]SpeedProfile] `json:"speedProfiles,omitzero" levels:"global,queue"`
	// Quota caps the bytes downloaded per period. The global quota counts
	// every byte, a queue's quota only that queue's bytes (it is not
	// inherited from the global layer). See the scheduler package for the
	// exact semantics.
	Quota Optional[Quota] `json:"quota,omitzero" levels:"global,queue"`

	Retry RetrySettings `json:"retry,omitzero"`

	Proxy Optional[Proxy] `json:"proxy,omitzero"`
	// ProxyRules route requests to matching hosts through a specific proxy.
	// They are global-only and apply only when neither the queue nor the
	// task sets Proxy explicitly (see Resolve); the first matching rule
	// wins, otherwise Proxy applies.
	ProxyRules Optional[[]ProxyRule] `json:"proxyRules,omitzero" levels:"global"`
	UserAgent  Optional[string]      `json:"userAgent,omitzero"`
	// Headers are extra request headers. The map is replaced as a whole by
	// the nearest level that sets it, not merged key by key.
	Headers   Optional[map[string]string] `json:"headers,omitzero"`
	TLSVerify Optional[bool]              `json:"tlsVerify,omitzero"`

	FileExistsPolicy Optional[FileExistsPolicy] `json:"fileExistsPolicy,omitzero"`
	// Checksum is the expected file hash; it only makes sense per task.
	Checksum Optional[Checksum] `json:"checksum,omitzero" levels:"task"`

	Refresh RefreshSettings `json:"refresh,omitzero"`

	Torrent TorrentSettings `json:"torrent,omitzero"`

	BrowserUsage   Optional[BrowserUsage] `json:"browserUsage,omitzero"`
	PostActions    Optional[[]PostAction] `json:"postActions,omitzero"`
	AutoCategorize Optional[bool]         `json:"autoCategorize,omitzero"`

	// Schedule defines when a queue runs; it only makes sense per queue.
	Schedule Optional[Schedule] `json:"schedule,omitzero" levels:"queue"`
	// OnQueueComplete is run when the queue runs out of work after
	// completing at least one task; it only makes sense per queue.
	OnQueueComplete Optional[QueueCompleteAction] `json:"onQueueComplete,omitzero" levels:"queue"`
}

// RetrySettings controls retries of transient failures.
type RetrySettings struct {
	// MaxAttempts is the number of retries after the first failure.
	MaxAttempts       Optional[int]      `json:"maxAttempts,omitzero"`
	InitialBackoff    Optional[Duration] `json:"initialBackoff,omitzero"`
	MaxBackoff        Optional[Duration] `json:"maxBackoff,omitzero"`
	RespectRetryAfter Optional[bool]     `json:"respectRetryAfter,omitzero"`
}

// RefreshSettings controls download-link refresh.
type RefreshSettings struct {
	Auto        Optional[bool]              `json:"auto,omitzero"`
	Strategies  Optional[[]RefreshStrategy] `json:"strategies,omitzero"`
	MaxAttempts Optional[int]               `json:"maxAttempts,omitzero"`
	// AwaitTimeout bounds the extension-capture wait.
	AwaitTimeout Optional[Duration]       `json:"awaitTimeout,omitzero"`
	OnMismatch   Optional[MismatchPolicy] `json:"onMismatch,omitzero"`
}

// TorrentSettings controls BitTorrent downloads (magnet links and .torrent
// files).
type TorrentSettings struct {
	// SeedKeep keeps a finished torrent seeding until the task is paused or
	// deleted, whatever SeedRatio and SeedTime say.
	SeedKeep Optional[bool] `json:"seedKeep,omitzero"`
	// SeedRatio stops seeding once the bytes uploaded reach this multiple
	// of the torrent's size; 0 means no ratio limit.
	SeedRatio Optional[float64] `json:"seedRatio,omitzero"`
	// SeedTime stops seeding this long after the download finished; 0 means
	// no time limit. Seeding ends at whichever limit is reached first, and
	// a torrent with neither limit (and no SeedKeep) is not seeded at all.
	SeedTime Optional[Duration] `json:"seedTime,omitzero"`
	// Trackers are announce URLs (http, https, udp, wss) added to every
	// torrent of the task, except private ones.
	Trackers Optional[[]string] `json:"trackers,omitzero"`
}

// ProxyMode selects how requests are proxied.
type ProxyMode string

// Proxy modes.
const (
	ProxyNone   ProxyMode = "none"
	ProxySystem ProxyMode = "system"
	ProxyCustom ProxyMode = "custom"
)

// Proxy configures the outbound proxy. URL is required for ProxyCustom and
// may embed credentials (http, https, socks5, socks5h schemes).
type Proxy struct {
	Mode ProxyMode `json:"mode"`
	URL  string    `json:"url,omitempty"`
}

// ProxyRule sends requests whose host matches any of Hosts through Proxy.
// Host patterns are case-insensitive globs ("*.example.com", "10.*",
// "files.example.org"); see MatchHost.
type ProxyRule struct {
	Hosts []string `json:"hosts"`
	Proxy Proxy    `json:"proxy"`
}

// MatchHost reports whether host matches the glob pattern ("*" matches any
// sequence of characters including dots, "?" one character),
// case-insensitively. A malformed pattern never matches.
func MatchHost(pattern, host string) bool {
	ok, err := path.Match(strings.ToLower(pattern), strings.ToLower(host))
	return err == nil && ok
}

// ProxyFor returns the proxy for host: the first matching ProxyRule, else
// Proxy, else the system proxy.
func (s *Settings) ProxyFor(host string) Proxy {
	for _, r := range s.ProxyRules.Value() {
		for _, p := range r.Hosts {
			if MatchHost(p, host) {
				return r.Proxy
			}
		}
	}
	return s.Proxy.OrElse(Proxy{Mode: ProxySystem})
}

// FileExistsPolicy decides what happens when the target file exists.
type FileExistsPolicy string

// File-exists policies.
const (
	FileExistsRename    FileExistsPolicy = "rename"
	FileExistsOverwrite FileExistsPolicy = "overwrite"
	FileExistsSkip      FileExistsPolicy = "skip"
	FileExistsResume    FileExistsPolicy = "resume"
	FileExistsAsk       FileExistsPolicy = "ask"
)

// ChecksumAlgorithm names a supported hash.
type ChecksumAlgorithm string

// Checksum algorithms.
const (
	ChecksumMD5    ChecksumAlgorithm = "md5"
	ChecksumSHA1   ChecksumAlgorithm = "sha1"
	ChecksumSHA256 ChecksumAlgorithm = "sha256"
	ChecksumBLAKE3 ChecksumAlgorithm = "blake3"
)

// Checksum is an expected hash of the downloaded file (hex encoded).
type Checksum struct {
	Algorithm ChecksumAlgorithm `json:"algorithm"`
	Expected  string            `json:"expected"`
}

// RefreshStrategy names a link-refresh strategy.
type RefreshStrategy string

// Refresh strategies, tried in the configured order.
const (
	RefreshOriginRedirect   RefreshStrategy = "origin-redirect"
	RefreshPageReload       RefreshStrategy = "page-reload"
	RefreshExtensionCapture RefreshStrategy = "extension-capture"
	RefreshPlugin           RefreshStrategy = "plugin"
	RefreshManual           RefreshStrategy = "manual"
)

// MismatchPolicy decides what happens when a refreshed link points to a
// different file than the partial download.
type MismatchPolicy string

// Mismatch policies.
const (
	MismatchRestart MismatchPolicy = "restart"
	MismatchFail    MismatchPolicy = "fail"
	MismatchAsk     MismatchPolicy = "ask"
)

// BrowserUsage controls when the internal headless browser is used.
type BrowserUsage string

// Browser usage modes.
const (
	BrowserNever  BrowserUsage = "never"
	BrowserAuto   BrowserUsage = "auto"
	BrowserAlways BrowserUsage = "always"
)

// Post-action kinds.
const (
	// PostActionExtract unpacks an archive (zip, rar, 7z, tar and its
	// compressed forms; multi-volume rar sets). Parameters: dir (where to
	// unpack: default a folder named after the archive, "." for the archive's
	// own folder), delete ("true" removes the archive after unpacking),
	// password.
	PostActionExtract = "extract"
	// PostActionAddTorrent turns a downloaded .torrent file into a torrent
	// task. Parameters: dir (download directory of the torrent; default the
	// folder of the .torrent file), delete ("true" removes the .torrent file).
	PostActionAddTorrent = "add-torrent"
	// PostActionCommand runs a program on the downloaded file. Parameters:
	// cmd (absolute path of the program, required; run directly, not through
	// a shell), args (JSON array of strings; {path}, {dir}, {name}, {taskId},
	// {url} and {size} are replaced; default [{path}]), timeout (default 5m).
	PostActionCommand = "command"
	// PostActionAVScan runs a virus scanner on the downloaded file.
	// Parameters: cmd, args, timeout as for command, infectedExit (exit status
	// that means "infected", default 1), onInfected ("delete", the default, or
	// "keep"). An infected file fails the task with the error code infected.
	PostActionAVScan = "av-scan"
)

// postActionParams lists, per kind, the parameters it understands.
var postActionParams = map[string][]string{
	PostActionExtract:    {"dir", "delete", "password"},
	PostActionAddTorrent: {"dir", "delete"},
	PostActionCommand:    {"cmd", "args", "timeout"},
	PostActionAVScan:     {"cmd", "args", "timeout", "infectedExit", "onInfected"},
}

// PostAction is an action run after a download completes: Kind names it (see
// the PostAction constants) and Params configure it. Actions run in order and
// never fail the task: a failure is recorded as a warning on the task.
type PostAction struct {
	Kind   string            `json:"kind"`
	Params map[string]string `json:"params,omitempty"`
}

// Schedule restricts when a queue runs to the given time windows.
type Schedule struct {
	// Timezone is an IANA zone name; empty means the local zone.
	Timezone string       `json:"timezone,omitempty"`
	Windows  []TimeWindow `json:"windows"`
}

// TimeWindow is a daily window on the given weekdays. Start and End are
// "HH:MM" (24h). An End earlier than Start spans midnight.
type TimeWindow struct {
	Days  []Weekday `json:"days"`
	Start string    `json:"start"`
	End   string    `json:"end"`
}

// SpeedProfile is a speed limit that applies while any of its windows is
// active.
type SpeedProfile struct {
	// Timezone is an IANA zone name; empty means the local zone.
	Timezone string       `json:"timezone,omitempty"`
	Windows  []TimeWindow `json:"windows"`
	// SpeedLimit is in bytes per second; 0 means unlimited.
	SpeedLimit int64 `json:"speedLimit"`
}

// MinQuotaPeriod is the shortest allowed Quota.Period.
const MinQuotaPeriod = time.Minute

// Quota limits the bytes downloaded per period (for example 500 MB per
// hour: {"bytes": 524288000, "period": "1h"}).
type Quota struct {
	// Bytes is the number of bytes allowed per period; it must be > 0.
	Bytes int64 `json:"bytes"`
	// Period is the length of one accounting window; at least
	// MinQuotaPeriod.
	Period Duration `json:"period"`
}

// QueueAction names what happens when a queue completes its work.
type QueueAction string

// Post-queue actions.
const (
	QueueActionNone     QueueAction = "none"
	QueueActionStop     QueueAction = "stop"
	QueueActionSleep    QueueAction = "sleep"
	QueueActionShutdown QueueAction = "shutdown"
	QueueActionCommand  QueueAction = "command"
)

// Delay bounds of a QueueCompleteAction.
const (
	DefaultQueueActionDelay = time.Minute
	MaxQueueActionDelay     = 24 * time.Hour
)

// QueueCompleteAction is the action run after a queue finishes its work.
type QueueCompleteAction struct {
	Action QueueAction `json:"action"`
	// Command is the absolute path of the executable run by the "command"
	// action. It is executed directly with Args, never through a shell.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Delay is the grace period between the queue completing and the
	// action running, during which new work or stopping the queue cancels
	// it; 0 means DefaultQueueActionDelay.
	Delay Duration `json:"delay,omitzero"`
}

// EffectiveDelay returns Delay, or DefaultQueueActionDelay when it is 0.
func (a QueueCompleteAction) EffectiveDelay() time.Duration {
	if a.Delay <= 0 {
		return DefaultQueueActionDelay
	}
	return a.Delay.Std()
}

// Weekday is a time.Weekday encoded as a three-letter lower-case name.
type Weekday time.Weekday

var weekdayNames = [...]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// String returns the three-letter weekday name.
func (w Weekday) String() string {
	if w < 0 || int(w) >= len(weekdayNames) {
		return fmt.Sprintf("weekday(%d)", int(w))
	}
	return weekdayNames[w]
}

// MarshalText implements encoding.TextMarshaler.
func (w Weekday) MarshalText() ([]byte, error) {
	if w < 0 || int(w) >= len(weekdayNames) {
		return nil, fmt.Errorf("settings: invalid weekday %d", int(w))
	}
	return []byte(w.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (w *Weekday) UnmarshalText(text []byte) error {
	i := slices.Index(weekdayNames[:], strings.ToLower(string(text)))
	if i < 0 {
		return fmt.Errorf("settings: unknown weekday %q", text)
	}
	*w = Weekday(i)
	return nil
}

// DefaultUserAgent is a mainstream browser user agent; many servers refuse
// or throttle unknown clients.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

// Defaults returns the built-in lowest settings layer. downloadDir is the
// default download directory (normally the user's Downloads folder).
func Defaults(downloadDir string) Settings {
	return Settings{
		DownloadDir:             Some(downloadDir),
		MaxConnections:          Some(16),
		MaxConcurrentDownloads:  Some(4),
		MaxActiveDownloadsTotal: Some(0),
		SpeedLimit:              Some[int64](0),
		Retry: RetrySettings{
			MaxAttempts:       Some(5),
			InitialBackoff:    Some(Duration(time.Second)),
			MaxBackoff:        Some(Duration(time.Minute)),
			RespectRetryAfter: Some(true),
		},
		Proxy:            Some(Proxy{Mode: ProxySystem}),
		UserAgent:        Some(DefaultUserAgent),
		TLSVerify:        Some(true),
		FileExistsPolicy: Some(FileExistsRename),
		Refresh: RefreshSettings{
			Auto:         Some(true),
			Strategies:   Some([]RefreshStrategy{RefreshOriginRedirect, RefreshPageReload, RefreshExtensionCapture}),
			MaxAttempts:  Some(3),
			AwaitTimeout: Some(Duration(10 * time.Minute)),
			OnMismatch:   Some(MismatchFail),
		},
		Torrent: TorrentSettings{
			SeedKeep:  Some(false),
			SeedRatio: Some(1.0),
			SeedTime:  Some(Duration(2 * time.Hour)),
		},
		BrowserUsage:   Some(BrowserAuto),
		AutoCategorize: Some(false),
	}
}
