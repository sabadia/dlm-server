# dlm-server

Cross-platform download manager backend written in Go. It runs as a local daemon
exposing a REST API (with Server-Sent Events) and ships a CLI with full feature
parity. A Flutter desktop UI and a Chromium/Firefox extension will consume the
same API.

> Status: early development. See [docs/PLAN.md](docs/PLAN.md) for the
> authoritative implementation plan and the project board for progress.

## Highlights (planned)

- Multi-connection segmented HTTP(S) downloads with resume, plus FTP/FTPS, HLS/DASH,
  BitTorrent/magnet (with seeding rules), ed2k and an optional yt-dlp engine
- Multiple independent queues with Global → Queue → Task settings inheritance
- Link refresh using the stored origin page, extension capture or a headless browser
- Internal headless Chromium (chromedp) and an encrypted cookie store
- Hierarchical speed limits, scheduling, categories, checksums, post-actions
- Loopback-only by default, bearer-token auth, OpenAPI 3.1 spec

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
