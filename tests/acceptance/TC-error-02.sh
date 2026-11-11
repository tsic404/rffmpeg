#!/usr/bin/env bash
# TC-error-02: 参数不完整（编码器选项缺值）（AC-12）
# 判据：ffmpeg 风格的错误提示；无 panic；不卡死。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media args.mp4 2)"
LOG="$TMP_ROOT/cli.log"

START=$(now_ns)
set +e
"$RFFMPEG_BIN/rffmpeg" \
	-i "$MEDIA" -c:v >/dev/null 2>"$LOG"
RC=$?
set -e
WALL_MS=$(( ($(now_ns) - START) / 1000000 ))

expect_rc "$RC" 1 "incomplete-args rc"
expect_in_file "$LOG" "requires a value"
expect_in_file "$LOG" "-c:v"
expect_no_panic "$LOG"
(( WALL_MS < 30000 )) || fail "CLI hung for ${WALL_MS}ms"

# A second output used to be dropped silently with exit 0: the user got a
# success code and only one file. It must be a single-line, non-zero error
# under a quiet configuration.
check_multiple_outputs() {
	local label="$1" log
	shift
	log="$TMP_ROOT/cli-multi-$label.log"
	set +e
	RFFMPEG_QUIET=1 "$RFFMPEG_BIN/rffmpeg" \
		"$@" >/dev/null 2>"$log"
	RC=$?
	set -e
	expect_rc "$RC" 1 "multiple-outputs rc ($label)"
	expect_in_file "$log" "multiple output files are not supported"
	expect_no_panic "$log"
	[[ "$(wc -l <"$log")" -eq 1 ]] ||
		fail "multiple-outputs error is not a single line ($label): $(cat "$log")"
}
check_multiple_outputs positional -i "$MEDIA" "$TMP_ROOT/out1.mp4" "$TMP_ROOT/out2.mp4"
# A positional before -i is ffmpeg's output syntax, not an implicit input:
# both positionals name outputs, so the extra one must be rejected.
check_multiple_outputs positional-before-input "$TMP_ROOT/out1.mp4" -i "$MEDIA" "$TMP_ROOT/out2.mp4"
[[ ! -e "$TMP_ROOT/out1.mp4" && ! -e "$TMP_ROOT/out2.mp4" ]] ||
	fail "multiple-outputs run produced an output file"

# `-timeout` is ffmpeg's own option (protocol scope), not rffmpeg's job
# budget: the CLI must pass it through. Against an unreachable server the run
# stops at the health check, which proves parsing accepted the option instead
# of rejecting it as an invalid duration. The job budget lives in the
# configuration plane, so a malformed value there must fail before submission.
TIMEOUT_LOG="$TMP_ROOT/cli-timeout.log"
set +e
RFFMPEG_QUIET=1 RFFMPEG_SERVER_URL=http://127.0.0.1:1 "$RFFMPEG_BIN/rffmpeg" \
	-timeout 5000000 -i "rtsp://example/stream" "$TMP_ROOT/timeout-out.mp4" \
	>/dev/null 2>"$TIMEOUT_LOG"
RC=$?
set -e
expect_rc "$RC" 1 "single-dash -timeout rc"
expect_in_file "$TIMEOUT_LOG" "server health check failed"
grep -q 'invalid timeout value' "$TIMEOUT_LOG" &&
	fail "single-dash -timeout was captured by rffmpeg: $(cat "$TIMEOUT_LOG")"

BUDGET_LOG="$TMP_ROOT/cli-budget.log"
set +e
RFFMPEG_QUIET=1 RFFMPEG_JOB_TIMEOUT=not-a-duration "$RFFMPEG_BIN/rffmpeg" \
	-i "$MEDIA" "$TMP_ROOT/budget-out.mp4" >/dev/null 2>"$BUDGET_LOG"
RC=$?
set -e
expect_rc "$RC" 1 "malformed job budget rc"
expect_in_file "$BUDGET_LOG" "must be a positive duration"

echo "PASS: rc=1, ffmpeg-style error, no panic, wall=${WALL_MS}ms, multiple outputs rejected, -timeout passed through"
