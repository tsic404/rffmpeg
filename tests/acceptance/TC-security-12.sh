#!/usr/bin/env bash
# TC-security-12: 参数的可达资源边界（AC-58）
#
# 可观察判据（docs/03-acceptance.md AC-58）:
#   - 引入清单之外的输入项、合成输入或滤镜文件源 → 提交阶段可判者 400
#     invalid_request，作业不创建。
#   - 参数要求在工作区之外写入 → 除本作业的直通目标外，工作区外无新建 / 覆盖 / 删除。
#   - 参数指向清单之外的本机路径 → 该路径的内容不出现在任何产物中。
#   - 执行侧在启动 ffmpeg 之前可判的违反 → 作业失败 INFRA，不执行 ffmpeg，
#     不产生产物；该分支无法在提交侧构造（提交侧先判），由实现侧断言覆盖
#     （pkg/worker 参数边界用例）。

set -euo pipefail
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries

MEDIA="$(make_media args.mp4 1)"
ROOT="$MEDIA_DIR/args-space"
mkdir -p "$ROOT"
cp "$MEDIA" "$ROOT/input.mp4"
SECRET="$TMP_ROOT/secret.txt"
printf 'do-not-leak\n' >"$SECRET"
GUARD="$TMP_ROOT/guard"
mkdir -p "$GUARD"
printf 'guard\n' >"$GUARD/keep.txt"

SERVER_EXTRA_FLAGS="--shared-fs-roots $ROOT" start_server
wait_server_api
start_worker "acceptance-worker-$PORT" RFFMPEG_SHARED_FS_ROOTS="$ROOT"
wait_ready

job_count() {
	api_get /api/v1/jobs | python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("jobs") or []))'
}

# submit posts a raw job body and prints the HTTP status.
submit() {
	local body="$1"
	curl -sS -o "$TMP_ROOT/submit.json" -w '%{http_code}' \
		-H "Authorization: Bearer $AUTH_TOKEN" -H 'Content-Type: application/json' \
		-d "$body" "$(acceptance_server_url)/api/v1/jobs"
}

expect_rejected() {
	local label="$1" body="$2" before after code
	before="$(job_count)"
	code="$(submit "$body")"
	[[ "$code" == "400" ]] || fail "$label: status $code, want 400 (body: $(cat "$TMP_ROOT/submit.json"))"
	python3 -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1])).get("code") == "invalid_request" else 1)' \
		"$TMP_ROOT/submit.json" || fail "$label: code is not invalid_request (body: $(cat "$TMP_ROOT/submit.json"))"
	after="$(job_count)"
	[[ "$after" == "$before" ]] || fail "$label: job list grew $before -> $after"
}

# snapshot prints "<relative path> <sha256>" for the guarded region.
snapshot() {
	python3 - "$1" <<'PY'
import hashlib, os, sys
root = sys.argv[1]
for dirpath, dirnames, filenames in os.walk(root):
    dirnames.sort()
    for name in sorted(filenames):
        path = os.path.join(dirpath, name)
        with open(path, "rb") as f:
            print(os.path.relpath(path, root), hashlib.sha256(f.read()).hexdigest())
PY
}

BEFORE="$(snapshot "$GUARD")"
INPUT_ID="http://files.example.test/in.mp4"

# 清单之外的额外输入项：第二个 -i 不是占位替换。
expect_rejected "second input outside the manifest" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-i\",\"/etc/passwd\",\"-c:v\",\"libx264\"],\"output_filename\":\"out.mp4\"}"
# 合成输入：lavfi 生成的源不在清单内。
expect_rejected "synthetic (lavfi) input" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-f\",\"lavfi\",\"-i\",\"testsrc=duration=1\",\"-c:v\",\"libx264\"],\"output_filename\":\"out.mp4\"}"
# 滤镜文件源：movie / subtitles 读取了清单之外的路径。
expect_rejected "filter file source" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-vf\",\"movie=$SECRET\",\"-c:v\",\"libx264\"],\"output_filename\":\"out.mp4\"}"
# 同一个文件源跟在链路标签后面：元素边界由 filtergraph 解析给出，不是由字符位置给出。
expect_rejected "filter file source behind a label prefix" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-filter_complex\",\"[0:v]subtitles=$SECRET[v]\"],\"output_filename\":\"out.mp4\"}"
# 滤镜选项位置的文件参数：drawtext 的 textfile / fontfile。
expect_rejected "drawtext textfile parameter" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-vf\",\"drawtext=textfile=$SECRET:fontsize=12\"],\"output_filename\":\"out.mp4\"}"
expect_rejected "drawtext fontfile parameter behind a label" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-filter_complex\",\"[in]drawtext=fontfile=$SECRET:fontsize=12[out]\"],\"output_filename\":\"out.mp4\"}"
# 滤镜实例名不改变被查的滤镜：movie@x 仍按 movie 拒。
expect_rejected "file filter behind an instance name" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-vf\",\"movie@x=$SECRET\"],\"output_filename\":\"out.mp4\"}"
# JSON-encode an argv list so a case can carry ffmpeg backslash escapes
# without the JSON layer eating them.
json_args() {
	python3 -c 'import json, sys; print(json.dumps(sys.argv[1:]))' "$@"
}

