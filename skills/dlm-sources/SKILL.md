---
name: dlm-sources
description: Download from hard sources with dlm - video and audio pages through yt-dlp, HLS and DASH streams and quality selection, BitTorrent, magnet and .torrent files with file selection and seeding, FTP/FTPS, ed2k, sites needing a login, cookies or Basic/Digest credentials, pages needing JavaScript through the internal headless browser, blob downloads, and expired or signed links that must be refreshed. Use when a link is a page rather than a file, a download returns HTML or 403, or a link has expired, or when someone says "download this video", "grab this stream", "this needs my login", or "the link died", even if dlm is not named.
license: Apache-2.0
compatibility: Needs the dlm binary. ffmpeg, yt-dlp and a browser are found on PATH or auto-downloaded by the daemon. Load the dlm skill first for connecting.
metadata:
  suite: dlm
---

# Getting files from difficult sources

Connect first, as the `dlm` skill describes. Waiting and status handling are the same
for every engine: use `dlm_wait.py` from that skill.

## Choosing the route

`dlm system info -o json` lists the engines and whether ffmpeg, yt-dlp and a browser
are available. A missing tool is fetched on first use unless auto-download is off.

| Source | Engine | How |
|---|---|---|
| Direct link | `http` | `dlm task add URL` |
| `.m3u8` | `hls` | `dlm task add URL [--format 720]` |
| `.mpd` | `dash` | `dlm task add URL [--format 720]` |
| Page with a video | `ytdlp` | `dlm task add --ytdlp URL` |
| `magnet:`, `.torrent` link or local file | `bt` | `dlm task add LINK-OR-PATH` |
| `ftp://`, `ftps://` | `ftp` | `dlm task add URL`; resumes |
| `ed2k://` | `ed2k` | `dlm task add LINK` |
| A page only a real browser can work through | `browser` | `dlm browser resolve URL` |

Work down from cheap to expensive: try yt-dlp for a known video site, then the
browser.

A page is not always an error to dlm. If the URL names a file type (`manual.pdf`) and
HTML comes back, dlm treats the link as expired and the task goes to `awaiting_link`.
If the URL has no extension (`/download`, `/get?id=7`), it saves the HTML and ends
`completed` with a name like `download.html`. So when a link might be a page, run
`dlm resolve URL -o json` first: it fails for the first kind, and for the second
`source.fingerprint.contentType` reads `text/html`. A 401, 403, 404 or 410 goes to
`awaiting_link` as well (see "Expired links").

## Video and audio pages

```sh
dlm task add --ytdlp 'https://video.example/watch?v=abc'
dlm task add --ytdlp URL --format 'bv*[height<=720]+ba/b'    # a yt-dlp selector
dlm task add --ytdlp URL --format 'ba/b' --name talk.m4a     # audio only
```

`dlm resolve URL -o json` lists `formats`, best first, for HLS and DASH links. Each
has an `id`, a `label` (`1080p`, `audio only (m4a)`), size and codecs. `dlm resolve`
has no `--ytdlp` flag and treats a video page as a file, so to list a page's formats
use the API (see `dlm-admin`):
`POST /v1/resolve` with `{"source":{"currentUrl":URL,"resolverKind":"ytdlp"}}`.
Pass a chosen `id` to `--format`, or as `source.mediaHint` over the API.

If yt-dlp needs a login, import cookies (below): dlm hands them to it. It reports a
failure on the task, so wait and read `error`, and look in `dlm logs` for details.

## HLS and DASH

`--format` takes a quality such as `720`. HLS covers VOD and live recording, AES-128
and fMP4; DASH selects audio and video and muxes them with ffmpeg. Both resume.

A manifest whose URL does not end in `.m3u8` or `.mpd` (a signed or API link) must be
marked as one, or it is fetched as a plain file. Use `streamKind` through a capture
or the API, since `task add` has no flag for it:

```sh
echo '{"url":"https://cdn.example/api/stream?id=1","mediaCandidates":[{"url":"https://cdn.example/api/stream?id=1","kind":"hls"}]}' | dlm capture -r -
```

## Torrents

```sh
dlm task add 'magnet:?xt=urn:btih:…' --paused        # paused lets you choose files
dlm task add ./linux.iso.torrent 'https://x/y.torrent'
dlm task files ID                                    # the files, with indexes
dlm task files ID --select 0,3                       # only these from now on
dlm task resume ID
dlm task torrent ID -o json                          # state, peers, ratio, trackers
```

`task files` needs the task paused or not started (409 `task.files_locked` while
active). A magnet has no file list until its metadata arrives, up to
`torrent.metadata_timeout` (2m), and `dlm task torrent` shows `metadata` meanwhile.

A finished torrent stays `running` while it seeds (`state: seeding`); `dlm_wait.py`
treats that as done. It stops when it reaches `torrent.seedRatio` (default 1) or
`torrent.seedTime` (default 2h). Set both to `0` to skip seeding, or
`torrent.seedKeep=true` to seed until paused:

```sh
dlm task add MAGNET --set torrent.seedRatio=0 --set torrent.seedTime=0
dlm task add MAGNET --set 'torrent.trackers=["udp://tracker.example:1337/announce"]'
```

Only fetch torrents the user has the right to share: seeding uploads to strangers.
Boot options such as DHT, PEX, ports and upload limits are in `dlm-admin`.

