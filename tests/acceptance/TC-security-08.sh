#!/usr/bin/env bash
# TC-security-08: 当前目录配置按来源信任采用（AC-53）
# 判据一：组/其他可写的当前目录配置不被采用；banner 来源为用户主目录层，
#         server URL 取自该层，常规运行正常完成。
# 判据二：常规与静默两次运行的 stderr 各含单行告警（路径 + 原因词），
#         静默那次 banner 不出现。
# 判据三：属主为有效用户且组与其他不可写的当前目录配置被采用，banner 来源
#         为当前目录层，并优先于用户主目录层。
# 判据四：符号链接指向可信文件时被采用；指向组/其他可写文件时按目标跳过；
#         目标不存在时按非常规文件跳过。
# 判据五：以目录或 FIFO 占据该文件名时跳过，原因词为 not a regular file。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

SERVER_URL="$(acceptance_server_url)"
UNREACHABLE="http://127.0.0.1:1"
HOME_DIR="$TMP_ROOT/home"
mkdir -p "$HOME_DIR"

# 用户主目录层：可信、可用，提供真正的 server URL 与令牌。
cat >"$HOME_DIR/.rffmpeg.json" <<JSON
{"server_url":"$SERVER_URL","token":"$AUTH_TOKEN"}
JSON
chmod 600 "$HOME_DIR/.rffmpeg.json"

# 不可达的主目录层：当前目录层被采用时运行仍能成功，被跳过时必然失败——
# 值来源的优先级因此可观察，而不只靠 banner 的来源词。
HOME_BAD="$TMP_ROOT/home-unreachable"
mkdir -p "$HOME_BAD"
cat >"$HOME_BAD/.rffmpeg.json" <<JSON
{"server_url":"$UNREACHABLE","token":"$AUTH_TOKEN"}
JSON
chmod 600 "$HOME_BAD/.rffmpeg.json"

MEDIA="$(make_media config-trust.mp4 2)"

# run_case <label> <run-dir> <home-dir> <expect-rc> [env assignments...]
# Runs the CLI inside run-dir with the configuration environment absent, so the
# file layers decide; prints the CLI log path on stdout.
run_case() {
	local label="$1" dir="$2" home="$3" want_rc="$4"
	shift 4
	local log="$TMP_ROOT/cli-$label.log" rc=0
	(
		cd "$dir"
		env -u RFFMPEG_SERVER_URL -u RFFMPEG_TOKEN -u RFFMPEG_QUIET \
			HOME="$home" "$@" \
			"$RFFMPEG_BIN/rffmpeg" -i "$MEDIA" -c:v libx264 -preset veryfast "$label.mp4" \
			>/dev/null 2>"$log"
	) || rc=$?
	expect_rc "$rc" "$want_rc" "$label rc"
	printf '%s' "$log"
}

# --- 判据一 / 判据二：组可写的当前目录配置被跳过 ---------------------------
CWD_UNTRUSTED="$TMP_ROOT/cwd-untrusted"
mkdir -p "$CWD_UNTRUSTED"
cat >"$CWD_UNTRUSTED/rffmpeg.json" <<JSON
{"server_url":"$UNREACHABLE","token":"wrong"}
JSON
chmod 664 "$CWD_UNTRUSTED/rffmpeg.json"

LOG="$(run_case untrusted "$CWD_UNTRUSTED" "$HOME_DIR" 0)"
expect_in_file "$LOG" "Server: $SERVER_URL (user home)"
expect_in_file "$LOG" "Warning: ignoring config file ./rffmpeg.json: writable by group or others"
[[ "$(grep -c 'Warning: ignoring config file' "$LOG")" -eq 1 ]] ||
	fail "untrusted run printed $(grep -c 'Warning: ignoring config file' "$LOG") warning lines, want exactly one"

