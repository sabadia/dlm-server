# Using the REST API

The daemon (`dlm serve`) serves a REST API under `/v1`. Everything here is also
available as a command: `dlm COMMAND --help`, and [CLI.md](CLI.md) lists them all.
The authoritative description of every operation, field and error is the OpenAPI
3.1 document: [openapi.json](openapi.json) in this repository, `GET /openapi.json`
and a browsable UI at `/docs` on a running daemon, and `dlm_<version>_openapi.json`
as a release asset (feed it to a client generator).

## Connecting

The daemon listens on `127.0.0.1:17171` (`--address`, or `server.address` in the
config file). It refuses to bind a non-loopback address unless `--allow-remote` is
given together with a TLS certificate and key (`--tls-cert`, `--tls-key`).

Every request under `/v1` needs the API token, except `GET /v1/health`:

```sh
TOKEN=$(cat "$(dlm token show -o json | jq -r .path)")      # the token file, mode 0600
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:17171/v1/queues
```

The token is created on first start. Without it, requests get `401` with a
`WWW-Authenticate: Bearer` header. A browser page can only call the API when its
origin is listed in `server.allowed_origins` (the extension's origin, for
example); requests from other origins are refused, and there is no wildcard.

### Finding and controlling a daemon

A program that starts the daemon itself (a desktop app, a service manager) should
not fight over port 17171: start it with `--address 127.0.0.1:0` and the operating
system picks a free port. While it runs, the daemon holds an exclusive lock on
`dlm.lock` in its config directory (a second daemon for the same config directory
refuses to start) and publishes `runtime.json` there, written atomically:

```json
{"pid": 4242, "address": "127.0.0.1:50321", "url": "http://127.0.0.1:50321",
 "version": "0.3.0", "apiVersion": 1, "startedAt": "2026-10-29T12:00:00Z",
 "tlsFingerprint": "sha256:…"}
```

`tlsFingerprint` (the SHA-256 of the leaf certificate) is present only when the API
is served over TLS; remote clients pin it. The file is removed on a clean stop; a
file left behind by a crash is not trustworthy unless the lock is held, which is
what `dlm` checks before using its address. So `dlm --home <dir> …` reaches a
daemon on a random port without `--address`; an explicit `--address`,
`DLM_SERVER_ADDRESS` or `--server` always wins.

`GET /v1/system/info` describes the daemon (version, API version, commit, OS and
architecture, pid, uptime, listen address, directories, enabled engines, database
size, and the status of ffmpeg, yt-dlp and the browser — found or not, never
downloaded by this call). `local` is true when the request came from the daemon's
own machine. `POST /v1/system/shutdown` stops the daemon gracefully (running
downloads are requeued); `POST /v1/system/restart` stops it and starts it again with
the same arguments, keeping the pid on Linux and macOS. Both answer `202` before the
daemon goes down. CLI: `dlm system info|stop|restart`.

## Conventions

- **JSON** field names are `camelCase`, enums are lower-case strings
  (`"running"`, `"awaiting_link"`), timestamps are RFC 3339 in UTC, sizes are
  bytes (`-1` means unknown).