# 滤镜名 token 是反转义后再查表：subtit\les 就是 subtitles。
expect_rejected "escaped filter name resolves to a file filter" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":$(json_args -i '<INPUT_FILE>' -vf 'subtit\les=/etc/hostname'),\"output_filename\":\"out.mp4\"}"
# 同一个滤镜文件参数用 ffmpeg 的反斜杠转义写法：ffmpeg 先还原再读参数，
# 判定必须落在还原后的文本上。
# 目标路径的转义写法：把 run 目录的 `/` 写成 ffmpeg 的反斜杠转义 `\/`。
OUTSIDE="$TMP_ROOT/outside"
OUTSIDE_ESC="${OUTSIDE//\//\\/}"
expect_rejected "escaped drawtext textfile parameter" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":$(json_args -i '<INPUT_FILE>' -vf "drawtext=textfile\\=${OUTSIDE_ESC}/NOPE.txt:fontsize=12"),\"output_filename\":\"out.mp4\"}"
expect_rejected "escaped drawtext fontfile behind a label" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":$(json_args -i '<INPUT_FILE>' -filter_complex "[in]drawtext=fontfile\\=${OUTSIDE_ESC}/NOPE.ttf:fontsize=12[out]"),\"output_filename\":\"out.mp4\"}"
# 转义的分隔符把参数名推到下一层：ffmpeg 先把 "\:" 还原成 ":" 再切分，
# 所以 textfile 是一个独立参数，不是前一个参数取值的一部分。
expect_rejected "escaped separator hides a file parameter" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":$(json_args -i '<INPUT_FILE>' -vf "drawtext=fontsize=20\\:textfile=$OUTSIDE/NOPE.txt"),\"output_filename\":\"out.mp4\"}"
expect_rejected "escaped separator hides a filter write parameter" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":$(json_args -i '<INPUT_FILE>' -vf "vidstabdetect=shakiness=5\\:result=$OUTSIDE/trf.txt"),\"output_filename\":\"out.mp4\"}"
# 位置参数是滤镜的第一个选项；对这些滤镜而言它就是文件路径。
expect_rejected "positional filter read path" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":$(json_args -i '<INPUT_FILE>' -vf "vidstabtransform=$SECRET"),\"output_filename\":\"out.mp4\"}"
expect_rejected "positional filter write path" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":$(json_args -i '<INPUT_FILE>' -vf "vidstabdetect=$OUTSIDE/pos.txt"),\"output_filename\":\"out.mp4\"}"
expect_rejected "positional filter upward path" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":$(json_args -i '<INPUT_FILE>' -vf "vidstabtransform=../../outside/secret.txt"),\"output_filename\":\"out.mp4\"}"
expect_rejected "positional filter protocol address" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":$(json_args -i '<INPUT_FILE>' -vf "vidstabtransform=http://169.254.169.254/latest"),\"output_filename\":\"out.mp4\"}"
# tee 的选项组里，转义的分隔符同样在还原之后才切分。
expect_rejected "escaped tee option-group separator" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":$(json_args -i '<INPUT_FILE>' -c copy -f tee "[f=segment:segment_time=1\\:segment_list=$OUTSIDE/TEE.csv]seg%03d.mp4"),\"output_filename\":\"out.mp4\"}"
# tee 的选项组：组内的文件取值与从属路径同样要逐条判定（含转义写法）。
expect_rejected "tee option group with segment_list" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":$(json_args -i '<INPUT_FILE>' -c copy -f tee "[f=segment:segment_list=$OUTSIDE/EVIL.csv]seg%03d.mp4"),\"output_filename\":\"out.mp4\"}"
expect_rejected "escaped tee option group file value" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":$(json_args -i '<INPUT_FILE>' -c copy -f tee "[f=segment:segment_list=${OUTSIDE_ESC}/EVIL.csv]seg%03d.mp4"),\"output_filename\":\"out.mp4\"}"
expect_rejected "escaped tee slave path" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":$(json_args -i '<INPUT_FILE>' -c copy -f tee "[f=mpegts]${OUTSIDE_ESC}/esc.ts"),\"output_filename\":\"out.mp4\"}"
# 滤镜选项取值经 ffmpeg 两层还原：图解析一层、滤镜自己的选项解析再一层。
# 只还原一层时 `\/` 看似相对路径放行，ffmpeg 还原后是绝对路径。
DOUBLE_ESC="${OUTSIDE_ESC//\\/\\\\}"
expect_rejected "double-escaped filter write path" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":$(json_args -i '<INPUT_FILE>' -vf "vidstabdetect=result=$DOUBLE_ESC/trf.txt"),\"output_filename\":\"out.mp4\"}"
expect_rejected "double-escaped filter read path" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":$(json_args -i '<INPUT_FILE>' -vf "vidstabtransform=input=$DOUBLE_ESC/secret.txt"),\"output_filename\":\"out.mp4\"}"
# 相对参数的上溯段：cwd 是工作区，`../` 正好跑出工作区。
expect_rejected "relative output climbing out of the workspace" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c\",\"copy\",\"../outside/PWN.mp4\"],\"output_filename\":\"out.mp4\"}"
expect_rejected "segment list climbing out of the workspace" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c\",\"copy\",\"-f\",\"segment\",\"-segment_list\",\"../leak.csv\",\"seg%03d.mp4\"],\"output_filename\":\"out.mp4\"}"
# muxer 选项的文件取值与复合写法。
expect_rejected "tee slave with an absolute path" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c\",\"copy\",\"-f\",\"tee\",\"[f=mp4]$TMP_ROOT/tee-slave.mp4\"],\"output_filename\":\"out.mp4\"}"
expect_rejected "hls segment filename outside the workspace" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c\",\"copy\",\"-hls_segment_filename\",\"../out/x%03d.ts\",\"-f\",\"hls\",\"p.m3u8\"],\"output_filename\":\"out.mp4\"}"
# 清单内文件被当作路径清单再解释：concat 会打开文件里列出的每个路径。
expect_rejected "concat demuxer over a manifest input" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-f\",\"concat\",\"-safe\",\"0\",\"-i\",\"<INPUT_FILE>\",\"-c\",\"copy\"],\"output_filename\":\"out.mp4\"}"
# 占位符出现在输出位置：BuildArgs 的顺序替换会把它映射成清单输入路径。
expect_rejected "bare placeholder in the output position" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c\",\"copy\",\"-y\",\"<INPUT_FILE>\"],\"output_filename\":\"out.mp4\"}"
# 工作区之外的写入点：统计与 passlog 落点。
expect_rejected "stats written outside the workspace" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\",\"-stats_period\",\"1\",\"-progress\",\"$TMP_ROOT/progress.txt\"],\"output_filename\":\"out.mp4\"}"
expect_rejected "two-pass log written outside the workspace" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\",\"-passlogfile\",\"$TMP_ROOT/passlog\"],\"output_filename\":\"out.mp4\"}"
# 清单占位之外的本机路径作为位置参数。
expect_rejected "bare host path as an output argument" \
	"{\"input_files\":[\"$INPUT_ID\"],\"args\":[\"-i\",\"<INPUT_FILE>\",\"-c:v\",\"libx264\",\"$TMP_ROOT/bare.mp4\"],\"output_filename\":\"out.mp4\"}"

