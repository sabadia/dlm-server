---
name: dlm
description: Operate dlm, the local download-manager daemon and CLI, to download anything and see it through to a verified file. Use for any request to download, fetch, grab or save a file, batch of links, torrent, magnet, video or stream on a machine with dlm, and to check, pause, resume, retry, fix, list or delete downloads, even if the user never says "dlm". Use whenever someone pastes a `dlm` command or error, or asks why a download is stuck, failed, slow or missing. Start here for all dlm work; it routes to dlm-organize, dlm-sources and dlm-admin.
license: Apache-2.0
compatibility: Needs the dlm binary. The bundled scripts need Python 3.9+ and nothing else.
metadata:
  suite: dlm
---

# dlm: getting downloads done

dlm is one binary. `dlm serve` is a long-running daemon that does the downloading,
and every other `dlm` command is a client of it, so a download keeps running after
your command returns and survives a restart. Nothing here needs a terminal UI, and
the same work is available over REST and MCP.

This skill covers the daily loop: connect, add, follow, react. Three companion
skills go deeper, and you should load them when the task calls for it:

| Task | Skill |
|---|---|
| Queues, schedules, speed limits, quotas, settings, categories, post-processing, bulk changes, import/export, cleanup, stats | `dlm-organize` |
| Video pages, HLS/DASH, torrents, magnet, FTP, ed2k, logins, cookies, the headless browser, expired links | `dlm-sources` |
| Installing, daemon lifecycle, boot config, tokens, remote use, webhooks, events, plugins, browser extension, REST and MCP | `dlm-admin` |

The full command list is in [references/cli.md](references/cli.md), generated from
the binary. `dlm COMMAND --help` is the final word if it disagrees with anything here.

## 1. Connect first

Do not assume plain `dlm` reaches the daemon. The desktop app runs its own daemon
under a custom `--home` on a random port, so `dlm health` can say "not running"
while downloads are in progress. Find it:

```sh
python3 scripts/dlm_connect.py          # prints the prefix, e.g.  dlm --home '/path'
python3 scripts/dlm_connect.py --json   # every daemon found, and where
python3 scripts/dlm_connect.py --start  # start one if none is running
```

Paths are relative to this skill's directory. Use the printed prefix in place of
`dlm` for the rest of the session; examples below write plain `dlm`. If several
daemons are running, ask which one is meant rather than guessing: each has its own
tasks and settings. A prefix like `dlm --server URL --token T` reaches a remote one.