## Logins

Choose by how the site authenticates:

**HTTP Basic or Digest** (a browser pop-up prompt): store a login. The host may be a
wildcard. Pass the password on stdin so it stays out of the process list and history:

```sh
printf '%s\n' "$PASSWORD" | dlm credential add files.example.com --user ann --password-stdin
dlm credential list                                   # passwords are never shown
```

**A session from a web login**: import cookies. Every engine, the browser and yt-dlp
share one encrypted store. Export `cookies.txt` from the user's browser, then:

```sh
dlm cookies import cookies.txt            # Netscape or JSON, detected
dlm cookies list --domain example.com     # names only, never values
dlm cookies rm ID
dlm cookies clear --domain example.com
```

`cookies export` writes values to stdout or a file (mode 0600). Treat that as a
password file, and prefer `--file` so it never lands in a transcript.

**A one-off token or header**:
`dlm task add URL --header 'Authorization=Bearer …'`. Headers are saved with the task
but left out of exports. `Cookie` works as a header on `task add`, and is ignored on
`capture`, which takes real cookies.

**Everything a browser sent**: use `capture`, which also keeps what link refresh
needs:

```sh
dlm capture https://cdn.example.com/f.zip --page-url https://example.com/get \
  --referrer https://example.com/ --user-agent 'Mozilla/5.0 …' --cookie-scope task
```

`--cookie-scope task` keeps cookies for this download only, removed when it ends;
`store` adds them to the store. `--resolver direct|ytdlp|browser` chooses how the link
is treated. Cookies, media candidates, form bodies (`method`, `body` as base64, up to
256 KiB) need the JSON form: `dlm capture -r capture.json`. Fields are in the
`CaptureBody` schema of `dlm openapi`.

## The headless browser

Use it for a page that builds its download with JavaScript, sits behind a challenge,
or plays through `blob:` media. It starts on demand with the stored cookies, and
watches the network for what the page fetches.

```sh
dlm browser resolve 'https://example.com/watch?v=1' --wait 30 -o json
dlm browser download RESULT_ID 0 --queue Main --name movie.mp4
```

`resolve` returns a result ID and candidates, each with `index`, `kind`, `mime`,
`size` and `url`. Pick by size and kind: the largest media or manifest is usually the
real one, and small ones are often ads or previews. Results are kept only for a while.

```sh
dlm browser status
dlm browser profiles list                 # one profile per site group, keeps logins
dlm browser profiles rm GROUP
```

`browserUsage` (`never`, `auto`, `always`) limits when it is used. Some sites refuse
Go's TLS fingerprint with a handshake failure or challenge page: turn on
`http.tls_impersonation` (`dlm config set http.tls_impersonation true`, then restart).
Challenges that need JavaScript still need the browser.

Solving a challenge meant to stop automation is not always welcome. If a site is
clearly refusing automated access, say so rather than working around it.

## Expired links

Every task remembers where its link came from, so a dead link can often be renewed
without losing progress. dlm detects expiry from 401/403/404/410, HTML arriving where the
URL or an earlier response promised a file, a changed
size or ETag, and signed-URL parameters (`X-Amz-Expires`, `Expires`,
`se`, `exp` and similar) before the link actually dies.

`refresh.strategies` runs in order. The default is the first three:

| Strategy | Does | Needs |
|---|---|---|
| `origin-redirect` | Re-requests the original URL | An original URL that redirects to a fresh link |
| `page-reload` | Reopens the page in the browser and sniffs the link | A `pageUrl`: use `capture --page-url`, or `set-url --page-url` |
| `extension-capture` | Waits up to `refresh.awaitTimeout` (10m) in `awaiting_link` for the extension | The browser extension |
| `plugin` | Asks a plugin's `refresh` hook; opt-in | A plugin, see `dlm-admin` |
| `manual` | Waits for you to give a URL | `dlm task set-url` |

A new link is probed and its fingerprint compared before it replaces the old one. A
match resumes where it stopped. A mismatch follows `refresh.onMismatch`: `fail`
(default), `restart`, or `ask`, which parks the task for a decision.

```sh
dlm task refresh ID                     # run the chain now; pause a running task first
dlm task refresh-tasks --status awaiting_link
dlm task refresh-history ID -o json     # read this first when a refresh fails
dlm task set-url ID 'URL' --page-url P --referrer R --header 'A=B' --on-mismatch restart
dlm task await-link ID                  # hand it to the extension again
```

Because `page-reload` needs a page, give dlm one whenever you know it. A task added
with `capture --page-url` can renew itself, and one added with a bare signed URL
cannot. `dlm task resume` alone does not fix a dead link: it fails again.

To stop dlm waiting for the extension, set the chain without it:
`--set 'refresh.strategies=["origin-redirect"]'` (or `refresh.auto=false` for no
refresh). The task then goes to `error` at once.

## Mirrors and big files

`--mirror` adds links to the same file. Each is probed and kept only if it has the
same size and supports ranges; a failing mirror is dropped without failing the task.
Direct HTTP uses several connections, resumes across restarts and writes to
`name.part` until complete. A server without range support falls back to one stream.

Retries cover network errors, 5xx and 429 with backoff, honouring `Retry-After`, up to
`retry.maxAttempts`. For a rate-limited site, lower `maxConnections` before raising
retries: dlm cannot out-wait a limit it keeps tripping.