# 以上全部在提交阶段被拒：作业列表不变，被引用路径无新建 / 覆盖 / 删除。
AFTER="$(snapshot "$GUARD")"
[[ "$BEFORE" == "$AFTER" ]] || fail "rejected submissions changed the guarded region"
[[ ! -e "$TMP_ROOT/progress.txt" && ! -e "$TMP_ROOT/bare.mp4" ]] ||
	fail "rejected argument paths were written"
if compgen -G "$TMP_ROOT/passlog*" >/dev/null; then
	fail "rejected passlog path was written"
fi
[[ ! -e "$TMP_ROOT/tee-slave.mp4" ]] || fail "a rejected tee slave was written"
[[ ! -e "$OUTSIDE" ]] || fail "a rejected escaped parameter wrote $OUTSIDE"
[[ ! -e "$TMP_ROOT/outside" && ! -e "$TMP_ROOT/../outside" ]] || fail "a rejected relative output escaped the workspace"
if compgen -G "$TMP_ROOT/leak.csv" >/dev/null; then
	fail "a rejected segment list was written"
fi
expect_in_file "$SECRET" "do-not-leak"

# 合法直通目标仍是唯一的工作区外写入点：产物落在目标本身。
TARGET="$ROOT/out-target.mp4"
LOG="$TMP_ROOT/cli.log"
set +e
env RFFMPEG_SHARED_FS=1 "$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$ROOT/input.mp4" -c:v libx264 "$TARGET" 2>"$LOG"
RC=$?
set -e
[[ $RC -eq 0 ]] || fail "legal pass-through run failed (rc=$RC); see $LOG"
[[ -s "$TARGET" ]] || fail "pass-through product missing at $TARGET"
expect_in_file "$LOG" "Shared FS: using direct path"
# 工作区内 staging：执行期间目标目录里除目标本身外不出现第二条路径，
# 因此没有任何 .rffmpeg-* 目录残留在共享目标旁。
if compgen -G "$ROOT/.rffmpeg-*" >/dev/null; then
	fail "pass-through run left a staging path beside the target: $(ls -d "$ROOT"/.rffmpeg-* | head -1)"
fi

echo "PASS: manifest-external inputs, filter file sources and outside writes are 400 with no job row and no write; the pass-through target stays the only outside write"
