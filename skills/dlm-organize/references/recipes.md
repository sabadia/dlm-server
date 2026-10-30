# Ready-made setups

Each recipe is a complete sequence. Adapt names and numbers to the user's situation,
and read the current state first so you extend it rather than replace it.

## Contents

- [Download only off-peak](#download-only-off-peak)
- [Stay under a data cap](#stay-under-a-data-cap)
- [Keep working while downloading](#keep-working-while-downloading)
- [A sorted media library](#a-sorted-media-library)
- [A verified software archive](#a-verified-software-archive)
- [A large batch that must not fail silently](#a-large-batch-that-must-not-fail-silently)
- [Migrate from aria2](#migrate-from-aria2)
- [Rescue a mess of failed tasks](#rescue-a-mess-of-failed-tasks)

## Download only off-peak

```sh
dlm queue add night --max-concurrent 3 \
  --set 'schedule={"windows":[{"days":["sun","mon","tue","wed","thu","fri","sat"],"start":"01:00","end":"07:00"}]}'
dlm task add -f urls.txt --queue night
```

Outside the window tasks stay `queued`, which is correct; say so, or the user may
think it is stuck. Set `timezone` if the machine's zone differs from theirs.

## Stay under a data cap

```sh
dlm settings set 'quota={"bytes":21474836480,"period":"24h"}'   # 20 GiB a day
dlm limits                                                      # usage so far
```

A window opens with the first byte counted and lasts one period, so `24h` is a rolling
day from then, not a calendar day. Usage is kept in memory: a daemon restart resets it.
Use a queue-level quota when only some downloads are metered. A `quota.exhausted` event fires when it runs out,
and the downloads continue after `quota.reset`.

## Keep working while downloading

```sh
dlm settings set 'speedProfiles=[{"windows":[{"days":["mon","tue","wed","thu","fri"],"start":"09:00","end":"18:00"}],"speedLimit":1048576}]'
dlm limits override 512K -m 60     # or just for the next hour
```

## A sorted media library

```sh
dlm category add Movies --dir /media/movies --ext mp4 --ext mkv --ext webm --mime 'video/*'
dlm category add Music  --dir /media/music  --ext mp3 --ext flac --ext m4a --mime 'audio/*'
dlm category add Books  --dir /media/books  --ext pdf --ext epub
dlm category match --name film.mkv          # check before relying on it
dlm settings set autoCategorize=true
```

Files given `--dir` skip sorting, so leave it off for tasks that should be filed.

## A verified software archive

```sh
dlm queue add archive --max-concurrent 2 \
  --set 'postActions=[{"kind":"extract","params":{"dir":"."}}]' --set retry.maxAttempts=10
dlm task add 'https://example.org/tool.tar.gz' --queue archive \
  --dir /data/archive --checksum sha256:HEX --mirror 'https://mirror.example.org/tool.tar.gz'
```

Then check `dlm logs --level warn`, since a failed extraction leaves the task
`completed`.

## A large batch that must not fail silently

```sh
dlm task import urls.txt --dry-run -o json > plan.json   # exits 1 if some would fail
# read plan.json: fix or drop bad entries, tell the user about duplicates
dlm queue add batch --max-concurrent 4
dlm task import urls.txt --queue batch -o json > added.json
python3 ../dlm/scripts/dlm_wait.py --queue batch --timeout 7200
```

`dlm_wait.py` exits 3 if anything needs attention, and its report lists what. Report
counts of completed and failed, not just "done". On a timeout (exit 4) the work
continues, so run it again later. `--stall` sets how long without progress counts as
stuck; raise it for a queue with a schedule.

## Migrate from aria2

```sh
dlm task import session.aria2 --dry-run
dlm task import session.aria2 --paused        # review, then resume
dlm task resume --status paused
```

aria2 input files carry the link, `out=` and `dir=`. Other aria2 options, headers and
cookies do not come across, so set those up separately. Partial aria2 files are not
reused.

## Rescue a mess of failed tasks

```sh
python3 ../dlm/scripts/dlm_doctor.py          # what is wrong, grouped by cause
dlm task list --status error -o json          # read error.code for each
dlm task refresh-tasks --status awaiting_link
```

Fix by cause rather than resuming everything: `network` can resume, `link_expired`
needs a new link, and `checksum_mismatch` needs a human decision. After fixing:

```sh
dlm task resume --status error
python3 ../dlm/scripts/dlm_wait.py --all --timeout 3600
```
