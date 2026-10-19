# Architecture

dlm is one Go binary. `dlm serve` runs the daemon; every other command is a client
of a running daemon. The code follows a hexagonal layout: the domain at the core,
use-cases around it, and adapters (HTTP API, MCP, SQLite, download engines) at the
edge. [CONVENTIONS.md](CONVENTIONS.md) has the rules; [PLAN.md](PLAN.md) the plan.

## Layers

```
            REST API (api)     MCP (mcp)     CLI (cli → client)
                   \              |              /
                    +-------- service ----------+        use-cases
                     |   |    |     |     |   |
        scheduler  runner refresh postproc  plugin webhook    run the work
                     |        |
              engine/* (httpx ftp hls dash bt ed2k ytdlp browserx)
                     |
        netclient  ratelimit  cookies  browser  ffmpeg  ytdlp     tools
                     |
                  storage (SQLite)                                persistence
                     |
              domain, settings                                    pure, no I/O
```

- **`domain`, `settings`** hold the entities (task, queue, source, category, ...), the
  task status machine and the layered settings model (`Optional[T]`, resolved as
  Defaults ⊕ Global ⊕ Queue ⊕ Task with the source level of each value). No I/O.
- **`service`** holds the use-cases (`TaskService`, `QueueService`, `SettingsService`,
  `CaptureService`, `RefreshService`, ...) and the `Runner` that executes one task.
  The REST API and the MCP server both call it, which is why they cannot drift apart.
  `service` talks to storage and engines through interfaces.
- **Adapters** never import each other. `api` (huma on `net/http`) and `mcp` expose the
  services; `cli` is a typed REST client (`client`) so it works against any daemon;
  `storage/sqlite` implements the repositories; each `engine/*` is a download protocol.
- **`daemon`** is the composition root: the only package that wires everything,
  starts the long-running components in an `errgroup`, and shuts them down in order.
- **`platform`** is the only place with OS-specific code (build tags). All builds are
  `CGO_ENABLED=0`.

## Life of a download

1. **Resolve** (`POST /v1/resolve`, optional). The engine registry picks the first
   engine whose `Match` accepts the source (browser, hls, dash, ftp, bt, ed2k, ytdlp,
   then http as the catch-all). The engine probes the link and returns a resource
   (files, size, range support, name). For HTTP the probe response is kept open, so a
   one-time URL still works when the download starts. Plugins may rewrite the link
   first (`resolve` hook).
2. **Create.** The task is stored in a queue (default `main`) with its `Source`: the
   link plus everything needed to get it again (original URL, page URL, referrer,
   headers, mirrors, fingerprint). Duplicates are refused. The scheduler is woken.
3. **Schedule.** Each queue has its own slot count; a global cap spans queues. Order
   is priority, then position. Queue time windows, quotas and speed profiles gate it.
4. **Run.** The `Runner` computes the effective settings, asks the engine for a
   `Download`, and runs it with a rate limiter chained task → queue → global.
   Progress goes to the event bus, coalesced; resume state is checkpointed every
   couple of seconds and on exit, so a crash or restart continues where it stopped.
5. **Errors.** Engines classify failures as transient, fatal or link-expired. Transient
   ones are retried with backoff (bounded, `Retry-After` honoured). A link-expired
   failure starts **link refresh**: strategies run in the configured order
   (origin-redirect, page-reload in the headless browser, plugin, then waiting for the
   extension or a manual new URL); a candidate link is probed and its fingerprint is
   compared before the existing segments are resumed.
6. **Post-process.** The task enters `postprocessing`: checksum verification (a
   mismatch deletes the file and downloads again), then the configured actions
   (extract, add-torrent, command, av-scan). Actions fail soft, except an infected
   file. Then the file is sorted into its category folder and the task is `completed`.
7. **Notify.** Every transition is an event on the bus. Webhooks, plugin hooks, SSE
   clients and MCP waiters are subscribers.

All status changes go through one guarded method of the task, and a per-task lock
serialises read-modify-write cycles, which removes the races of a free-for-all
status field.

## The HTTP engine

`engine/httpx` downloads with several connections: it starts one, then adds
connections gradually (slow start), and a connection that finishes steals half of
the largest remaining segment. Responses are validated strictly: a `206` must carry
the `Content-Range` that was asked for, `If-Range` guards against the file changing,
and a server that ignores ranges makes the download fall back to one sequential
stream. The file is written as `name.part` and renamed on completion. Mirrors share
the same segment table: each is probed first and kept only if it serves the same
size with ranges, and a failing mirror is dropped without failing the task.
`engine/segmented` is the shared machinery of the HLS and DASH engines.

## Settings

One `Settings` struct is used at every layer, with `Optional[T]` fields. Resolution
picks the nearest layer that sets a value and records which one did, so
`dlm task settings ID` can explain itself. Some fields only make sense at
some levels (concurrency on a queue, a checksum on a task); validation rejects the
rest. Rate limits nest instead of override.

## Storage

SQLite (pure Go driver, WAL) with versioned goose migrations embedded in the binary
and sqlc-generated queries. Tasks, queues, segments, engine state, cookies, refresh
history, categories, webhooks, credentials and plugins are tables; `source`,
`resource` and settings are JSON columns. Timestamps are UTC milliseconds. Writes of
progress are throttled. Secrets (cookie values, webhook secrets, passwords) are
sealed with AES-GCM; the key lives in the OS keyring, or in a 0600 file in portable
mode (`--home`).

## Security model

- The daemon binds to loopback by default; a non-loopback bind needs TLS.
- Every `/v1` and `/mcp` request needs the bearer token (constant-time compare);
  `GET /v1/health` is public. Cross-origin browser calls only work for configured
  origins, and the server refuses cross-site form posts.
- There is no endpoint that fetches arbitrary URLs for the caller, and no secret is
  returned by any response.
- Plugins run in a goja sandbox with no file system, process or network access except
  the hosts their manifest lists, under a time limit per call. Post-action commands
  are executed directly (never through a shell). MCP clients cannot change settings
  that run programs or redirect traffic.

## Extension points

| To add | Implement | Register in |
|---|---|---|
| A protocol | `engine.Engine` (`Match`, `Resolve`, `NewDownload`) | `daemon.Run`, in front of `httpx` |
| A link-refresh strategy | `refresh.Strategy` | `daemon.NewServices` |
| A post action | `postproc.Action` | `postproc.Default` and `settings.postActionParams` |
| A site (without Go) | a JavaScript plugin, see [PLUGINS.md](PLUGINS.md) | `dlm plugin install` |
| An API operation | a huma operation plus a CLI command annotated with its operation id | `api`, `cli` |

The last row is enforced: a test fails when an OpenAPI operation has no CLI command.
`docs/CLI.md` and `docs/openapi.json` are generated (`make docs`) and tested to be
current.
