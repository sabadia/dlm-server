---
name: dlm-admin
description: Install, run and integrate dlm - installing the binary, starting and stopping the daemon, starting at login, boot config, API tokens and scopes, remote access with TLS, logs, webhooks and signature verification, the live event stream, JavaScript plugins, browser-extension pairing and native host, the REST API and OpenAPI document, and the MCP server for AI assistants. Use when someone wants to set up or configure dlm, connect a script, dashboard or agent to it, get notified when downloads finish, write a plugin, pair the extension, or debug the daemon itself, even if they only say "hook this up to my downloads".
license: Apache-2.0
compatibility: Needs the dlm binary. Load the dlm skill first for connecting to the daemon.
metadata:
  suite: dlm
---

# Running and integrating dlm

Connect first, as the `dlm` skill describes, and use its prefix instead of plain `dlm`.

## Installing

```sh
brew install sabadia/tap/dlm                         # macOS, Linux
scoop bucket add sabadia https://github.com/sabadia/scoop-bucket && scoop install dlm
winget install sabadia.dlm
```

Releases at <https://github.com/sabadia/dlm-server/releases> also have archives,
`.deb` and `.rpm`. From a checkout: `make build` gives `bin/dlm` (Go 1.27+, no cgo).

## Running the daemon

```sh
dlm serve                     # foreground, on 127.0.0.1:17171
dlm service install           # at login, and now: launchd, systemd user, or Run key
dlm service status            # dlm service uninstall removes it
dlm health                    # is it up, and is the token accepted
dlm version                   # CLI and daemon versions
dlm system info -o json       # paths, engines, tools, database size, uptime
dlm system restart            # graceful; running downloads continue afterwards
dlm system stop               # graceful; running downloads are requeued
```

