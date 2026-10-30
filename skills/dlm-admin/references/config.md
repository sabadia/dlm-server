# Boot configuration keys

Read at daemon start. Change with `dlm config set KEY VALUE`, then `dlm system restart`.
Environment form: `DLM_` + the key in upper case with `.` as `_`, so
`server.fs_roots` is `DLM_SERVER_FS_ROOTS`. The config file is `config.yaml` in the
config directory, or `--config` / `DLM_CONFIG`. Lists are comma-separated.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `home` | string | none | Put all directories below this. Read-only here: use `--home` or `DLM_HOME`. |
| `paths.config_dir` | string | per OS | Config, token, `runtime.json`. Read-only here: use `--config-dir`. |
| `paths.data_dir` | string | per OS | Database and state. |
| `paths.cache_dir` | string | per OS | Downloaded tools and caches. |
| `paths.log_dir` | string | under data | Rotating logs. |
| `server.address` | string | `127.0.0.1:17171` | Listen address; port 0 picks a free one. |
| `server.allow_remote` | bool | false | Allow a non-loopback bind; needs TLS. |
| `server.tls_cert`, `server.tls_key` | string | none | Certificate and key files. |
| `server.allowed_origins` | list | none | Browser origins allowed to call the API. No wildcard. |
| `server.fs_roots` | list | none | Directories `dlm fs` may show. Empty turns it off. |
| `server.shutdown_timeout` | duration | 10s | Grace period when stopping. |
| `log.level` | string | info | debug, info, warn, error. |
| `log.format` | string | json | json or text. |
| `log.stderr` | bool | false | Also log to stderr. |
| `log.file` | bool | true | Log to a rotating file. |
| `log.max_size_mb`, `log.max_backups`, `log.max_age_days` | int | 20, 5, 30 | Rotation. |
| `browser.path` | string | detected | Browser executable. Empty: Chrome, Chromium, Edge or Brave, else a download. |
| `browser.auto_download` | bool | true | May download chrome-headless-shell. |
| `browser.max_browsers` | int | 2 | Browser processes at once; one per site group. |
| `browser.idle_timeout` | duration | 2m | How long an idle browser stays. |
| `browser.no_sandbox` | bool | false | For containers running as root. |
| `ffmpeg.path` | string | detected | Empty: PATH, else a downloaded static build. |
| `ffmpeg.auto_download` | bool | true | May download ffmpeg. |
| `ytdlp.path` | string | detected | Empty: PATH, else a managed download. |
| `ytdlp.auto_download` | bool | true | May download yt-dlp. |
| `http.tls_impersonation` | bool | false | Retry refused TLS handshakes with a Chrome fingerprint. |
| `http.impersonation_ttl` | duration | 1h | How long a host that needed it is remembered. |
| `torrent.enabled` | bool | true | The BitTorrent engine. |
| `torrent.listen_host`, `torrent.listen_port` | string, int | any, 0 | Where peers connect. |
| `torrent.dht`, `torrent.pex` | bool | true | Peer discovery. |
| `torrent.port_forwarding` | bool | true | UPnP/NAT-PMP. |
| `torrent.upload_limit` | int | 0 | Bytes per second; 0 is unlimited. |
| `torrent.metadata_timeout` | duration | 2m | How long a magnet may take to fetch metadata. |
| `ed2k.enabled` | bool | true | The ed2k engine. |
| `ed2k.listen_port` | int | 0 | Listen port. |
| `ed2k.servers` | list | none | Servers to use. |
| `ed2k.server_met` | string | none | A `server.met` file or URL. |
| `ed2k.kad` | bool | false | Use Kad. |
| `ed2k.upnp` | bool | false | Forward the port. |
| `ed2k.upload_limit_kb` | int | 0 | KiB per second; 0 is unlimited. |

`dlm config get` shows live values with `source` and `pending`. Errors:
`config.unknown_key`, `config.read_only`, `config.bad_value`, `config.invalid`. Nothing
is written when one occurs. Other keys of the file are kept, but comments are not.
