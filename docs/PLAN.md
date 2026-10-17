# dlm-server — Implementation Plan (v1)

## Context

We are building a cross-platform download manager: **Go backend** (this plan), a Flutter desktop UI and a Chromium/Firefox extension (both later). The backend must offer **every feature of Gopeed's Go server** plus the user's requirements: multiple independent queues, 3-level settings inheritance (Global → Queue → Task), real link-refresh with origin-page references, an internal headless browser with cookie management (for cookie-gated, streaming and blob downloads), full REST API **and** a CLI with 100 % parity, and the usual "serious download manager" features found by research (IDM, FDM, JDownloader, aria2, XDM, AB DM).

Research basis: Gopeed (commit `5462b14`, Oct 17 2026) was cloned and its Go backend analysed (UI and extension code ignored). Decisions confirmed with the user:

| Decision | Choice |
|---|---|
| Repo | `github.com/sabadia/dlm-server`, public |
| License | **Clean-room, Apache-2.0**. Gopeed is GPL-3.0, so we study its design and write all code ourselves. Every dependency must be Apache-2.0-compatible (MIT/BSD/Apache/MPL-2.0). |
| Protocols | HTTP/HTTPS, FTP, HLS/DASH, BitTorrent/magnet, ed2k |
| Access | Binds to loopback; bearer token required; optional remote mode with TLS |
| Flutter link | Sidecar daemon, talks over REST + SSE (no cgo/FFI) |
| Plugins | JS plugin runtime in a **later phase**; hook interfaces designed now |
| Extras | yt-dlp engine, MCP server, archive auto-extract, webhooks + post-actions |
| ffmpeg | System ffmpeg on PATH, else auto-downloaded static build (nothing GPL linked into our binary) |

## What we adopt from Gopeed, and what we fix

**Adopt (the ideas, re-implemented):**
- Protocol-engine interface + registry; first scheme/extension filter that matches picks the engine.
- Two-step flow: resolve → create; also a direct create that skips resolve.
- HTTP probe that **keeps the probe response open**, so one-time URLs still work.
- Slow-start connection growth, work-stealing segment split, strict validation of Range/`If-Range` responses, fallback to a single sequential stream when the server ignores ranges.
- Filename detection (RFC 6266/5987 `filename*`, extension added from Content-Type), safe names, auto-rename, path placeholders (`%year%`…), download-dir allow-list.
- HLS VOD (AES-128, fMP4 `EXT-X-MAP`, byte ranges, resume journal); BT seeding rules (ratio/time/keep); checksum with full re-download on mismatch.
- Auto-create a BT task from a downloaded `.torrent`; archive auto-extract (multi-part, password).
- Webhooks, user scripts, MCP tools, browser TLS impersonation fallback for Cloudflare, temp-file scope cleanup, per-protocol stats, startup auto-resume, auto-delete tasks whose files are missing.

**Fix (Gopeed weaknesses we must not copy):**
- 2.6k-line god-object downloader and global singletons (BT client, default downloader) → small services wired by DI.
- Single FIFO queue; a task paused while waiting can still auto-start (bug); no priorities.
- **No speed limiting at all** → hierarchical token buckets.
- Retries forever on 5xx/429/network errors, so `onError`/refresh never fires → bounded backoff + error classification.
- New cookie jar on every retry → persistent cookie store.
- Polling only, single event listener → multi-subscriber bus + SSE.
- HTTP 200 for errors → RFC 9457 problem details and real status codes.
- `0.0.0.0` with no auth by default, open SSRF proxy, `GET /config` leaks the token → none of these.
- No migrations, a disk-syncing write every 350 ms per task → versioned SQL migrations, throttled writes.
- Data races (unlocked status reads, internal slices returned to callers), extension hooks with no timeout.
- CLI cannot drive a running server → CLI is a client of the daemon.
- Old dependencies: gorilla/mux, pkg/errors, zerolog 1.31, go-git with CVEs.

## Tech stack (verified current, Oct 2026)

