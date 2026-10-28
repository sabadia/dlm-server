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

## Events (Server-Sent Events)

`GET /v1/events` streams what happens, so a client need not poll:

```sh
curl -N -H "Authorization: Bearer $TOKEN" \
  'http://127.0.0.1:17171/v1/events?types=task.status,task.progress&taskId=<id>'
```

Each event is `id: <bootID>-<seq>`, `event: <type>`, `data: <JSON>`. Types:
`task.created`, `task.updated`, `task.status`, `task.progress` (coalesced, several
per second at most), `task.deleted`, `task.refresh`, `queue.created`,
`queue.updated`, `queue.deleted`, `queue.completed`, `queue.action_cancelled`,
`settings.updated`, `quota.exhausted` and `quota.reset`. Filter with `types`,
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

`POST /v1/capture` creates a task from a download the extension caught, with the
page, referrer, cookies and headers that make it work; `POST /v1/cookies/import`
loads cookies. `dlm native-host install` registers the Native Messaging host that
tells the extension the daemon's port and token.

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
