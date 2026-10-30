---
name: dlm-organize
description: Organise and tune dlm downloads - queues, schedules, night or off-peak windows, speed limits, quotas and data caps, the settings hierarchy, categories and sorted folders, labels, priorities, checksums, auto-extract, post-download commands and virus scans, duplicate handling, moving and bulk-changing tasks, import/export including aria2 lists, cleanup, retention, statistics. Use whenever someone wants to set up, sort, schedule, throttle, tidy, migrate or report on their downloads, or says things like "only at night", "don't use all my bandwidth", or "keep movies and ISOs apart", even if they do not name a dlm feature.
license: Apache-2.0
compatibility: Needs the dlm binary. Load the dlm skill first for connecting to the daemon.
metadata:
  suite: dlm
---

# Organising downloads with dlm

Connect first, as the `dlm` skill describes, and use its prefix instead of plain `dlm`.

## The model

Four tools do different jobs, and most setups use them together:

| Tool | Answers | Example |
|---|---|---|
| **Queue** | *When and how fast?* A set of tasks with its own slots, schedule, limits, quota and end action. | `night`, `metered` |
| **Category** | *Where does it land?* Sorts finished files into folders by extension or MIME type. | `Video` → `Video/` |
| **Label** | *What is it for?* Free-form key/value for filtering. | `project=thesis` |
| **Priority** | *What first within a queue?* `low`, `normal`, `high`. Order is priority, then position. | |

Before changing anything, read what exists: `dlm queue list`, `dlm category list`,
`dlm settings get`, `dlm limits`. Then explain the plan briefly. A setup the user
has already tuned is easy to break and hard to notice.

## Settings: the one idea to get right

One set of settings exists at every level, and the nearest level that sets a value
wins: **Defaults → Global → Queue → Task**. Setting something at the widest level
that makes sense keeps the setup small: put it on a queue rather than on 50 tasks.

```sh
dlm settings set maxConnections=8 retry.maxAttempts=10   # global
dlm settings unset maxConnections                        # back to the default
dlm queue set night --set speedLimit=2097152 --unset maxConnections
dlm task set ID --set speedLimit=1048576
dlm task settings ID -o json      # the effective value, and which level set it
dlm queue settings ID -o json
```

When a value is not what you expect, `dlm task settings ID` tells you which level
set it, so read that before changing anything.

Values after `=` are parsed as JSON, so objects and lists work if quoted:
`--set 'refresh.strategies=["origin-redirect"]'`. Numbers are plain: `speedLimit` is
bytes per second and rejects `2MB`, though the `--speed-limit` *flag* accepts it.

Not every setting exists at every level (`maxConcurrentDownloads` is queue or global;
`schedule` is queue only; `checksum` is task only), and a wrong level is refused with
a clear message. [references/settings.md](references/settings.md) lists every field
with its level, range and default, plus the JSON shape of the object settings. Live
schema: `dlm settings schema -o json`; defaults: `dlm settings defaults`.

## Queues

```sh
dlm queue add night --max-concurrent 2 --speed-limit 5MB
dlm queue add hold --stopped                  # collects tasks, runs nothing
dlm queue start hold      dlm queue stop hold  # stop returns running tasks to queued
dlm queue set night --name overnight
dlm queue reorder ID 0
dlm queue rm ID --move-tasks-to main           # needed when the queue has tasks
dlm queue replace-settings ID -f night.json    # replaces all overrides
```

Main (ID `main`) always exists and cannot be deleted. Names are unique ignoring case.

**Schedule**: a queue runs only inside its windows. `end` before `start` spans
midnight. Days are `sun`…`sat`; times are 24h; `timezone` is an IANA name, default local.

```sh
dlm queue set overnight --set 'schedule={"timezone":"Asia/Dhaka","windows":[{"days":["mon","tue","wed","thu","fri"],"start":"23:00","end":"06:30"}]}'
```

**Speed profiles** apply a limit only during certain hours, at queue or global level.
Use one instead of a schedule when downloads should slow down rather than stop:

```sh
dlm settings set 'speedProfiles=[{"windows":[{"days":["mon","tue","wed","thu","fri"],"start":"09:00","end":"18:00"}],"speedLimit":1048576}]'
```

**Quota** caps bytes per period, minimum `1m`. The period starts at the first byte
counted, so `24h` is not a calendar day, and a daemon restart resets usage. When it is
used up, downloads wait for the next period. `{"bytes":10737418240,"period":"24h"}` is 10 GiB a day.

**When a queue finishes**, `onQueueComplete` can run `none`, `stop`, `sleep`,
`shutdown` or `command` (absolute path, run without a shell) after `delay`, default
1 minute, at most 24 hours. New work or stopping the queue cancels it. `sleep` and
`shutdown` act on the user's machine, so confirm them first.

```sh
dlm queue set overnight --set 'onQueueComplete={"action":"sleep","delay":"5m"}'
```

## Speed limits

Limits **nest**: a task's bytes must pass its own limit, its queue's and the global
one, so the lowest wins. `dlm limits` shows what is in force.

```sh
dlm settings set speedLimit=5242880          # 5 MiB/s for everything, permanently
dlm limits override 500K -m 30               # temporarily, for 30 minutes
dlm limits override                          # "turbo": unlimited for an hour
dlm limits override --clear
```

Use an override for "for now" requests: it ends by itself and restores what the
user configured. `maxConnections` (per task, default 16) and
`maxConcurrentDownloads` (per queue, default 4) control parallelism;
`maxActiveDownloadsTotal` is a cap across all queues, 0 for none.