- **Errors** are [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem details
  (`application/problem+json`): `status`, `title`, `detail`, and `errors[]` with a
  `location` for validation failures. The status says what happened: `404` unknown
  id, `409` conflict (duplicate download, name taken), `422` invalid input, `503`
  while the daemon is starting or a feature is not wired. Every problem also has
  a stable machine-readable `code` (for example `task.not_found`,
  `queue.name_taken`, `task.duplicate`, `settings.invalid`) and a `params` map
  with the values of the message (`{"taskId": "…"}`); its `type` is
  `urn:dlm:problem:<code>`. Clients localise their own message from the code and
  keep `detail` as the English fallback. Validation failures keep one entry per
  field in `errors[]`. Codes are part of the API: new ones may appear, existing
  ones do not change meaning.
- **Settings** are layered Global → Queue → Task. A value that is absent or `null`
  inherits from the layer above. `GET /v1/tasks/{id}/settings/effective` returns
  every setting with the level it comes from.
- **Partial updates** use `PATCH`. Settings accept an
  [RFC 7396](https://www.rfc-editor.org/rfc/rfc7396) merge patch
  (`Content-Type: application/merge-patch+json`): a field is set, or removed with
  `null`.
- **Lost updates**: `GET /v1/settings` returns an `ETag`; send it as `If-Match`
  with `PUT` or `PATCH /v1/settings` to update only if nobody changed the
  settings since (`412` with code `settings.changed` otherwise). Without the
  header the update is unconditional.
- **Settings schema**: `GET /v1/settings/schema` lists every settings field with its
  type, group, unit, numeric bounds, accepted values and the levels that may set it,
  so a client can build a settings form without hard-coding the fields.
- **Versions**: `GET /v1/health` reports `apiVersion` (the revision of this API) and
  `minClientApi` (the oldest client revision still supported). The OpenAPI document is
  served as 3.1 at `/openapi.json` and as a 3.0.3 downgrade at `/openapi-3.0.json`
  (`dlm openapi --oas 3.0`); both are release assets.
- **Lists** are paged with a cursor: pass the `nextCursor` of a page as `cursor` to
  get the next one, with the same `sort`. `GET /v1/tasks` filters by `queueId`,
  `status`, and `q` (text in the name or URL).
- **Bulk operations** (pause, resume, delete, create-batch, import) answer `200`
  with one result per item: `{"id", "ok", "task", "error"}`. Check each `ok`.

## Adding and following downloads

```sh
# Add a download (the daemon probes the link, picks an engine and queues it).
curl -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"url": "https://example.com/big.iso", "queueId": "main"}' \
  http://127.0.0.1:17171/v1/tasks

# Two steps: look first, then create from what was found.
curl -X POST ... -d '{"url": "https://example.com/big.iso"}' http://127.0.0.1:17171/v1/resolve
curl -X POST ... -d '{"resolveId": "<id from the answer>"}'  http://127.0.0.1:17171/v1/tasks
```

Useful fields of `POST /v1/tasks`: `name`, `dir`, `paused`, `priority`, `labels`,
`settings` (task-level overrides), and `source` instead of `url` for the full
description of a link (`referrer`, `pageUrl`, `headers`, `mirrors`, ...). A link
already downloaded or queued is refused with `409` unless `allowDuplicate` is set.
`POST /v1/tasks/batch` creates many; with `expandPatterns` a range in a URL such
as `img[001-100].jpg` becomes a task per value.

A task moves through `queued → running → postprocessing → completed`, with
`paused`, `awaiting_link`, `refreshing` and `error` on the side.

## Finding and changing many tasks

`GET /v1/tasks` filters by `status`, `queueId`, `q` (name or URL), `engine`, `label` (`key` or
`key=value`), `category` (ID or name: the category's extensions and MIME types), `minSize` /
`maxSize`, and `createdAfter` / `createdBefore` / `completedAfter` / `completedBefore` (RFC 3339).
`sort` is `position` (queue order), `created`, `name`, `size` or `completed`, with `order=asc|desc`
(a cursor is only valid for the sort and order it was issued for); `total=true` adds the number of
matching tasks. `GET /v1/queues` answers `counts`: per queue ID, the tasks per status.

`POST /v1/tasks/move`, `/update`, `/restart` and `/refresh` (CLI: `dlm task move-tasks`,
`update-tasks`, `restart-tasks`, `refresh-tasks`) take the same selector as pause and delete (`ids`
or a `filter`) and answer one result per task. `update` sets the `priority`, adds or changes
`labels`, drops `removeLabels`, patches the `settings` overrides and/or moves the tasks (`queueId`).

## Adding downloads

- `POST /v1/tasks/expand` (`dlm task expand`): how many URLs a pattern such as
  `https://host/img[001-100].jpg` stands for, and the first ones (`preview`), before adding them.
- `POST /v1/tasks/import` with `"dryRun": true` (`dlm task import --dry-run`): creates nothing and
  answers, per entry (after expanding patterns), whether it would be added or why not (an invalid
  link, or a duplicate: 409 `task.duplicate` with the id of the task that has it). Every import
  result also carries the entry's `url`.
- `POST /v1/resolve/batch` (`dlm resolve-many`): probes up to 100 sources a few at a time and
  answers one result per item. Resolve results (single and batch) carry `duplicateOf` when another
  task already has the link, or the file name and size the probe found.
- `GET /v1/fs/browse` and `GET /v1/fs/free` (`dlm fs browse|free`): list the sub-directories of
  a directory, and the free space, on the daemon's machine, for choosing a download directory when
  the daemon runs elsewhere. They only work below the directories in `server.fs_roots`
  (`DLM_SERVER_FS_ROOTS`, comma-separated; empty turns them off: 404 `fs.disabled`). A path outside
  the roots, also through a symbolic link, is 422 `fs.outside_roots`, and it does not say whether
  such a path exists. `get-system-info` lists the roots as `fsRoots`.

## Speed, limits and traffic history

- `system.speed` events carry the speed of the whole daemon (`down`, `up` in bytes per second,
  `active` and `queued` task counts) once a second while anything is transferring, and once more
  with zeros when it stops; an idle daemon sends none.
- `GET /v1/stats/timeline` (`dlm stats timeline`): bytes downloaded and uploaded per `hour` or `day`
  (UTC), optionally split with `groupBy=engine|queue`, from `from` to `to` (at most 1000 buckets).
  The history is recorded from the daemon's first start with this version.
- `GET /v1/limits` (`dlm limits`): the global speed limit in force, the configured one, the active
  speed profile, the quota usage of the global scope, and the same per queue.
- `POST /v1/limits/override` (`dlm limits override [SPEED] -m MINUTES`) replaces the global speed
  limit by `speedLimit` (0: unlimited, "turbo") for 1 to 1440 minutes; `DELETE` ends it. Settings
  changes made meanwhile apply when it ends. A `limits.changed` event announces every change,
  including the end of an override.

## Boot configuration and logs

- `GET /v1/config` (`dlm config get [KEY]`): every boot configuration key (listen address, logging,
  engines, ...) with the value the running daemon uses, its default, where it came from (`default`,
  `file`, `env` or `flag`), and `pending` when the config file now holds another value that applies
  at the next start. All of it needs a restart.
- `PATCH /v1/config` (`dlm config set KEY VALUE`, `--unset`): writes keys into the config file after
  checking that the result loads and validates (422 `config.unknown_key`, `config.read_only`,
  `config.bad_value` or `config.invalid`, with the key in `errors[].location`); nothing is written
  otherwise. Other keys of the file are kept, comments are not. The daemon is not changed until it is
  restarted (`restart-daemon`). A key an environment variable or a flag overrides is written but
  keeps its running value (`source` says so).
- `GET /v1/logs` (`dlm logs`): the daemon keeps its last 5000 log records in memory; this lists
  them filtered by `level`, `q` (message or attribute text) and `since`/`until`, newest last, and
  pages backwards with `before`. `lastId` and `bootId` tell where to start a stream.
- `GET /v1/logs/stream` (`dlm logs -f`): SSE, event `log`, id `<bootId>-<id>`; `Last-Event-ID`
  replays what is still kept (a `reset` event with reason `gap` first when some was dropped,
  `overflow` when the client reads too slowly).

## Integrations and keeping clients in sync

- `GET /v1/webhooks/{id}/deliveries` (`dlm webhook deliveries ID`): the last 100 attempts to send an
  event to the webhook since the daemon started, newest first (event, attempt number, HTTP status,
  duration, error, whether a retry follows). Test deliveries are included.
- `GET /v1/plugins/{name}/logs` (`dlm plugin logs NAME`): the last 500 log records of a plugin, what its
  script wrote with `dlm.log` (at every level) and what the daemon said about it. Same filters as `get-logs`.
- `resource.changed` events (`{kind, id, op}`) announce that a `category`, `webhook`, `credential`,
  `cookie`, `plugin` or the boot `config` was `created`, `updated` or `deleted`, by any client, so
  several windows or devices stay current. An empty `id` means several changed (a cookie import) or
  there is only one (`config`): refetch the list.

## Access, remote use and housekeeping

- **Tokens.** The primary token lives in the daemon's token file. `GET /v1/auth/whoami` (`dlm auth
  whoami`) says which token a client uses (`name`, `scope`, `primary`, `local`, `tls`).
  `POST /v1/auth/tokens` (`dlm token add NAME --scope admin|read`; extension tokens are made by pairing, see below) creates a named token for a
  dashboard, script or device and shows its secret once (only a hash is kept); `GET /v1/auth/tokens`
  lists them, `DELETE /v1/auth/tokens/{id}` (`dlm token rm`) revokes one at once. A **read** token
  may only `GET` (403 `auth.read_only` otherwise), and not read cookie exports, the token list or MCP.
  `POST /v1/auth/token/rotate` (`dlm token rotate`) replaces the primary token: the new one is
  written to the token file and returned once, the old one stops working at once, named tokens are
  unaffected. Token management needs an admin token (403 `auth.admin_required`).
- **TLS for remote use.** `dlm tls init --host nas.local --apply` creates a self-signed certificate
  and key in the config directory, prints the fingerprint clients pin, and with `--apply` sets
  `server.tls_cert` and `server.tls_key`; also set `server.allow_remote` and a listen address, then
  restart.
- **Browser extension host.** `GET /v1/native-host` (`dlm native-host status`) and `POST
  /v1/native-host` with `{"action":"install"|"uninstall", ...}` register or remove the Native
  Messaging host with the browsers of the user the daemon runs as; only from the machine the daemon
  runs on (403 `native_host.local_only`).
- **Start at login.** `dlm service install|uninstall|status` installs a launchd agent (macOS), a
  systemd user unit (Linux) or a Run registry value (Windows) that runs `dlm serve`.
- **Cleanup.** `POST /v1/tasks/cleanup` (`dlm task cleanup`) removes completed tasks that finished
  at least `olderThanDays` days ago (0: all), optionally with their files (`files`), or only counts
  (`dryRun`). The global setting `retentionDays` does the same automatically every hour, never
  touching files; 0 (the default) keeps everything.

## Task details

Three read/write operations describe a task beyond its progress:

- `GET /v1/tasks/{id}/segments` (`dlm task segments`): the byte ranges downloaded in parallel and
  how much of each is done, as last saved (about once a second while the task runs).
- `PATCH /v1/tasks/{id}/files` (`dlm task files --select 0,3`): choose which files of a
  multi-file task, such as a torrent, are downloaded, also after it was created. The task's size
  follows the selection; files already downloaded and no longer selected stay on disk. Active and
  completed tasks cannot change (409 `task.files_locked`): pause the task first.
- `GET /v1/tasks/{id}/torrent` (`dlm task torrent`): state (`metadata`, `downloading`, `seeding`,
  `stopped`), info hash, trackers, connected peers, uploaded bytes and share ratio. Live while the
  task runs, otherwise what its last run saved; 404 `task.no_torrent` for other engines.

`task.progress` events also carry `uploadSpeed` (bytes per second sent to peers) and `peers`
(connected peers) for torrents.

## Events (Server-Sent Events)

`GET /v1/events` streams what happens, so a client need not poll:

```sh
curl -N -H "Authorization: Bearer $TOKEN" \
  'http://127.0.0.1:17171/v1/events?types=task.status,task.progress&taskId=<id>'
```

Each event is `id: <bootID>-<seq>`, `event: <type>`, `data: <JSON>`. Types:
`task.created`, `task.updated`, `task.status`, `task.progress` (coalesced, several
per second at most), `task.deleted`, `task.refresh`, `task.refresh.requested`, `queue.created`,
`queue.updated`, `queue.deleted`, `queue.completed`, `queue.action_cancelled`,
`settings.updated`, `quota.exhausted`, `quota.reset`, `system.speed`, `limits.changed`, `resource.changed`,
`pairing.requested` and `pairing.resolved`. Filter with `types`,
`taskId` and `queueId`. The OpenAPI document describes every event type with its
own payload schema (`StreamEvent` is a `oneOf` discriminated by `type`).

On reconnect send `Last-Event-ID`: missed events are replayed. A `reset` event
(`restart`, `gap` or `overflow`) means they could not be: refetch the state you
mirror. `dlm watch` shows the stream in a terminal.

## Webhooks

A webhook receives a `POST` for each event type it subscribes to
(`task.progress` cannot be subscribed to).

```sh
dlm webhook add https://example.com/hook --event task.status --event queue.completed --secret s3cret
dlm webhook test <id>
```

The body is the event: `{"id", "type", "time", "taskId", "queueId", "data"}`.
Headers: `X-DLM-Event` (the type), `X-DLM-Delivery` (`<event id>-<webhook id>`), and,
when the webhook has a secret, `X-DLM-Signature: sha256=<hex>`, the HMAC-SHA256 of
the raw body under the secret. Verify it before trusting the body:

```python
expected = "sha256=" + hmac.new(secret, body, hashlib.sha256).hexdigest()
ok = hmac.compare_digest(expected, request.headers["X-DLM-Signature"])
```

An answer in the 2xx range is success. Network errors, `408`, `429` and `5xx` are
retried (four attempts, growing waits); other `4xx` answers are final. Redirects are
not followed. Secrets are stored encrypted and never returned.

## Browser extension

A browser extension (Chrome, Chromium-based browsers and Firefox) hands downloads to the
daemon. It never holds the primary token: it is **paired**, and the daemon issues it a token
of its own with the scope `extension`.

**Pairing.** The extension asks its Native Messaging host (`dlm native-host`) to pair. The
host calls `POST /v1/pairing` (`dlm pairing start`) with the primary token, from the machine
the daemon runs on, and gets back a six-digit `code` that the extension shows. The daemon
publishes the event `pairing.requested`; the DLM app asks the user to allow or deny it
(`POST /v1/pairing/{id}/approve` or `/deny`, `dlm pairing approve|deny`) and the user
compares the code. On approval the daemon issues the token. The first `GET /v1/pairing/{id}`
after that returns it, once (`token`); if nobody reads it within five minutes it is revoked.
A pairing waits two minutes for an answer and at most five wait at once (409
`pairing.too_many`). `GET /v1/pairing` lists the ones waiting, for an app that started late;
`pairing.resolved` tells clients to close the prompt.

**What an extension token may not do.** It creates tasks, nothing more. A request from it is
refused with 403 `auth.extension_scope` when it carries task `settings` beyond limits
(`speedLimit`, `maxConnections`), `retry`, `checksum`, `refresh`, `autoCategorize` and the
seeding rules (so no post-action that runs a program, no proxy, no `downloadDir`); when `dir`
(or the `dir` query of `POST /v1/tasks/torrent`) is outside the download directory and
`server.fs_roots`; or when a link, mirror or media candidate points at the daemon's own machine
(`localhost`, loopback and link-local addresses such as 169.254.169.254, by address or by name;
addresses of the local network are allowed). A batch is capped at 200 tasks after patterns are
expanded (10000 for other tokens, 422 `batch.too_large`). Read and extension tokens get tasks
without the captured request headers and body, `/v1/settings` and effective settings with proxy
credentials masked and the global `headers` left out, and no `pairing.requested` events.

**What a capture can carry.** Besides the page, referrer, user agent, headers, cookies and media
candidates, `POST /v1/capture` takes `name`, `resolverKind` (`direct`, `ytdlp` for a page yt-dlp can
extract, `browser` for a page that needs the headless browser), `checksum` (`algorithm`, `expected`),
`mirrors`, `method` and `body` (base64, at most 256 KiB, for a form that answers with a file) and a
cookie's `partitionKey`. `cookieScope` says where the cookies go: `task` keeps them for this
download only: they are not in the cookie store or its exports, never replace the user's own cookie
of the same name, are sent with the download's requests, and are removed when the task completes or
is deleted (or after 7 days); `store` adds them to the cookie store. The default is `task` for an
extension token and `store` for any other token.

**Quality picker and manifests.** `POST /v1/resolve` returns `formats` for a stream, best first:
HLS variants, DASH video representations, and for a video page (`resolverKind: ytdlp`) one format per
height plus the best audio-only one. Each has an `id`, a `label` (`1080p`, `720p, 2.5 Mbit/s`,
`audio only (m4a)`), the size in pixels, bandwidth, codecs, container and, when yt-dlp knows it, the
size. Passing a format's `id` as `source.mediaHint` downloads that one. A manifest whose URL does not
end in `.m3u8` or `.mpd` (a signed or API link) is taken as HLS or DASH when the source says
`streamKind: hls|dash`; a capture sets it from the media candidate's `kind`.

**Expired links.** When a task's link expires and its refresh chain reaches the strategy
`extension-capture`, the task waits in `awaiting_link` and the daemon publishes
`task.refresh.requested` (`name`, `pageUrl`, `originalUrl`, `deadline`, `fingerprint`): the
extension tells the user, opens `pageUrl`, and the user starts the download again. The next
capture matches the waiting task when nothing contradicts it and the size, a strong ETag, the
file name or the clicked link (`url`) is the same; the task then gets the new link
(`refreshed: true`) instead of a duplicate being created.

**Torrent files.** `POST /v1/tasks/torrent` (`add-torrent-file`, and `dlm task add FILE.torrent` for a
file above 512 KiB) takes the `.torrent` file itself as the request body
(`Content-Type: application/x-bittorrent`, up to 16 MiB) and the options in the query (`queueId`,
`name`, `dir`, `priority`, `paused`, `allowDuplicate`). A `data:` URL in `POST /v1/tasks` still
works but is limited to the 1 MiB of an ordinary request. An extension token may use it (a
`.torrent` file that needs the browser's cookies is fetched by the extension and sent here).

**Limits of an extension token.** So a leaked token cannot flood the daemon, requests with it are
rate limited per token: writes (anything but GET and HEAD) at 10 per second with a burst of 30,
reads at 50 per second with a burst of 100; over the limit the answer is 429 `rate_limited` with
`Retry-After` (seconds). A capture from it may carry at most 200 cookies (8 KiB per name and value,
256 KiB together), 64 headers (32 KiB together) and 100 media candidates, and a batch at most 100
tasks; more is 422 `capture.too_large`. Other tokens are not limited.

**What the token may do.** Hand downloads over and follow them: `POST /v1/capture`,
`POST /v1/tasks`, `/v1/tasks/batch`, `/v1/tasks/expand` and `/v1/tasks/torrent`, `POST /v1/resolve` and
`/v1/resolve/batch`, `POST /v1/browser/resolve` (and downloading a candidate),
`POST /v1/cookies/import`, pausing and resuming, the link-refresh calls, and reading tasks,
queues, categories, limits (and the turbo override), system info and the event stream. It may
**not** change settings, read or export cookies, manage tokens or pairings, install the native
host, delete anything, or use MCP: those answer 403 `auth.extension_scope`.

The extension appears in `GET /v1/auth/tokens` (`dlm auth token list`) with its browser, the
extension's id and version (`kind: extension`); revoking that token (`dlm auth token rm`) ends its
access at once and does not touch any other client.

**Calling the API from an extension.** An extension that has host access calls the daemon with
`fetch` from its background: no preflight is sent and the daemon's cross-origin protection lets
the request through (checked with Chrome and Firefox), so no `allowed_origins` entry is needed.
The token is the only gate.

`dlm native-host install` registers the Native Messaging host. The published extension's ids are
built in (Chrome and every Chromium browser `gfjgahdobbldpjiijaakihgmmjemdalg`, Firefox
`dlm-extension@sabadia.github.io`), so nothing needs typing and `server.allowed_origins` is not
needed for it. The host answers `connect` (where the daemon is *now*: it reads `runtime.json`, so a
daemon on a random port or one that restarted is found; plus version, API version and, for HTTPS,
the certificate fingerprint), `pair` and `pair.status`. **It no longer hands out the primary token:**
a `connect` reply has no `token` (v0.5.0 had one); an extension gets a token only by pairing.

## MCP

The daemon serves the [Model Context Protocol](https://modelcontextprotocol.io)
over streamable HTTP at `/mcp`, with the same bearer token. Tools: `add_download`,
`list_downloads`, `get_download`, `wait_for_download` (with progress notifications),
`pause_downloads`, `resume_downloads`, `restart_download`, `delete_downloads`,
`get_stats`, `list_queues`, `create_queue`, `start_queue`, `stop_queue`,
`get_settings` and `update_settings`. For safety an assistant cannot pick a download
directory, and may only change the speed limit, connections, retries, file-exists
policy and auto-categorising. For example, with Claude Code:

```sh
claude mcp add --transport http dlm http://127.0.0.1:17171/mcp \
  --header "Authorization: Bearer $TOKEN"
```

## Plugins

JavaScript plugins resolve links, refresh expired ones and react to downloads
starting, failing and finishing: [PLUGINS.md](PLUGINS.md).