For an always-on setup prefer `dlm service install`: it survives reboots. Stopping or
restarting interrupts the user's transfers briefly, so check `dlm task list --status
running` first.

Only one daemon runs per config directory. `--home DIR` keeps everything under `DIR`
(config, data, cache, logs) for a portable or isolated instance, and
`--address 127.0.0.1:0` picks a free port, published in `runtime.json` in the config
directory, where `dlm --home DIR` finds it. To experiment safely, use a throwaway
`--home` rather than the user's daemon.

Default directories: `os.UserConfigDir()/dlm` for config, with data and cache in the
platform equivalents. `dlm system info` shows the real ones.

## Boot config vs. settings

Two different things, and mixing them up wastes time:

- **Settings** (`dlm settings`, see `dlm-organize`) shape downloads. They apply at once.
- **Boot config** (`dlm config`) shapes the daemon. It needs a restart.

```sh
dlm config get                   # every key: value, default, source, pending
dlm config get torrent           # one group, or one key
dlm config set log.level debug
dlm config set server.fs_roots /data,/media
dlm config set torrent.dht --unset
dlm system restart
```

A value is resolved flag > environment > file > default, and `source` says which won.
Every key has an environment form: `log.max_size_mb` is `DLM_LOG_MAX_SIZE_MB`, and
`DLM_HOME` and `DLM_CONFIG` exist too. A key overridden that way is written to the file
but keeps its running value. All keys are in
[references/config.md](references/config.md).

## Logs

```sh
dlm logs -n 50 --level warn
dlm logs -f --q scheduler        # follow, filtered by text
dlm logs --since 15m
```

The daemon keeps the last 5000 records in memory; older ones are in its log directory.

## Tokens and access

Requests need a bearer token, except `GET /v1/health`. The CLI reads the primary token
from the token file by itself. `dlm token show` gives the file's path, and only
`--reveal` prints the token, so avoid that in shared output.

```sh
dlm auth whoami
dlm token add dashboard --scope read       # secret is shown once
dlm token list
dlm token rm ID
dlm token rotate                           # old primary dies at once; confirm first
```

| Scope | Can |
|---|---|
| `admin` | Everything. |
| `read` | Only `GET`, and not cookie exports, the token list or MCP. |
| `extension` | Made by pairing: hand over and follow downloads. Rate limited. No settings, tokens, deletes, cookie export or MCP. |

Give each script its own named token with the narrowest scope. Revoking one leaves the
others working. Docs and help text that say `dlm auth token …` mean `dlm token …`.

## Remote access

The daemon binds to loopback and refuses anything else without TLS:

```sh
dlm tls init --host nas.local --host 192.168.1.10 --apply   # prints the fingerprint
dlm config set server.allow_remote true
dlm config set server.address 0.0.0.0:17171
dlm system restart
dlm --server https://nas.local:17171 --token "$TOKEN" health
```

Opening a downloader to the network is a real exposure: confirm with the user and
use a named token. To let remote clients pick folders, set `server.fs_roots`; then
`dlm fs browse [PATH]` and `dlm fs free PATH` work, below those roots only. Without
roots they answer `fs.disabled`.

Browser pages may call the API only from origins in `server.allowed_origins`, with no
wildcard.

## Getting told when things happen

**Polling less: the event stream.** `dlm watch -o json` prints one JSON object per
line and reconnects by itself; filter with `--type`, `--task` and `--queue`. It runs
until stopped, so give it a timeout. Over HTTP it is `GET /v1/events`, with
`Last-Event-ID` to resume; a `reset` event means events were lost, so refetch. Event
types are in [references/events-and-errors.md](references/events-and-errors.md).

**Webhooks** push events to a URL:

```sh
dlm webhook add https://example.com/hook --event task.status --event queue.completed --secret s3cret
dlm webhook test ID                 # sends a real test event
dlm webhook deliveries ID           # last 100 attempts with status and error
dlm webhook set ID --enabled=false  # also --url, --event (replaces), --secret
dlm webhook list
dlm webhook get ID
dlm webhook rm ID
```

`task.progress` cannot be subscribed to. A 2xx is success. Network errors, 408, 429
and 5xx are retried (four attempts); other 4xx are final, and redirects are not
followed. Body: `{"id","type","time","taskId","queueId","data"}`. Headers:
`X-DLM-Event`, `X-DLM-Delivery`, and with a secret
`X-DLM-Signature: sha256=<hex HMAC-SHA256 of the raw body>`. Verify it before trusting
the body, with a constant-time compare:

```python
expected = "sha256=" + hmac.new(secret, body, hashlib.sha256).hexdigest()
ok = hmac.compare_digest(expected, request.headers["X-DLM-Signature"])
```

## Plugins

A plugin is JavaScript that runs inside the daemon, for sites the engines cannot
handle: turning a page into a real link, renewing expired links, reacting to tasks.

```sh
dlm plugin install ./my-plugin            # or an https:// git URL, with --ref TAG
dlm plugin install ./my-plugin --replace  # upgrade; keeps the old one if the new breaks
dlm plugin list
dlm plugin get NAME
dlm plugin logs NAME                      # the last 500 records
dlm plugin enable NAME
dlm plugin disable NAME
dlm plugin rm NAME
```

`--disabled` installs it switched off. `rm` also deletes its stored data. A plugin
decides what links your downloads use, so read the code before installing someone
else's. [references/plugins.md](references/plugins.md) is the authoring guide.

## Browser extension

The extension hands browser downloads to dlm with their cookies and page, and helps
renew links. It never gets the primary token.

```sh
dlm native-host install       # registers with Chrome, Chromium, Edge, Brave, Firefox
dlm native-host status        # dlm native-host uninstall removes it
dlm pairing list              # waiting requests
dlm pairing approve ID        # only after the user compares the six-digit code
dlm pairing deny ID
dlm pairing show ID           # an approved one prints its token, once
dlm pairing start --name X    # what the host does; rarely by hand
```

Approval is the user's decision: do not approve a pairing yourself unless the user
confirmed that the code matches what the extension shows. A pairing waits 2 minutes,
at most five wait at once, and an unread token is revoked after 5 minutes.
`--extension-id` and `--firefox-id` on `install` add your own builds. Revoke an
extension with `dlm token rm ID`.

## REST API

Everything in the CLI is available over HTTP, on the address in `dlm system info`:

```sh
TOKEN=$(cat "$(dlm token show -o json | python3 -c 'import json,sys;print(json.load(sys.stdin)["path"])')")
curl -H "Authorization: Bearer $TOKEN" 'http://127.0.0.1:17171/v1/tasks?status=running&total=true'
```

Conventions: JSON in `camelCase`, RFC 3339 UTC times, sizes in bytes with `-1` for
unknown. Lists page with `nextCursor`. Bulk operations answer 200 with `{id, ok, task,
error}` per item, so check each `ok`. PATCH uses JSON merge patch (`null` removes).
`GET /v1/settings` returns an `ETag`; send `If-Match` to avoid overwriting someone
else's change (412 `settings.changed`).

Errors are RFC 9457 problem details with a stable `code` and `params`. Branch on
`code`, not on `detail`, which is English text.
[references/events-and-errors.md](references/events-and-errors.md) lists them.

The spec is `dlm openapi` (`--oas 3.0` for older generators), or `/openapi.json` with a
UI at `/docs` on a running daemon. Every operation is mapped to its command in the
`dlm` skill's [rest-api.md](../dlm/references/rest-api.md).

## MCP server

`/mcp` on the daemon speaks streamable HTTP with an admin token. Prefer it for agents
that support MCP: `wait_for_download` blocks with progress notifications.

```sh
claude mcp add --transport http dlm http://127.0.0.1:17171/mcp --header "Authorization: Bearer $TOKEN"
```

```json
{"mcpServers": {"dlm": {"type": "http", "url": "http://127.0.0.1:17171/mcp",
  "headers": {"Authorization": "Bearer <token>"}}}}
```

Fill in the daemon's real address; the desktop app's port changes on each start.

| Tool | Notes |
|---|---|
| `add_download` | `url`, `mirrors`, `name`, `queueId`, `paused`, `ytdlp` |
| `list_downloads` | `status`, `queueId`, `search`, `limit` (up to 200), `cursor` |
| `get_download`, `get_stats`, `list_queues` | Read-only |
| `wait_for_download` | `id`, `timeoutSeconds` 1-3600, default 60; check `timedOut` |
| `pause_downloads`, `resume_downloads` | `ids` |
| `restart_download` | Discards progress |
| `delete_downloads` | `ids`, `deleteFiles` |
| `create_queue`, `start_queue`, `stop_queue` | |
| `get_settings` | Global, or `queueId` or `taskId` for effective values |
| `update_settings` | Global patch of `speedLimit`, `maxConnections`, `retry`, `fileExistsPolicy`, `autoCategorize` |

MCP is deliberately limited: it cannot pick a directory, set post-actions, or change
settings that run programs or redirect traffic. For those, use the CLI, and only with
the user's say-so.
