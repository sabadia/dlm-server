# Events and error codes

## Events

From `dlm watch -o json` (one object per line) or `GET /v1/events?types=&taskId=&queueId=`.
An SSE message has `id: <bootID>-<seq>`, `event: <type>`, `data: <JSON>`. Each carries
`type`, `time`, and `taskId` or `queueId` where it applies.

| Type | Data | Fired when |
|---|---|---|
| `task.created`, `task.updated`, `task.deleted` | the task or its ID | A task changes. |
| `task.status` | `from`, `to`, `error` | Any status change. The one to watch. |
| `task.progress` | `downloaded`, `size`, `speed`, `connections`, `uploadSpeed`, `peers` | Several times a second at most. Not available to webhooks. |
| `task.refresh` | attempt | A link refresh attempt. |
| `task.refresh.requested` | `name`, `pageUrl`, `originalUrl`, `deadline`, `fingerprint` | A task waits for the extension. |
| `queue.created`, `queue.updated`, `queue.deleted` | the queue | A queue changes. |
| `queue.completed` | | A queue ran out of work. |
| `queue.action_cancelled` | | New work cancelled the end-of-queue action. |
| `settings.updated` | | Global settings changed. |
| `quota.exhausted`, `quota.reset` | scope and usage | A quota ran out, or a new period began. |
| `system.speed` | `down`, `up`, `active`, `queued` | Each second while transferring, then once with zeros. |
| `limits.changed` | | The speed limit in force changed, including an override ending. |
| `resource.changed` | `kind`, `id`, `op` | A category, webhook, credential, cookie, plugin or the config changed. Empty `id`: refetch the list. |
| `pairing.requested`, `pairing.resolved` | pairing | An extension asks to pair, or the request ended. |

A `reset` message has reason `restart`, `gap` or `overflow`: events were lost and
cannot be replayed, so refetch the state you mirror. `GET /v1/logs/stream` works the
same way, with event `log`.

## Task `error.code`

Recorded on a task in `error`, with `message` and `retryable`: `network`,
`http_status`, `link_expired`, `browser`, `disk_full`, `checksum_mismatch`,
`file_exists`, `postprocess`, `infected`, `fatal`. The `dlm` skill's table says what to
do about each.

## API problem codes

Every API error is `application/problem+json` with `status`, `title`, `detail`, a stable
`code` and `params`; `type` is `urn:dlm:problem:<code>`. Validation errors add `errors[]`
with a `location`. The CLI prints `error: <title>: <detail>`; use `-o json` on bulk
commands to see per-item errors.

New codes may appear, and generated ones follow the pattern `<entity>.not_found`,
`<entity>.already_exists` and `<entity>.name_taken`.

| Area | Codes |
|---|---|
| Auth | 401 for a missing token; `auth.admin_required`, `auth.read_only`, `auth.extension_scope` (403); `rate_limited` (429, extension tokens) |
| Tasks | `task.not_found`, `task.duplicate`, `task.invalid_state`, `task.invalid_transition`, `task.state_changed`, `task.locked`, `task.file_exists` |
| Task files | `task.files_locked`, `task.no_files`, `task.no_file_selected`, `task.unknown_file`, `task.no_torrent`, `torrent.not_a_torrent` |
| Queues | `queue.not_found`, `queue.name_taken`, `queue.not_empty` |
| Settings | `settings.invalid`, `settings.changed` (412) |
| Config | `config.unknown_key`, `config.read_only`, `config.bad_value`, `config.invalid` |
| Limits and stats | `limits.bad_limit`, `limits.bad_duration`, `stats.bad_bucket`, `stats.bad_range`, `stats.range_too_large` |
| Cleanup | `cleanup.bad_age` |
| Capture | `capture.bad_resolver`, `capture.body_too_large`, `capture.too_large` |
| Filesystem | `fs.disabled`, `fs.outside_roots`, `fs.not_absolute`, `fs.not_directory`, `fs.path_not_found`, `fs.unreadable` |
| Pairing | `pairing.not_found`, `pairing.not_pending`, `pairing.bad_browser`, `pairing.bad_field`, `pairing.too_many`, `pairing.local_only` |
| Native host | `native_host.local_only`, `native_host.nothing_to_install` |
| Tokens | `token.bad_name`, `token.bad_scope` |
| System | `origin.failed`, `browser.unavailable`, `browser.running`, `service.unavailable`, `request.cancelled` |
