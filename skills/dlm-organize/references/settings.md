# Settings reference

Levels: **G** global, **Q** queue, **T** task. A value set at a nearer level wins
(Defaults → G → Q → T), except speed limits, which nest: all of them apply.

Set with `dlm settings set path=value`, `dlm queue set ID --set path=value`,
`dlm task set ID --set path=value`, or `--set` on `task add`. Remove with
`dlm settings unset path` or `--unset path`. Durations look like `30s`, `5m`, `2h`.

## Contents

- [Fields](#fields)
- [Object shapes](#object-shapes)

## Fields

| Path | Type | Levels | Default | Meaning |
|---|---|---|---|---|
| `downloadDir` | absolute path | GQT | the user's Downloads | Where files are saved. |
| `filenameTemplate` | string | GQT | none | Accepted but has no effect. Use `--name`. |
| `maxConnections` | int 1-128 | GQT | 16 | Connections per task. |
| `maxConcurrentDownloads` | int 1-100 | GQ | 4 | Tasks a queue runs at once. |
| `maxActiveDownloadsTotal` | int 0-10000 | G | 0 | Running tasks across all queues; 0 is no cap. |
| `retentionDays` | int 0-36500 | G | 0 | Remove completed tasks from the list after this long, hourly. 0 keeps them. Files are never deleted. |
| `speedLimit` | bytes/s | GQT | 0 | 0 is unlimited. |
| `speedProfiles` | list | GQ | none | Time-of-day speed limits. |
| `quota` | object | GQ | none | Bytes allowed per period. |
| `retry.maxAttempts` | int 0-100 | GQT | 5 | Retries after the first failure. |
| `retry.initialBackoff` | duration | GQT | 1s | Delay before the first retry. |
| `retry.maxBackoff` | duration | GQT | 1m | Longest delay; at least `initialBackoff`. |
| `retry.respectRetryAfter` | bool | GQT | true | Honour `Retry-After`. |
| `proxy` | object | GQT | `{"mode":"system"}` | Outbound proxy. |
| `proxyRules` | list | G | none | A proxy per host pattern. Ignored when a queue or task sets its own proxy. |
| `userAgent` | string | GQT | a Chrome UA | `User-Agent` header. |
| `headers` | object | GQT | none | Extra request headers. May carry credentials. |
| `tlsVerify` | bool | GQT | true | Verify certificates. Turn off only for a host the user trusts, and on the task rather than globally. |
| `fileExistsPolicy` | enum | GQT | `rename` | `rename`, `overwrite`, `skip`, `resume`, or `ask`, which fails with `file_exists` rather than prompting. |
| `checksum` | object | T | none | Expected hash. `--checksum ALGO:HEX` sets it. |
| `refresh.auto` | bool | GQT | true | Refresh expired links automatically. |
| `refresh.strategies` | list | GQT | origin-redirect, page-reload, extension-capture | Tried in order. Also `plugin`, `manual`. |
| `refresh.maxAttempts` | int 0-20 | GQT | 3 | Refresh attempts per failure. |
| `refresh.awaitTimeout` | duration | GQT | 10m | How long to wait for a captured link. |
| `refresh.onMismatch` | enum | GQT | `fail` | New link is a different file: `restart`, `fail`, `ask`. |
| `torrent.seedKeep` | bool | GQT | false | Seed until paused or deleted. |
| `torrent.seedRatio` | number 0-1000 | GQT | 1 | Stop at this ratio; 0 is no limit. |
| `torrent.seedTime` | duration | GQT | 2h | Stop seeding after this long; 0 is no limit. With ratio 0 too, no seeding happens. |
| `torrent.trackers` | list | GQT | none | Extra announce URLs. |
| `browserUsage` | enum | GQT | `auto` | When the headless browser may be used: `never`, `auto`, `always`. |
| `postActions` | list | GQT | none | Run after a download. They can run programs. |
| `autoCategorize` | bool | GQT | false | Sort finished files into category folders. |
| `schedule` | object | Q | none | When the queue runs. |
| `onQueueComplete` | object | Q | none | What happens when the queue runs out of work. |

## Object shapes

```jsonc
// schedule (speedProfiles entries have the same windows, plus speedLimit)
{"timezone": "Asia/Dhaka",       // IANA name; empty means local
 "windows": [{"days": ["mon", "fri"], "start": "23:00", "end": "06:30"}]}  // end < start spans midnight

// speedProfiles
[{"timezone": "UTC", "windows": [ /* as above */ ], "speedLimit": 1048576}]

// quota: bytes > 0, period at least 1m
{"bytes": 10737418240, "period": "24h"}

// proxy: mode none | system | custom; url for custom (HTTP or SOCKS5)
{"mode": "custom", "url": "socks5://127.0.0.1:1080"}

// proxyRules: host patterns such as *.example.com
[{"hosts": ["*.example.com"], "proxy": {"mode": "custom", "url": "http://proxy:3128"}}]

// checksum: md5, sha1, sha256 or blake3
{"algorithm": "sha256", "expected": "9f86d0…"}

// onQueueComplete: none | stop | sleep | shutdown | command
{"action": "command", "command": "/usr/local/bin/notify", "args": ["done"], "delay": "2m"}

// postActions: params are all strings; args is a JSON array inside a string
[{"kind": "extract", "params": {"dir": ".", "delete": "true", "password": "x"}},
 {"kind": "add-torrent", "params": {"delete": "true"}},
 {"kind": "command", "params": {"cmd": "/usr/bin/touch", "args": "[\"{path}.done\"]", "timeout": "1m"}},
 {"kind": "av-scan", "params": {"cmd": "/usr/bin/clamscan", "args": "[\"{path}\"]", "infectedExit": "1", "onInfected": "delete"}}]
```

Whole-layer replacement: `dlm settings replace -f file.json` and
`dlm queue replace-settings ID -f file.json` overwrite every override with the file's
contents. `dlm settings get` first, because anything you leave out is lost.