## Sorting into folders

Categories need `autoCategorize` on, and apply only to tasks with no `--dir` of
their own. They are applied when a download finishes, so tasks already queued are
sorted too. The first match wins, so order matters.

```sh
dlm category add Video --dir Video --ext mp4 --ext mkv --mime 'video/*'
dlm category add Archives --dir /data/archives --ext zip --ext 7z --ext iso
dlm category match --name movie.mkv          # test a rule
dlm settings set autoCategorize=true
dlm category set ID --position 0             # tried first
dlm task list --category Video
```

`--dir` is absolute, or relative to the download directory. `--ext` and `--mime` on
`category set` replace the whole list. Deleting a category leaves sorted files alone.

If files exist, `fileExistsPolicy` decides: `rename` (default, gives `name (1).ext`),
`overwrite`, `skip` (the task completes without downloading), `resume`, or `ask`.
Note that `ask` does not prompt anyone: the task fails with `file_exists`. Use
`rename` unless the user wants otherwise.

## After a download

The order is fixed: checksum, then your `postActions` in order, then category sorting.

```sh
dlm task add URL --checksum sha256:HEX
dlm settings set 'postActions=[{"kind":"extract","params":{"delete":"true"}}]'
```

| `kind` | Params (all strings) |
|---|---|
| `extract` | `dir` (default a folder named after the archive, `.` for alongside it), `delete` (`"true"`), `password`. zip, rar, 7z, tar and compressed tar, multi-part rar. |
| `add-torrent` | Turns a downloaded `.torrent` into a task. `dir`, `delete`. |
| `command` | `cmd` (absolute path, required), `args` (a JSON array **as a string**), `timeout` (default 5m). Placeholders: `{path}`, `{dir}`, `{name}`, `{taskId}`, `{url}`, `{size}`. No shell, so no pipes or globs. |
| `av-scan` | Same as `command`, plus `infectedExit` (default 1, ClamAV's) and `onInfected` (`delete` by default, or `keep`). |

```sh
dlm task add URL --set 'postActions=[{"kind":"command","params":{"cmd":"/usr/local/bin/notify","args":"[\"{name}\",\"{size}\"]"}}]'
```

**A post-action failure does not fail the task.** The task still ends `completed`,
because the file is fine, and the problem shows only in `dlm logs --level warn`. So
check the log before telling the user that extraction worked. The exception is an
infected file, which fails the task with `infected`. Post-actions run programs:
confirm with the user, and check `dlm logs` after the first run. MCP clients cannot
set them at all.

## Changing many tasks

Select by IDs, or by `--all`, `--queue` and `--status` together. These select only
by queue and status; to act on a search or label, list the IDs first:

```sh
dlm task move-tasks --queue main --status queued --to night
dlm task update-tasks --status queued --priority high --label batch=7 --remove-label old
dlm task update-tasks ID1 ID2 --settings '{"speedLimit":1048576,"retry":null}' --to night
dlm task restart-tasks --status error
dlm task refresh-tasks --status awaiting_link
dlm task pause --queue main          # also: resume, rm
dlm task list --label project=thesis --min-size 1GB --sort size --all -o json
```

`task list` filters: `--status` (several allowed), `--queue`, `--search`, `--engine`,
`--label KEY[=VALUE]`, `--category`, `--min-size`/`--max-size`,
`--created-after`/`--created-before`, `--completed-after`/`--completed-before`
(RFC 3339), with `--sort position|created|name|size|completed`, `--order`, `--limit`,
`--cursor`, `--all`, `--total`.

One at a time: `dlm task move ID QUEUE`, `dlm task reorder ID POS`. `task set --label`
replaces **all** of a task's labels; `update-tasks --label` adds.

## Moving lists in and out

```sh
dlm task import backup.json --dry-run        # shows per entry what would happen
dlm task import downloads.aria2 --paused
dlm task import - --format urls < urls.txt
dlm task export --format urls --status completed
dlm task export -f backup.json               # format follows .json, .aria2, .txt
```

`task export --queue` needs the queue's ID: given a name it prints nothing, with no
error. Every other `--queue` here accepts a name or an ID.

A dry run exits 1 when any entry would fail. That is the report doing its job: read
the per-entry results, then import, adding `--allow-duplicate` only if wanted. Exports
carry link, mirrors, name, directory, referrer and user agent, never captured
headers or cookies, so a re-imported gated link may need its login set up again.

## Tidying up

```sh
dlm task cleanup --older-than 30 --dry-run   # always preview
dlm task cleanup --older-than 30             # removes list entries, keeps files
dlm task cleanup --all --files               # also deletes files; confirm first
dlm settings set retentionDays=90            # automatic, hourly; never touches files
```

## Reporting

```sh
dlm stats -o json                            # by status and engine, last 24h/7d/30d
dlm stats timeline --bucket day --group-by engine
dlm queue list -o json                       # with per-queue status counts
```

Timeline buckets are `hour` or `day` in UTC, at most 1000, from `--from` to `--to`.

## Ready-made setups

[references/recipes.md](references/recipes.md) has complete command sequences:
off-peak-only downloading, metered connections, a sorted media library, a verified
software archive, and migrating from aria2.

## Settings that are accepted but do nothing

`filenameTemplate` is validated and stored, but no code reads it, so it has no
effect. To control names, use `--name`, or rename from a `command` post-action.