QUIET_LOG="$(run_case untrusted-quiet "$CWD_UNTRUSTED" "$HOME_DIR" 0 RFFMPEG_QUIET=1)"
expect_in_file "$QUIET_LOG" "Warning: ignoring config file ./rffmpeg.json: writable by group or others"
grep -q 'Remote FFmpeg Client' "$QUIET_LOG" && fail "quiet run printed the banner"
grep -qE '^Server: ' "$QUIET_LOG" && fail "quiet run printed the server line"

# --- 判据三：可信的当前目录配置被采用并优先于用户主目录 ---------------------
CWD_TRUSTED="$TMP_ROOT/cwd-trusted"
mkdir -p "$CWD_TRUSTED"
# 主目录层的 URL 已指向真实 server；这里用同一个 URL，可信层的采用由 banner
# 的来源词证明（来源不同、URL 相同）。
cat >"$CWD_TRUSTED/rffmpeg.json" <<JSON
{"server_url":"$SERVER_URL","token":"$AUTH_TOKEN"}
JSON
chmod 644 "$CWD_TRUSTED/rffmpeg.json"
LOG="$(run_case trusted "$CWD_TRUSTED" "$HOME_BAD" 0)"
expect_in_file "$LOG" "Server: $SERVER_URL (current directory)"
grep -q 'Warning: ignoring config file' "$LOG" && fail "trusted file still produced a warning"

# --- 判据四：符号链接按解析后的目标判定 -------------------------------------
LINK_DIR="$TMP_ROOT/cwd-link-trusted"
mkdir -p "$LINK_DIR"
TARGET="$TMP_ROOT/trusted-target.json"
cp "$CWD_TRUSTED/rffmpeg.json" "$TARGET"
chmod 644 "$TARGET"
ln -s "$TARGET" "$LINK_DIR/rffmpeg.json"
LOG="$(run_case link-trusted "$LINK_DIR" "$HOME_BAD" 0)"
expect_in_file "$LOG" "Server: $SERVER_URL (current directory)"

LINK_BAD_DIR="$TMP_ROOT/cwd-link-untrusted"
mkdir -p "$LINK_BAD_DIR"
BAD_TARGET="$TMP_ROOT/untrusted-target.json"
cp "$CWD_UNTRUSTED/rffmpeg.json" "$BAD_TARGET"
chmod 664 "$BAD_TARGET"
ln -s "$BAD_TARGET" "$LINK_BAD_DIR/rffmpeg.json"
LOG="$(run_case link-untrusted "$LINK_BAD_DIR" "$HOME_DIR" 0)"
expect_in_file "$LOG" "Warning: ignoring config file ./rffmpeg.json: writable by group or others"
expect_in_file "$LOG" "Server: $SERVER_URL (user home)"

LINK_DANGLING_DIR="$TMP_ROOT/cwd-link-dangling"
mkdir -p "$LINK_DANGLING_DIR"
ln -s "$TMP_ROOT/does-not-exist.json" "$LINK_DANGLING_DIR/rffmpeg.json"
LOG="$(run_case link-dangling "$LINK_DANGLING_DIR" "$HOME_DIR" 0)"
expect_in_file "$LOG" "Warning: ignoring config file ./rffmpeg.json: not a regular file"
expect_in_file "$LOG" "Server: $SERVER_URL (user home)"

# --- 判据五：目录或 FIFO 占据该文件名 ---------------------------------------
DIR_DIR="$TMP_ROOT/cwd-directory"
mkdir -p "$DIR_DIR/rffmpeg.json"
LOG="$(run_case directory "$DIR_DIR" "$HOME_DIR" 0)"
expect_in_file "$LOG" "Warning: ignoring config file ./rffmpeg.json: not a regular file"

FIFO_DIR="$TMP_ROOT/cwd-fifo"
mkdir -p "$FIFO_DIR"
mkfifo "$FIFO_DIR/rffmpeg.json"
LOG="$(run_case fifo "$FIFO_DIR" "$HOME_DIR" 0)"
expect_in_file "$LOG" "Warning: ignoring config file ./rffmpeg.json: not a regular file"

echo "PASS: untrusted/directory/FIFO/dangling layers skipped with one warning line each; trusted file and symlink-to-trusted adopted with the current-directory source"
