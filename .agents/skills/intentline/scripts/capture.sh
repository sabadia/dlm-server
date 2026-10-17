#!/bin/sh
# Stable wrapper around `intentline capture`. Forwards every argument.
# Exits non-zero with a clear message when intentline is not installed.
set -eu
if ! command -v intentline >/dev/null 2>&1; then
    echo "intentline is not on PATH. Install it with: uv tool install intentline" >&2
    exit 127
fi
exec intentline capture "$@"