Without Python, try `dlm health`, then `dlm --home DIR health` for the desktop app's
directory (`dlm/daemon` under the user's application-support directory).

If `dlm` is not installed, see "Installing" in `dlm-admin`.

## 2. Add a download

```sh
dlm task add 'https://example.com/big.iso' -o json
```

Quote URLs: `?`, `&` and `[ ]` mean things to the shell. Read `id` from the JSON and
keep the **full** ID. The table view shows only the last 8 characters, and those
are not accepted as an ID by any command.

Before adding, pick the right form. dlm chooses an engine from the link, but it
cannot tell a video page from a file:

| You have | Do |
|---|---|
| A direct file link | `dlm task add URL` |
| A page with a video or audio on it | `dlm task add --ytdlp URL` (see `dlm-sources`) |
| A `.m3u8` or `.mpd` link | `dlm task add URL`; `--format 720` picks a quality |
| `magnet:`, a `.torrent` URL or file, `ed2k://`, `ftp://`, `ftps://` | `dlm task add LINK-OR-PATH` |
| A page that needs a login or JavaScript | `dlm-sources` |
| Many links | `dlm task add -f urls.txt`, or `dlm task import FILE --dry-run` first |
| A numbered series | `dlm task expand 'URL[001-100].jpg'`, then `dlm task add 'URL[001-100].jpg' --expand` |

Useful flags on `task add`:

| Flag | Meaning |
|---|---|
| `--dir /abs/path` | Destination; must be absolute. Default: the effective `downloadDir`. |
| `--name file.ext` | File name, no directories; single URL only. |
| `--queue NAME` | Queue, by name or ID. Default: Main. |
| `--checksum sha256:HEX` | Verify (md5, sha1, sha256, blake3). Use it whenever the publisher gives a hash. |
| `--mirror URL` | Another link to the same file; repeatable. |
| `--priority low\|normal\|high`, `--paused` | Order within the queue; add without starting. |
| `--speed-limit 2MB`, `--connections 8` | Per-task limits. |
| `--header 'Name=Value'` | Extra request header; repeatable. |
| `--set path=value` | Any task-level setting, e.g. `--set retry.maxAttempts=10`. |
| `--allow-duplicate` | Download something dlm already has. |

To look without downloading, use `dlm resolve URL -o json` (engine, name, size,
range support, formats for a stream, `duplicateOf`). `dlm resolve-many URL...` does
up to 100, and exits 0 even when some fail, with the reason in a per-URL note.
Resolve when the user wants the size or file name first, or when a link might be
dead: a failing probe (exit 1, `Bad Gateway … [link_expired]`) costs nothing, while a
failed task needs cleanup. If it succeeds, look at `source.fingerprint.contentType`:
`text/html` means you found a page, not a file.

**Duplicates are refused**, exit 1 with `Conflict`, and the message names the
existing task. That usually means the work is already done, so look at that task
before reaching for `--allow-duplicate`.

## 3. Wait for it

`task add` returning means *queued*, not *downloaded*. Never report a download as
done until its status is `completed`. There is no blocking command in the CLI, so
use the bundled script, which also stops early when a task needs help:

```sh
python3 scripts/dlm_wait.py ID [ID...] --timeout 600
python3 scripts/dlm_wait.py --queue night --timeout 3600
```

It prints a JSON report with each task's `outcome` and, for finished ones, the
`path`. Exit status: `0` all completed or seeding, `1` daemon unreachable, `3` a task
needs attention, `4` timed out while tasks still move. On a timeout the download
continues in the daemon: report the progress and carry on, do not re-add it.

For one look, `dlm task get ID -o json`. The file is at `dir` + `name`, and `size`
is `-1` until known. `dlm watch -o json --task ID` streams events as JSON lines but
never exits by itself, so run it only with a timeout around it.

Over MCP, `wait_for_download` does this for you.

## 4. React to the status

| Status | Means | Do |
|---|---|---|
| `queued` | Waiting for a slot | Normal unless it stays there: run the doctor. A stopped queue, a schedule, a quota or a full queue all look identical from here. |
| `running` | Transferring. A torrent stays `running` while seeding. | Wait. |
| `postprocessing` | Verifying, extracting, sorting | Wait. |
| `completed` | Finished and in place, which does not prove it is the right file | Check it (below), then report `dir`/`name`. |
| `paused` | Stopped on request; never restarts alone | `dlm task resume ID` |
| `refreshing` | Getting a new link | Wait. |
| `awaiting_link` | The link died and dlm is waiting for a new one | See below. |
| `error` | Failed; read `error.code` | See below. |

**`awaiting_link` is the trap.** A 401, 403, 404, 410, or an HTML page from a URL
that names a file type, counts as an expired link, not a failure. With default settings the task goes
quiet here for 10 minutes (`refresh.awaitTimeout`) waiting for the browser extension,
then becomes `error`. Do not sit waiting for that:

```sh
dlm task refresh-history ID -o json          # what was tried and why it failed
dlm task set-url ID 'https://new/link'       # give it a working link
dlm task rm ID                               # or give up on it
```

For a one-off link where a dead URL should simply fail, add
`--set refresh.auto=false`: the task goes straight to `error`. Refresh in depth is in
`dlm-sources`.

| `error.code` | Means | Do |
|---|---|---|
| `network` | Connection failed after all retries | Check the host, then `dlm task resume ID` |
| `http_status` | Server refused (not an expiry) | Inspect headers or cookies; see `dlm-sources` |
| `link_expired` | Refresh found no link | `dlm task set-url ID URL`. Resuming alone fails again. |
| `checksum_mismatch` | File differs from the hash | dlm already downloaded it `retry.maxAttempts`+1 times. Tell the user: the hash or the source is wrong. Do not use the file. |
| `file_exists` | Target exists and policy is `ask` | `dlm task set ID --set fileExistsPolicy=rename`, then resume |
| `disk_full` | No space | Free space, or `dlm task set ID --dir /other`, then resume |
| `browser` | Headless browser could not open the page | `dlm browser status`, `dlm logs --level warn` |
| `postprocess` | A step after the download failed | `dlm logs --level warn` |
| `infected` | Virus scan flagged it | Tell the user. Do not retry. |
| `fatal` | Anything else unrecoverable | Read `error.message` |

**`completed` can still be wrong.** When a URL has no file extension (`/download`,
`/get?id=7`) and answers with a web page, such as a login form, dlm saves it as
`download.html` and ends `completed`: it has no way to know you wanted something else.
(A URL ending in `.pdf` or `.zip` is caught, and goes to `awaiting_link`.) Check that
the `name` and `size` are plausible; `dlm_wait.py` adds a `warning` when the result looks like a web page. If
it is one, the link was not a file: delete the task with `--files` and see `dlm-sources`.

When several things look wrong, or a task will not start, run the read-only doctor.
It checks tools, disk, queues, quotas, overrides and every unfinished task, and
gives the command that fixes each problem:

```sh
python3 scripts/dlm_doctor.py
```

Beyond that: `dlm logs --level warn -n 50`, and `dlm task get ID -o json`.

## 5. Control tasks

```sh
dlm task list --status running              # also --search, --queue, --limit, --all
dlm task pause ID...     dlm task resume ID...
dlm task set ID --priority high --set speedLimit=1048576
dlm task set-url ID URL                     # keeps progress when the file matches
dlm task restart ID                         # from scratch, discards progress
dlm task rm ID                              # keeps the files
dlm task rm ID --files                      # also deletes them
```

`pause`, `resume` and `rm` also select with `--all`, `--queue` and `--status`, and
accept several IDs. They print one result per task and exit 1 if any failed, so
check each result rather than the exit code alone: pausing a completed task is a
per-item conflict, not a broken command.

Pause before `set-url`, `refresh` or `task files` on a running task. Resume continues
from saved progress, and so does a daemon restart.

## Output and errors

Use `-o json` whenever a program reads the result; tables truncate IDs and round
sizes. It goes before or after the subcommand; `yaml` also works.

CLI exit status: `0` ok, `1` failed, `2` usage error. A `2` means your command is
wrong, so read `--help` instead of retrying. API failures read
`error: Conflict: …`, `Not Found: …`, `Unprocessable Entity: …`; codes are listed in
`dlm-admin`.

Sizes accept `500K`, `2M` (×1024), `2MB` (×1000) and `2MiB` in *flags*, but `--set`
and `dlm settings set` take plain numbers: `speedLimit=2097152`, not `2MB`.

## Using dlm responsibly

These are the actions to pause on, because they cannot be undone or reach beyond
the task at hand:

- **Confirm first:** `--files`, `--all`, `task cleanup --files`, `queue rm`,
  `token rotate`, `system stop`, and anything that makes a queue sleep, shut down or
  run a command. Preview with `--dry-run` where it exists, and show the user the count.
- **Keep secrets out of commands and output.** Do not print the API token. Pass
  passwords through `--password-stdin`. Do not paste cookie exports into a reply.
- **Treat downloads as untrusted.** Do not run, source or install a downloaded file
  unless the user asked for that, and verify a checksum first when one exists.
- **Stay within what the user may access.** dlm can reuse logins and cookies. Do not
  use that to get around paywalls or access controls, or to fetch material the user
  has no right to.

## Where things are documented

- [references/cli.md](references/cli.md): every command and flag.
- [references/rest-api.md](references/rest-api.md): every REST operation, mapped to its command.
- `scripts/maintain_skills.py`: for contributors; regenerates those two files and
  checks that the suite still covers all of dlm.