| Concern | Choice | Reason |
|---|---|---|
| Language | Go 1.27.x (`go 1.27` in go.mod) | Latest stable (1.27.2 installed) |
| HTTP/API | `net/http` ServeMux + **huma v2** (humago adapter) | OpenAPI 3.1 generated from Go types, validation, SSE; the spec then drives the Dart/TS client generators |
| CLI | **cobra** | Standard choice, shell completions, nested commands |
| Boot config | **koanf v2** (file + env + flags) | Lighter than viper, no key-lowercasing quirks |
| Storage | SQLite via **modernc.org/sqlite** (pure Go, WAL) + **sqlc** + **goose** migrations | History/filter queries, no CGO, type-safe queries, versioned schema |
| Logging | `log/slog` (JSON) to a rotating file | stdlib |
| Rate limiting | `golang.org/x/time/rate` | Nested token buckets |
| Headless browser | **chromedp** (v0.21) | Actively maintained, pure CDP, full Network/Fetch/Storage access. go-rod has stalled since 2024; playwright-go needs Node. |
| HLS/DASH parsing | Eyevinn m3u8 fork + `Eyevinn/dash-mpd` + `mp4ff` (MIT) | Maintained parsers |
| BT / ed2k / FTP | `anacrolix/torrent` (MPL-2.0), `monkeyWie/goed2k` (MIT), `jlaffaye/ftp` (MIT) | License-compatible |
| Archives | `mholt/archives` (MIT) | zip/7z/rar/tar |
| MCP | `modelcontextprotocol/go-sdk` | Official SDK |
| JS plugins (later) | `dop251/goja` with interrupt-based timeouts | Pure Go |
| Tooling | golangci-lint v2, gofumpt, govulncheck, GoReleaser v2, GitHub Actions | |

Before adding any dependency, check its latest version and docs (Context7 / official docs).

## Architecture

Hexagonal layout: domain at the core, ports as interfaces, adapters outside. Both the REST API and the MCP server call the same `service` layer. The CLI is a typed REST client.

```
cmd/dlm/                main.go → cli.Execute()  (single binary: `dlm serve`, `dlm task add`, `dlm native-host`…)
internal/
  domain/               Task, Queue, Source, Segment, Category, Settings layers, Status FSM, typed errors
  settings/             Optional[T] layered model, Resolve(global, queue, task) → Effective + provenance
  service/              use-cases: TaskService, QueueService, SettingsService, CaptureService, RefreshService…
  scheduler/            multi-queue scheduler, global + per-queue concurrency, priority, time windows
  engine/               Engine interface + registry
    httpx/ ftp/ hls/ dash/ bt/ ed2k/ ytdlp/ browser/   (each a self-contained adapter)
  ratelimit/            hierarchical limiter: global → queue → task
  refresh/              expiry detection, strategy chain, new-link validation
  browser/              chromedp manager: discovery/download, pool, profiles, sniffer, cookie sync
  cookies/              persistent jar (RFC 6265, public suffix), import/export (JSON, Netscape)
  storage/              repository ports + sqlite adapter (sqlc), migrations (goose, embedded)
  events/               multi-subscriber bus, progress coalescing, SSE fan-out
  postproc/             checksum, extract, move-by-category, run-command, AV scan, webhook
  api/                  huma operations, auth middleware, SSE, problem details
  client/               Go REST client (shared by CLI and tests)
  cli/                  cobra commands (thin; call client; `serve` delegates to daemon)
  daemon/               composition root: wires config, logging, storage, bus, services, API; runs until shutdown
  mcp/                  MCP tools mapped onto services
  nativehost/           Native Messaging shim + manifest installer (Chrome/Chromium/Edge/Brave/Firefox × OS)
  platform/             ONLY place for build-tagged OS code: prealloc, disk-free, sleep/shutdown, paths
  plugins/              (later) goja runtime, hook dispatch
```

**Download lifecycle**
1. `resolve`: pick the engine, probe, return a Resource (files, size, ranges, suggested name). The probe is cached for N minutes under a resolve ID.
2. `create`: the task is placed in a queue (default: **Main**).
3. The scheduler admits it when queue and global slots allow.
4. The engine runs, using the effective settings, the limiter and the cookie jar.
5. Progress events go onto the bus, are coalesced, and are sent over SSE.
6. On a classified error, the refresh/retry policy decides what happens next.
7. Completion runs the post-processing pipeline.

