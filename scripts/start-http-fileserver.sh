#!/usr/bin/env bash
# Start HTTP file server for S8.1 remote input testing.
# Serves test media files over plain HTTP so the worker can exercise
# the HTTP remote-input download path (pkg/inputsource SchemeHTTP).
#
# Usage: ./scripts/start-http-fileserver.sh [PORT] [ROOT_DIR]
#   PORT     listen port          (default: 18080)
#   ROOT_DIR directory to serve  (default: test_data)
#
# Requirements: python3 or python on PATH (built-in http.server module).
set -euo pipefail

PORT="${1:-18080}"
ROOT_DIR="${2:-test_data}"

if [ ! -d "$ROOT_DIR" ]; then
    echo "rffmpeg http-fileserver: creating $ROOT_DIR"
    mkdir -p "$ROOT_DIR"
fi

echo "rffmpeg http-fileserver: serving $ROOT_DIR on http://127.0.0.1:$PORT"

if command -v python3 >/dev/null 2>&1; then
    cd "$ROOT_DIR"
    exec python3 -m http.server "$PORT" --bind 127.0.0.1
fi

if command -v python >/dev/null 2>&1; then
    cd "$ROOT_DIR"
    exec python -m SimpleHTTPServer "$PORT"
fi

echo "rffmpeg http-fileserver: no python or python3 found on PATH" >&2
exit 1
