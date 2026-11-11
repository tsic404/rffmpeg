#!/usr/bin/env bash
# TC-error-11: 位置参数作输出（AC-51）
# 判据：`-i` 单输出成功并产出输出；无 `-i` 与多余输出候选在提交前被拒——
# `-q` 下 stderr 恰一行、无作业终态行、作业列表无新增、无输出文件残留。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

job_count() {
	api_get /api/v1/jobs | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["jobs"]))'
}

MEDIA="$(make_media positional.mp4 2)"
OK_OUT="$TMP_ROOT/ok.mp4"

# 成功面：位置参数就是输出，-i 给出唯一输入。
set +e
RFFMPEG_QUIET=1 "$RFFMPEG_BIN/rffmpeg" \
	-i "$MEDIA" "$OK_OUT" >/dev/null 2>"$TMP_ROOT/cli-ok.log"
RC=$?
set -e
expect_rc "$RC" 0 "-i single output rc"
[[ -s "$OK_OUT" ]] || fail "successful run produced no output file"
ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$OK_OUT" >/dev/null 2>&1 ||
	fail "output is not a readable media file"

# 拒绝面：每个用例断言同一组外部可观察结果。
# reject <label> <expected text> <args...>
reject() {
	local label="$1" want="$2" log before after rc
	shift 2
	log="$TMP_ROOT/cli-reject-$label.log"
	before="$(job_count)"
	set +e
	RFFMPEG_QUIET=1 "$RFFMPEG_BIN/rffmpeg" \
		"$@" >/dev/null 2>"$log"
	rc=$?
	set -e
	expect_rc "$rc" 1 "reject rc ($label)"
	[[ "$(wc -l < "$log")" -eq 1 ]] ||
		fail "reject ($label): stderr is not exactly one line: $(cat "$log")"
	expect_in_file "$log" "$want"
	grep -qE '^Job (failed|timed out|completed)|^Output saved' "$log" &&
		fail "reject ($label): terminal line in stderr: $(cat "$log")"
	expect_no_panic "$log"
	after="$(job_count)"
	[[ "$before" == "$after" ]] || fail "reject ($label): job list gained a row ($before -> $after)"
}

NO_I_OUT="$TMP_ROOT/no-i-out.mp4"
reject no-i "no input file specified" "$MEDIA" "$NO_I_OUT"
expect_in_file "$TMP_ROOT/cli-reject-no-i.log" "-i"
[[ ! -e "$NO_I_OUT" ]] || fail "no-input rejection left an output file"

# 缺输入与多输出并存时按缺输入报：位置参数全是输出候选，第一个就不成立。
PRE_A="$TMP_ROOT/pre-a.mp4"
PRE_B="$TMP_ROOT/pre-b.mp4"
reject precedence "no input file specified" "$MEDIA" "$PRE_A" "$PRE_B"
expect_in_file "$TMP_ROOT/cli-reject-precedence.log" "-i"
! grep -q 'multiple output files' "$TMP_ROOT/cli-reject-precedence.log" ||
	fail "missing input plus extra outputs reported the extra output instead of the missing -i"
[[ ! -e "$PRE_A" && ! -e "$PRE_B" ]] || fail "precedence rejection left an output file"

MULTI_A="$TMP_ROOT/multi-a.mp4"
MULTI_B="$TMP_ROOT/multi-b.mp4"
reject multi-output "multiple output files are not supported" -i "$MEDIA" "$MULTI_A" "$MULTI_B"
[[ ! -e "$MULTI_A" && ! -e "$MULTI_B" ]] || fail "multi-output rejection left an output file"

echo "PASS: -i single output rc=0 with output; no -i and extra outputs rejected with one line, no job, no file"
