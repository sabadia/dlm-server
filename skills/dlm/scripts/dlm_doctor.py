#!/usr/bin/env python3
"""Explain, read-only, why dlm downloads are not doing what they should.

Checks the daemon, tools, disk space, queues, limits and every unfinished task,
and prints each problem with the command that fixes it.

    dlm_doctor.py [--json] [--cli 'dlm --home /path']

Exit status: 0 nothing wrong, 1 no daemon, 3 problems found.
Standard library only. It changes nothing.
"""

from __future__ import annotations

import argparse
import json
import os
import shlex
import shutil
import subprocess
import sys
from collections import Counter
from typing import Any

sys.dont_write_bytecode = True  # leave no __pycache__ in the skill
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dlm_connect  # noqa: E402

LOW_DISK_BYTES = 1 << 30

ERROR_FIXES = {
    "network": "connection failed after every retry: check the host, then `dlm task resume ID`",
    "http_status": "the server refused: `dlm task get ID -o json`, check headers, cookies or the site's limits",
    "link_expired": "get a fresh link: `dlm task set-url ID URL`",
    "browser": "the headless browser failed: `dlm browser status`, `dlm logs --level warn`",
    "disk_full": "free space or move the task: `dlm task set ID --dir /other/dir`, then resume",
    "checksum_mismatch": "the file differs from the expected hash: check the hash and the source; do not trust the file",
    "file_exists": "`dlm task set ID --set fileExistsPolicy=rename`, then `dlm task resume ID`",
    "postprocess": "a step after the download failed: `dlm logs --level warn --q TASK_ID`",
    "infected": "the virus scan flagged the file: tell the user, do not retry",
    "fatal": "not retryable as it is: read error.message",
}


def run(cli: list[str], *args: str) -> Any:
    """Run a dlm command and return its JSON output, or None if it failed."""
    proc = subprocess.run([*cli, *args, "-o", "json"], capture_output=True, text=True, check=False)
    if proc.returncode != 0:
        return None
    try:
        return json.loads(proc.stdout)
    except ValueError:
        return None


def check_tools(info: dict[str, Any], add) -> None:
    """Report external tools the daemon has and cannot get."""
    for tool in info.get("tools", []):
        if not tool.get("available") and not tool.get("autoDownload"):
            add("warn", f"{tool['name']} is missing and auto-download is off; streams or page extraction will fail")


def check_disk(info: dict[str, Any], add) -> None:
    """Report little free space on a daemon on this machine."""
    path = info.get("paths", {}).get("downloads")
    if not (info.get("local") and path and os.path.isdir(path)):
        return
    free = shutil.disk_usage(path).free
    if free < LOW_DISK_BYTES:
        add("warn", f"only {free >> 20} MiB free in {path}", "`dlm task cleanup --older-than 30 --dry-run`")


def check_queues(queues: list[dict[str, Any]], tasks: list[dict[str, Any]], add) -> None:
    """Report queues that hold work they will not run."""
    waiting = Counter(t["queueId"] for t in tasks if t["status"] == "queued")
    for queue in queues:
        count = waiting[queue["id"]]
        if queue.get("state") == "stopped" and count:
            add("problem", f"queue {queue['name']} is stopped with {count} queued task(s)", f"`dlm queue start {queue['id']}`")


def check_limits(limits: dict[str, Any], queues: list[dict[str, Any]], tasks: list[dict[str, Any]], add) -> None:
    """Report overrides and exhausted quotas."""
    if limits.get("override"):
        speed = limits["override"].get("speedLimit")
        add("info", f"a temporary speed override ({speed} B/s, 0 = unlimited) ends {limits['override'].get('until')}", "`dlm limits override --clear`")
    scopes = [("global", limits.get("global", {}))] + [(q.get("name"), q) for q in limits.get("queues", [])]
    for name, scope in scopes:
        quota = scope.get("quota") or {}
        if quota.get("exhausted"):
            add("problem", f"quota of {name} is used up; its downloads wait until {quota.get('resetsAt', 'the next period')}", "`dlm limits`")
    if any(t["status"] == "queued" for t in tasks) and not any(t["status"] == "running" for t in tasks):
        if any(q.get("settings", {}).get("schedule") for q in queues):
            add("info", "tasks are queued, nothing runs, and a queue has a schedule: it may be outside its time window", "`dlm limits`, `dlm queue settings ID`")


def check_tasks(cli: list[str], tasks: list[dict[str, Any]], add) -> None:
    """Report failed and waiting tasks with the reason and the fix."""
    for task in tasks:
        label = task.get("name") or task.get("source", {}).get("currentUrl")
        if task["status"] == "error":
            error = task.get("error") or {}
            code = error.get("code", "unknown")
            add("problem", f"{label} ({task['id']}) failed: {code}: {error.get('message', '')[:200]}", ERROR_FIXES.get(code, "read error.message"))
        elif task["status"] == "awaiting_link":
            history = run(cli, "task", "refresh-history", task["id"]) or []
            failed = [h for h in history if h.get("outcome") == "failed"]
            why = f": {failed[-1].get('error', '')[:160]}" if failed else ""
            add("problem", f"{label} ({task['id']}) waits for a new link{why}", f"`dlm task set-url {task['id']} NEW_URL`, or delete it if the file is gone")
        elif task["status"] == "refreshing":
            add("info", f"{label} ({task['id']}) is getting a new link")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--json", action="store_true")
    parser.add_argument("--cli", help="dlm command prefix (default: found with dlm_connect)")
    args = parser.parse_args()

    if args.cli:
        cli = shlex.split(args.cli)
    else:
        daemons = dlm_connect.find()
        if not daemons:
            print("no running dlm daemon found; run dlm_connect.py --start", file=sys.stderr)
            return 1
        cli = dlm_connect.prefix(os.environ.get("DLM_BIN", "dlm"), daemons[0])

    info = run(cli, "system", "info")
    if info is None:
        print(f"the daemon did not answer `{shlex.join(cli)} system info`", file=sys.stderr)
        return 1

    findings: list[dict[str, str]] = []

    def add(level: str, what: str, fix: str = "") -> None:
        findings.append({"level": level, "what": what, "fix": fix})

    queues = (run(cli, "queue", "list") or [])
    if isinstance(queues, dict):
        queues = queues.get("queues", [])
    listed = run(cli, "task", "list", "--all", "--status", "queued,running,paused,awaiting_link,refreshing,postprocessing,error")
    tasks = (listed or {}).get("tasks", [])
    check_tools(info, add)
    check_disk(info, add)
    check_queues(queues, tasks, add)
    check_limits(run(cli, "limits") or {}, queues, tasks, add)
    check_tasks(cli, tasks, add)

    summary = dict(Counter(t["status"] for t in tasks))
    if args.json:
        print(json.dumps({"version": info.get("version"), "unfinished": summary, "findings": findings}, indent=2))
    else:
        print(f"dlm {info.get('version')} at {info.get('address')}; unfinished tasks: {summary or 'none'}")
        for finding in findings:
            print(f"[{finding['level']}] {finding['what']}")
            if finding["fix"]:
                print(f"    fix: {finding['fix']}")
        if not findings:
            print("nothing wrong found")
    return 3 if any(f["level"] == "problem" for f in findings) else 0


if __name__ == "__main__":
    sys.exit(main())