**Platform rules**
- `CGO_ENABLED=0` builds for linux, darwin and windows on amd64 and arm64.
- Paths come from `os.UserConfigDir`, `os.UserCacheDir` and `filepath`.
- Filename sanitising follows the strictest OS rules (Windows reserved names) so names work everywhere.
- OS-specific code lives only in `internal/platform` with `_windows.go`/`_unix.go` files.
- CI runs the test matrix on all three OSes.

## Key designs for the user's requirements

### 1. Multiple queues + 3-level settings inheritance
- `Queue{id, name, isDefault, position, state(running|stopped), schedule, settingsOverride}`. A built-in **Main** queue always exists and can't be deleted. Users can create, rename, reorder, start and stop any queue, move tasks between queues, and reorder tasks inside one.
- `Settings` is one struct of `Optional[T]` fields, used at every level:
  - download dir and filename template
  - max connections per task
  - max concurrent downloads (queue/global only)
  - speed limit
  - retry policy (attempts, backoff, Retry-After)
  - proxy, user-agent, headers, TLS-verify
  - per-host proxy rules (global only; ignored when a queue or task sets its own proxy)
  - file-exists policy (rename/overwrite/skip/resume/ask)
  - checksum
  - refresh policy
  - browser usage (never/auto/always)
  - post-actions, category rules
  - schedule (queue only)
