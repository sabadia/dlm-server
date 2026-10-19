# dlm-server

A cross-platform download manager that runs as a local daemon. It has a REST API
(with Server-Sent Events), a command-line client with a command for every API
operation, and an MCP endpoint for AI assistants. A Flutter desktop UI and a
Chromium/Firefox extension are built on the same API.

## What it does

- **Protocols:** multi-connection HTTP(S) with resume, FTP/FTPS, HLS and DASH
  streams, BitTorrent and magnet links (with seeding rules), ed2k, and an
  optional yt-dlp engine for video pages.
- **Queues and settings:** several independent queues, with settings that inherit
  Global → Queue → Task and show where each value comes from. Speed limits nest the
  same way; queues have schedules, quotas and an action when they finish.
- **Links that expire:** each task remembers the page its link came from. When the
  link dies it is refreshed from the original URL, by reopening the page in an
  internal headless browser, by a plugin, or by the browser extension.
- **Browser and cookies:** an internal headless Chromium for cookie-gated and
  streaming pages, and an encrypted cookie store shared by every engine.
- **After a download:** checksum check (re-download on mismatch), sorting into
  category folders, archive extraction, `.torrent` files turned into torrent
  tasks, your own commands, a virus-scan hook, and webhooks.
- **Organising:** duplicate detection, mirrors, URL patterns (`img[001-100].jpg`),
  import/export of JSON, URL lists and aria2 input files, statistics, and stored
  logins for Basic/Digest sites.
- **Extending:** JavaScript plugins, and an MCP server.

## Install

Download an archive, `.deb` or `.rpm` from the
[releases](https://github.com/sabadia/dlm-server/releases) page, or:

```sh
brew install sabadia/tap/dlm          # macOS, Linux
scoop bucket add sabadia https://github.com/sabadia/scoop-bucket && scoop install dlm
winget install sabadia.dlm
```

Or build it (Go 1.27+, no cgo): `make build` puts the binary in `bin/dlm`.

## Quick start

```sh
dlm serve &                            # the daemon; binds to 127.0.0.1:17171
dlm task add https://example.com/big.iso
dlm watch                              # live progress
dlm task list --status running
```

The first start writes a random API token to the config directory (`dlm token
show` prints where). The CLI reads it by itself; other clients send it as
`Authorization: Bearer <token>`. The daemon only listens on loopback unless you
turn on remote mode, which requires TLS.

More:

```sh
dlm queue add night --max-concurrent 2    # a second queue
dlm task add -f urls.txt --queue night    # many links at once
dlm task add 'https://example.com/img[001-100].jpg' --expand
dlm settings set speedLimit=2097152 maxConnections=8
dlm webhook add https://example.com/hook --event task.status --secret s3cret
dlm plugin install ./my-plugin
```

## Documentation

| | |
|---|---|
| [docs/API.md](docs/API.md) | Using the REST API, events, webhooks and the MCP endpoint |
| [docs/CLI.md](docs/CLI.md) | Every command and flag (generated) |
| [docs/openapi.json](docs/openapi.json) | The OpenAPI 3.1 document (generated; the daemon serves it at `/openapi.json` and a UI at `/docs`) |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | How the code is organised and how a download runs |
| [docs/PLUGINS.md](docs/PLUGINS.md) | Writing a JavaScript plugin |
| [docs/RELEASING.md](docs/RELEASING.md) | How releases are made |
| [docs/CONVENTIONS.md](docs/CONVENTIONS.md) | Engineering rules for contributors |
| [docs/PLAN.md](docs/PLAN.md) | The implementation plan this project follows |

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
