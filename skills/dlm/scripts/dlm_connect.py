#!/usr/bin/env python3
"""Find the running dlm daemon and print the command prefix that reaches it.

Plain `dlm ...` only reaches a daemon in the default config directory. The
desktop app runs its own daemon with --home on a random port, so `dlm` alone can
report "not running" while downloads are in fact running. This script checks
every place a daemon can live and says which one to use.

    dlm_connect.py                 prints the prefix, e.g.  dlm --home '/path'
    dlm_connect.py --json          everything found, as JSON
    dlm_connect.py --start         starts a daemon if none is running

Exit status: 0 found, 1 no daemon found, 2 the dlm binary is missing.
Standard library only. It never reads or prints the API token.
"""

from __future__ import annotations

import argparse
import json
import os
import platform
import re
import shlex
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any


def user_config_dir() -> Path:
    """Return the directory Go's os.UserConfigDir gives on this system."""
    system = platform.system()
    if system == "Windows":
        return Path(os.environ.get("APPDATA", Path.home() / "AppData" / "Roaming"))
    if system == "Darwin":
        return Path.home() / "Library" / "Application Support"
    return Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config"))


def app_homes() -> list[Path]:
    """Return the --home directories the desktop app may use."""
    homes = [user_config_dir() / "dlm" / "daemon"]
    if platform.system() == "Windows":
        local = os.environ.get("LOCALAPPDATA")
        if local:
            homes.append(Path(local) / "dlm" / "daemon")
    elif platform.system() == "Linux":
        data = os.environ.get("XDG_DATA_HOME", str(Path.home() / ".local" / "share"))
        homes.append(Path(data) / "dlm" / "daemon")
    return homes


def homes_from_processes() -> list[Path]:
    """Return the --home of every `dlm serve` process (not on Windows)."""
    if platform.system() == "Windows":
        return []
    try:
        out = subprocess.run(
            ["ps", "-axww", "-o", "command="],
            capture_output=True,
            text=True,
            timeout=5,
            check=False,
        ).stdout
    except (OSError, subprocess.SubprocessError):
        return []
    homes = []
    for line in out.splitlines():
        if not re.search(r"\bdlm(\.exe)?\s+serve\b", line):
            continue
        match = re.search(r"--home[= ](.+?)(?:\s+--\S+|$)", line)
        if match:
            homes.append(Path(match.group(1).strip().strip("'\"")))
    return homes


def health(url: str) -> dict[str, Any] | None:
    """Return the public health answer of a daemon, or None if it is not there."""
    try:
        with urllib.request.urlopen(url.rstrip("/") + "/v1/health", timeout=2) as resp:
            return json.loads(resp.read().decode())
    except (OSError, ValueError, urllib.error.URLError):
        return None


def probe(config_dir: Path, home: Path | None, source: str) -> dict[str, Any] | None:
    """Return a description of the daemon of config_dir, if one answers."""
    try:
        info = json.loads((config_dir / "runtime.json").read_text())
    except (OSError, ValueError):
        return None
    url = info.get("url")
    if not url or health(url) is None:
        return None  # a crash can leave runtime.json behind
    return {
        "source": source,
        "home": str(home) if home else None,
        "url": url,
        "version": info.get("version"),
        "pid": info.get("pid"),
        "startedAt": info.get("startedAt"),
    }


def find() -> list[dict[str, Any]]:
    """Return every reachable daemon, the one to prefer first."""
    candidates: list[tuple[Path, Path | None, str]] = []
    env_home = os.environ.get("DLM_HOME")
    if env_home:
        candidates.append((Path(env_home) / "config", Path(env_home), "DLM_HOME"))
    candidates.append((user_config_dir() / "dlm", None, "default"))
    candidates += [(h / "config", h, "desktop-app") for h in app_homes()]
    candidates += [(h / "config", h, "process") for h in homes_from_processes()]

    found: list[dict[str, Any]] = []
    seen: set[str] = set()
    for config_dir, home, source in candidates:
        daemon = probe(config_dir, home, source)
        if daemon and daemon["url"] not in seen:
            seen.add(daemon["url"])
            found.append(daemon)
    return found


def prefix(dlm: str, daemon: dict[str, Any]) -> list[str]:
    """Return the argv prefix that reaches daemon."""
    return [dlm, "--home", daemon["home"]] if daemon["home"] else [dlm]


def start(dlm: str) -> None:
    """Start a detached daemon in the default location."""
    kwargs: dict[str, Any] = {}
    if platform.system() == "Windows":
        kwargs["creationflags"] = 0x00000008 | 0x00000200  # DETACHED, NEW_GROUP
    else:
        kwargs["start_new_session"] = True
    subprocess.Popen(  # noqa: S603 - the fixed `dlm serve` command
        [dlm, "serve"],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        **kwargs,
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--json", action="store_true", help="print everything found as JSON")
    parser.add_argument("--start", action="store_true", help="start a daemon if none is running")
    parser.add_argument("--dlm", default=os.environ.get("DLM_BIN", "dlm"), help="the dlm binary (or DLM_BIN)")
    args = parser.parse_args()

    dlm = shutil.which(args.dlm)
    if not dlm:
        print(f"error: {args.dlm!r} is not installed; see 'Installing' in the dlm-admin skill", file=sys.stderr)
        return 2

    daemons = find()
    started = False
    if not daemons and args.start:
        start(dlm)
        started = True
        for _ in range(50):
            time.sleep(0.2)
            daemons = find()
            if daemons:
                break

    best = daemons[0] if daemons else None
    if args.json:
        print(json.dumps({
            "found": best is not None,
            "started": started,
            "prefix": prefix(args.dlm, best) if best else None,
            "daemon": best,
            "others": daemons[1:],
        }, indent=2))
    elif best:
        print(shlex.join(prefix(args.dlm, best)))
        if len(daemons) > 1:
            print(f"note: {len(daemons)} daemons are running; run with --json and ask which one is meant", file=sys.stderr)
    else:
        print("no running dlm daemon found; start one with --start, or `dlm service install`", file=sys.stderr)
    return 0 if best else 1


if __name__ == "__main__":
    sys.exit(main())