- **Resolution:** `Effective = Defaults ⊕ Global ⊕ Queue ⊕ Task`; the nearest non-empty value wins. `GET /v1/tasks/{id}/settings/effective` (and `dlm task settings <id> --effective`) returns each value with its **source level**. Fields that make no sense at a level are rejected by validation (for example concurrent downloads on a task).
- **Scheduler:**
  - Each queue has its own run loop and slot count, plus an optional global cap across all queues.
  - Ordering: priority, then position.
  - Time windows (days + hours) start and stop a queue, with an optional post-queue action (stop, sleep, shutdown, run command).
  - Pause correctly removes a task from waiting (fixes Gopeed's bug).
  - Pause/stop/delete changes take effect at once.
- **Rate limits nest:** a task's bytes must pass the task, queue and global token buckets.

### 2. Link refresh with origin references
- Every task stores a `Source`:
  - `currentURL`, `originalURL`, `redirectURL`
  - `pageURL` (the origin web page), `referrer`, `method/body`
  - captured headers, user-agent, cookie domain(s)
  - `mediaHint` (manifest pattern/variant for streams)
  - `resolverKind` (direct, browser, ytdlp, plugin)
  - `fingerprint` (size, strong ETag, Last-Modified, filename)
- **Expiry detection:** 401/403/404/410, a 200 with `text/html` where a file was expected, a size/ETag mismatch, and signed-URL expiry params (`Expires`, `X-Amz-Expires`, `exp`…) so we can refresh *before* the link expires. These are classified as `ErrLinkExpired`, separately from transient errors.
- **Strategy chain**, configurable at Global/Queue/Task level (`refresh.auto`, `refresh.strategies`, `refresh.maxAttempts`):
  1. `origin-redirect`: re-request `originalURL`.
  2. `page-reload`: open `pageURL` in the headless browser with the stored cookies, sniff a matching download or media request (matched by filename, size, mediaHint or a URL pattern).
  3. `extension-capture`: IDM-style. The task waits in `awaiting_link` state; the next download captured from the extension that matches the fingerprint is attached. Has a timeout.
  4. `plugin` (later).
  5. `manual`: `PATCH` a new URL.
- **Validation:** before swapping in a refreshed URL, probe it and check the fingerprint (size + strong ETag/filename). On a match, resume the existing segments. On a mismatch, apply the policy (`restart` | `fail` | `ask`).
- Every attempt is recorded in a `refresh_history` table.
- Endpoints/commands: `POST /v1/tasks/{id}/refresh`, `POST /v1/tasks/{id}/refresh/await`, `PATCH /v1/tasks/{id}/source`; `dlm task refresh|await-link|set-url`.

### 3. Internal browser + cookies
- **`browser.Manager`** (chromedp):
  - Discovers Chrome/Edge/Chromium/Brave per OS, or downloads **chrome-headless-shell** (Chrome for Testing JSON endpoints) into the cache dir.
  - Keeps a bounded pool with an idle shutdown.
  - Uses an isolated profile per site group.
  - Injects cookies from the store (`storage.SetCookies`) and writes them back after the session.
- **`browser.Sniffer`** watches `Network.*` events for:
  - media (`.m3u8`, `.mpd`, `video/*`, `audio/*`);
  - file downloads, via `Browser.setDownloadBehavior` with events enabled. A JS-generated **blob** download is saved by the browser to a temp dir and then imported as a finished/streamed task.
  - MSE `blob:` media, by capturing the manifests or segments that feed it.

  The results become "candidates" the caller chooses from, or that are auto-picked using a `mediaHint`.
- **`cookies.Store`**:
  - SQLite-backed RFC 6265 jar (public-suffix aware, partition key), shared by all engines, the browser and yt-dlp (yt-dlp gets a temporary Netscape file, mode 0600, deleted afterwards).
  - Values are encrypted at rest with AES-GCM. The key is held in the OS keyring (`zalando/go-keyring`), falling back to a 0600 key file.
- **Extension contract:**
  - `POST /v1/capture`: url, finalUrl, referrer/pageUrl, filename, mime, size, cookies[], userAgent, headers, mediaCandidates[].
  - `POST /v1/cookies/import`, `GET /v1/health`, and `GET /v1/events` (SSE).
  - `dlm native-host` (Native Messaging shim) gives the extension the port and token; `dlm native-host install|uninstall` writes the manifests/registry entries for every supported browser on every OS.
  - The server checks `Origin` against the configured extension IDs.

### 4. REST + CLI parity
- Every service use-case gets a huma operation **and** a cobra command. The CLI uses `internal/client`, talks to the daemon, and outputs `--output table|json|yaml`.
- `dlm serve` runs the daemon. `dlm get <url>` is a convenience command that starts the daemon if it isn't running.
- **Parity test:** CI walks the generated OpenAPI operation IDs and fails if any lacks a registered CLI command (`cli.Annotations["operationId"]`).
- API conventions:
  - `/v1` prefix, RFC 9457 errors, cursor pagination with filters, JSON Merge Patch for PATCH.
  - `/openapi.json` and `/docs` served.
  - SSE `/v1/events?types=&taskId=&queueId=` with `Last-Event-ID`.
- Auth:
  - A random token is generated on first run (0600 file under the config dir), sent as `Authorization: Bearer`, compared in constant time.
  - Loopback bind by default. Remote mode requires TLS + token.
  - CORS allows only the extension origins; `http.CrossOriginProtection` is on.
- There is no generic proxy endpoint, and secrets are never returned by the API.

### 5. Additional features (research-driven)
- Categories that auto-sort by extension/MIME into dirs.
- Duplicate detection (URL / name+size / hash).
- Disk-space precheck and preallocation (fallocate / F_PREALLOCATE / sparse).
- `.part` file + atomic rename.
- Mirrors / multi-source per task.
- Batch and pattern URLs (`file[001-100].jpg`).
- Import/export (JSON, URL list, aria2 input file).
- History search and stats.
- Speed profiles by time of day, and MB/hour quotas.
- Proxies per site (HTTP/SOCKS5/system).
- Stored site credentials (Basic/Digest), kept in the keyring.
- Post-actions: checksum (md5/sha1/sha256/blake3), extract, AV-scan command, user command with templated variables, webhook, open/move.
- After a queue finishes: sleep/shutdown.
- Graceful shutdown with resume state flushed.
- Site grabber/spider is out of v1 scope (it can go on the board as a backlog item).

## Data model (SQLite, goose migrations)

| Table | Columns |
|---|---|
| `settings_global` | single row: JSON settings, updated_at |
| `queues` | id, name (unique, case-insensitive), is_default, position, state, settings (JSON, schedule lives inside), created_at/updated_at |
| `tasks` | id, queue_id, status, priority, position, engine, name, dir, size, downloaded, source, resource, settings, labels, error (JSON TEXT columns), created_at/updated_at/started_at/completed_at |
| `task_segments` | task_id, idx, start, end, done (writes throttled about every 2 s + on pause/exit) |
| `engine_state` | task_id, blob (per-engine resume data) |
| `refresh_history` | per-attempt record of refreshes |
| `cookies` | encrypted cookie values |
| `credentials` | stored site logins |
| `categories` | auto-sort rules |
| `webhooks` | webhook endpoints |
| `plugins` | (later) |

Task status FSM: `queued → running → (paused | awaiting_link | refreshing | error | completed)`, with `postprocessing` between `running` and `completed`. Every transition goes through a single guarded method, which removes the class of races Gopeed has.

## Git & project management
- Create `sabadia/dlm-server` (public, Apache-2.0) with `gh repo create`, and a GitHub Project (v2) board "dlm-server" with columns Backlog / Ready / In progress / In review / Done. If needed, run `gh auth refresh -s project` (the user may have to run it via `! gh auth refresh -s project`).
- One milestone per phase. Each task below becomes an issue with labels (`area/*`, `type/*`) and is added to the board.
- `main` is protected. Work goes on `feat/<issue#>-<slug>` branches and lands as PRs that close their issue, merged with squash. Conventional Commits.
- This plan is committed as `docs/PLAN.md`, the authoritative spec. Checkboxes are ticked one task at a time, as each one is completed.
- Definition of Done per task: gofumpt, golangci-lint, `go vet`, and `go test -race` all pass, and tests are added. On top of that, the CI matrix (ubuntu/macos/windows) is green, docs are updated, and the board card is moved.

## Phased tasks

### Phase 0: Bootstrap
- [x] P0.1 Create the GitHub repo, project board, labels and milestones; push `docs/PLAN.md`, LICENSE (Apache-2.0), README, NOTICE (clean-room statement)
- [x] P0.2 Go module `github.com/sabadia/dlm-server` (Go 1.27), directory skeleton, Makefile/Taskfile, `.golangci.yml` (v2), `.editorconfig`
- [x] P0.3 GitHub Actions: lint, test matrix (3 OS × amd64; `-race` on linux/macos), govulncheck, build of all 6 targets with CGO off; Dependabot
- [x] P0.4 Create issues for every task below and attach them to the board

### Phase 1: Foundations
- [x] P1.1 Boot config (koanf), data/config/cache dirs, slog logging with rotation
- [x] P1.2 Domain models + status FSM + typed errors (with unit tests)
- [x] P1.3 `settings` package: `Optional[T]`, layered resolve + provenance, per-level validation
- [x] P1.4 SQLite storage: goose migrations, sqlc queries, repositories for settings/queues/tasks/segments
- [x] P1.5 Event bus (multi-subscriber, non-blocking, coalescing)
- [x] P1.6 API server skeleton: huma, auth token, CORS/origin check, problem details, `/v1/health`, `/openapi.json`, graceful shutdown
- [x] P1.7 CLI skeleton: cobra root, `serve`, client package, output formats, parity-test harness

### Phase 2: HTTP engine
- [x] P2.1 Engine interface + registry; resolve cache
- [x] P2.2 HTTP probe (keeps the response open), filename detection, If-Range validator capture
- [x] P2.3 Segmented downloader: slow-start, work stealing, strict range validation, sequential fallback, `.part` + rename, preallocation, disk precheck
- [x] P2.4 Retry policy (exponential backoff + jitter, Retry-After) and error classification (transient / fatal / link-expired)
- [x] P2.5 Hierarchical rate limiter wired into the engines
- [x] P2.6 Proxy (global/per-site/per-task, system), custom headers/UA/TLS options, optional TLS impersonation fallback
- [x] P2.7 Resume persistence (throttled segment writes), task runner, and startup recovery (interrupted tasks re-queued; the P3.2 scheduler auto-starts them)

### Phase 3: Queues & scheduler
- [ ] P3.1 Queue service: CRUD for queues, built-in Main queue, move/reorder tasks
- [ ] P3.2 Scheduler: per-queue + global slots, priority, correct pause/remove semantics; drives `service.Runner` and auto-starts queued tasks at startup (incl. recovered ones)
- [ ] P3.3 Queue time windows, speed profiles, quotas, post-queue actions (platform sleep/shutdown)
- [ ] P3.4 REST + CLI for tasks (resolve, create, batch, list/filter/paginate, get, patch, pause/resume/delete, bulk ops), queues and settings (global/queue/task + effective)
- [ ] P3.5 SSE event stream + `dlm watch`

### Phase 4: Cookies & extension contract
- [ ] P4.1 Cookie store (encrypted, public suffix), jar used by engines; import/export endpoints + CLI
- [ ] P4.2 `/v1/capture` + CaptureService (creates the task with the full Source context)
- [ ] P4.3 Native Messaging host + manifest installer for all browsers and OSes

### Phase 5: Link refresh
- [ ] P5.1 Source model persistence, fingerprinting, expiry detection (incl. signed-URL params)
- [ ] P5.2 Refresh strategy chain (origin-redirect, extension-capture await, manual) + validation + history
- [ ] P5.3 Refresh endpoints/CLI and per-level refresh settings

### Phase 6: Internal browser
- [ ] P6.1 Browser discovery + chrome-headless-shell downloader, pool, profiles
- [ ] P6.2 Network sniffer (media/download/blob candidates), cookie sync both ways
- [ ] P6.3 Browser resolver engine + the `page-reload` refresh strategy; `/v1/browser/*` + CLI (resolve page, list candidates)

### Phase 7: Streams
- [ ] P7.1 ffmpeg locator/auto-downloader
- [ ] P7.2 HLS engine (VOD + live record, AES-128, variant select, fMP4, key/segment URL refresh)
- [ ] P7.3 DASH engine (MPD, audio+video selection, mux via ffmpeg)

### Phase 8: Other protocols
- [ ] P8.1 FTP/FTPS engine (resume via REST command)
- [ ] P8.2 BitTorrent engine (magnet/.torrent/base64, file select, trackers, DHT/PEX, seeding rules), instance-owned client
- [ ] P8.3 ed2k engine
- [ ] P8.4 yt-dlp engine (managed binary, `-J` formats, progress template, cookies file)

### Phase 9: Post-processing & organisation
- [ ] P9.1 Checksum verification (md5/sha1/sha256/blake3) + re-download on mismatch
- [ ] P9.2 Categories/auto-sort, file-exists policy, duplicate detection
- [ ] P9.3 Archive auto-extract, auto-torrent-from-.torrent
- [ ] P9.4 Webhooks, run-command/AV-scan post-actions
- [ ] P9.5 Mirrors/multi-source, pattern/batch URLs, import/export, history search & stats, stored credentials

### Phase 10: Integrations
- [ ] P10.1 MCP server (tools for tasks, queues, settings; progress via notifications)
- [ ] P10.2 JS plugin runtime (goja, timeouts, resolve/start/error/done hooks, `plugin` refresh strategy), plugin install from git/folder

### Phase 11: Release
- [ ] P11.1 GoReleaser v2 (archives, checksums, Homebrew/Scoop/winget, deb/rpm), versioned OpenAPI artifact
- [ ] P11.2 Docs: API guide, CLI reference (generated), architecture doc

## Verification
- **Unit tests:** settings resolution/provenance, FSM, scheduler (fake clock), limiter, refresh validation, filename parsing.
- **Integration tests:** `httptest` servers covering ranges, no-range, 200-on-range, `Retry-After`/429, slow/dying connections, expiring signed URLs (refresh), cookie-gated files, Content-Disposition edge cases. Local HLS/DASH fixtures, a local FTP server, a local BT swarm with two in-process clients.
- **Browser tests:** behind a `browser` build tag, run in CI where Chrome is available, against a local page that serves an m3u8, a blob download and cookie-gated content.
- **End to end:**
  1. `dlm serve`
  2. `dlm queue add fast --max-concurrent 2 --speed-limit 1MB`
  3. `dlm task add <url> --queue fast`
  4. `dlm watch` shows SSE progress.
  5. Kill and restart the daemon; the task resumes.
  6. The same flow through `curl` against the REST API.
- **Parity test** passes, and the CI matrix is green on all 3 OSes. Cross-builds of the 6 targets succeed with `CGO_ENABLED=0`.
