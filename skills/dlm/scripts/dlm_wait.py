#!/usr/bin/env python3
"""Wait until dlm tasks settle and report what happened to each.

The dlm CLI has no blocking wait, and a task that needs help does not always end
in `error`: a dead link sits in `awaiting_link`. This script stops as soon as
every task is finished or cannot get further without someone acting.

    dlm_wait.py ID [ID...] [--timeout 600] [--interval 2] [--stall 120]
    dlm_wait.py --queue NAME_OR_ID
    dlm_wait.py --all

Prints a JSON report: outcome and next step per task.
Exit status: 0 all completed (or seeding), 1 could not talk to the daemon,
3 some task needs attention, 4 timed out with tasks still moving.
Standard library only.
"""

from __future__ import annotations

import argparse
import json
import os
import shlex
import subprocess
import sys
import time
from typing import Any
from urllib.parse import urlsplit

sys.dont_write_bytecode = True  # leave no __pycache__ in the skill
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dlm_connect  # noqa: E402

NEXT_STEPS = {
    "failed": "see error.code in the dlm skill's status table; `dlm task resume ID` retries it",
    "awaiting_link": "`dlm task refresh-history ID` shows why; give a new link with `dlm task set-url ID URL`",
    "paused": "`dlm task resume ID`",
    "stalled": "no progress: run dlm_doctor.py (stopped queue, schedule, quota, full slots or a dead server)",
}


class DaemonError(Exception):
    """The CLI failed for a reason other than one task."""


def run(cli: list[str], *args: str) -> Any:
    """Run a dlm command and return its JSON output."""
    proc = subprocess.run([*cli, *args, "-o", "json"], capture_output=True, text=True, check=False)
    if proc.returncode != 0:
        raise DaemonError(proc.stderr.strip() or f"dlm {' '.join(args)} failed")
    return json.loads(proc.stdout)


def tasks_of(cli: list[str], ids: list[str], queue: str | None, every: bool) -> list[dict[str, Any]]:
    """Return the current state of the tasks being watched."""
    if ids:
        return [run(cli, "task", "get", i) for i in ids]
    args = ["task", "list", "--all"]
    if queue:
        args += ["--queue", queue]
    tasks = run(cli, *args)["tasks"]
    return tasks if (queue or every) else []


def seeding(cli: list[str], task: dict[str, Any]) -> bool:
    """Report whether a running torrent has finished and is only seeding."""
    if task.get("engine") != "bt" or task["status"] != "running":
        return False
    if not 0 < task.get("size", -1) <= task.get("downloaded", 0):
        return False
    try:
        return run(cli, "task", "torrent", task["id"]).get("state") == "seeding"
    except DaemonError:
        return False


def outcome(cli: list[str], task: dict[str, Any], stalled: bool) -> str | None:
    """Return why task is settled, or None while it is still moving."""
    status = task["status"]
    if status == "completed":
        return "completed"
    if status == "error":
        return "failed"
    if status in ("awaiting_link", "paused"):
        return status
    if seeding(cli, task):
        return "seeding"
    return "stalled" if stalled else None


def looks_like_web_page(task: dict[str, Any]) -> bool:
    """Report whether a completed task saved a page instead of a file.

    dlm treats a link that answers with HTML as a good download. A link that
    plainly asks for an HTML file is the one case where that is right.
    """
    source = task.get("source", {})
    content_type = source.get("fingerprint", {}).get("contentType", "")
    url_path = urlsplit(source.get("originalUrl") or source.get("currentUrl") or "").path.lower()
    return content_type.startswith("text/html") and not url_path.endswith((".html", ".htm"))


def describe(task: dict[str, Any], result: str | None) -> dict[str, Any]:
    """Return the report entry of a task."""
    entry = {
        "id": task["id"],
        "name": task.get("name") or task.get("source", {}).get("currentUrl"),
        "status": task["status"],
        "outcome": result or "still_running",
        "downloaded": task.get("downloaded"),
        "size": task.get("size"),
    }
    if result in ("completed", "seeding") and task.get("dir") and task.get("name"):
        entry["path"] = os.path.join(task["dir"], task["name"])
    if task.get("error"):
        entry["error"] = task["error"]
    if result == "completed" and looks_like_web_page(task):
        entry["warning"] = "the result looks like a web page, not the file: a page or login link, see dlm-sources"
    if result in NEXT_STEPS:
        entry["next"] = NEXT_STEPS[result]
    return entry


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("ids", nargs="*", help="full task IDs (`dlm task list -o json`)")
    parser.add_argument("--queue", help="watch every task of this queue")
    parser.add_argument("--all", action="store_true", help="watch every task")
    parser.add_argument("--timeout", type=float, default=600, help="give up after this many seconds")
    parser.add_argument("--interval", type=float, default=2, help="seconds between checks")
    parser.add_argument("--stall", type=float, default=120, help="seconds without progress that count as stalled")
    parser.add_argument("--cli", help="dlm command prefix (default: found with dlm_connect)")
    args = parser.parse_args()
    if not (args.ids or args.queue or args.all):
        parser.error("give task IDs, --queue or --all")

    if args.cli:
        cli = shlex.split(args.cli)
    else:
        daemons = dlm_connect.find()
        if not daemons:
            print("error: no running dlm daemon found", file=sys.stderr)
            return 1
        cli = dlm_connect.prefix(os.environ.get("DLM_BIN", "dlm"), daemons[0])

    deadline = time.monotonic() + args.timeout
    moved: dict[str, tuple[int, float]] = {}
    while True:
        try:
            tasks = tasks_of(cli, args.ids, args.queue, args.all)
        except (DaemonError, ValueError) as err:
            print(f"error: {err}", file=sys.stderr)
            return 1
        now = time.monotonic()
        report = []
        for task in tasks:
            seen = moved.get(task["id"])
            if seen is None or seen[0] != task.get("downloaded", 0):
                moved[task["id"]] = seen = (task.get("downloaded", 0), now)
            stalled = now - seen[1] >= args.stall
            report.append(describe(task, outcome(cli, task, stalled)))
        unsettled = [r for r in report if r["outcome"] == "still_running"]
        if not unsettled or now >= deadline:
            print(json.dumps({"timedOut": bool(unsettled), "tasks": report}, indent=2))
            if unsettled:
                return 4
            return 0 if all(r["outcome"] in ("completed", "seeding") for r in report) else 3
        time.sleep(args.interval)


if __name__ == "__main__":
    sys.exit(main())
