#!/bin/sh
# Stable wrapper around `intentline why`.
# A missing install or a line with no record exits 0 and says so.
# Any other intentline failure is passed through.
set -eu
if ! command -v intentline >/dev/null 2>&1; then
    echo "No record: intentline is not on PATH."
    exit 0
fi
exec intentline why "$@"
