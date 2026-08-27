#!/usr/bin/env bash
# Start RTMP stream receiver for S8.2 streaming output testing.
# ffmpeg pushes its output to rtmp://127.0.0.1/live/<stream> and this
# script consumes the stream so the worker's network-output path
# (pkg/worker executor networkOutput detection) can be exercised
# end-to-end.
#
# Usage: ./scripts/start-rtmp-server.sh [PORT] [APP] [STREAM_KEY]
#   PORT       listen port     (default: 1935)
#   APP        RTMP app path   (default: live)
#   STREAM_KEY stream key      (default: test)
#
# Requirements: ffmpeg on PATH (built with the rtmp protocol).
set -euo pipefail

PORT="${1:-1935}"
APP="${2:-live}"
STREAM_KEY="${3:-test}"

if ! command -v ffmpeg >/dev/null 2>&1; then
    echo "rffmpeg rtmp-server: ffmpeg not found on PATH" >&2
    exit 1
fi

RTMP_URL="rtmp://127.0.0.1:$PORT/$APP/$STREAM_KEY"

echo "rffmpeg rtmp-server: listening on $RTMP_URL"

# Standalone RTMP listener: ffmpeg -f flv listens for inbound RTMP
# and discards the received stream (a null sink). Run with -loglevel
# info to show connection events on stderr.
exec ffmpeg -hide_banner -loglevel info \
    -f flv -listen 1 -i "$RTMP_URL" \
    -f null -
