#!/usr/bin/env bash
# TC-encoder-02: 不可用编码器 → 同族降级；跨格式拒绝（AC-22、F6 负向）
# 判据：出现 `[rffmpeg] <codec> unavailable, fallback to <name>` 行；降级目标
# 与请求同族，跨格式（AV1→H.264 之类）不发生。
set -euo pipefail
PORT="${PORT:-18999}"
ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ENV_DIR/_env.sh"

build_binaries
start_server
start_worker
wait_ready

MEDIA="$(make_media family.mp4 4)"
ENCODERS="$(api_get /api/v1/workers |
	python3 -c 'import json,sys; print(" ".join(sorted({e for w in json.load(sys.stdin)["workers"] for e in w["encoders"]})))')"

pick_missing() {
	local candidate
	for candidate in "$@"; do
		grep -qw "$candidate" <<<"$ENCODERS" || { printf '%s' "$candidate"; return 0; }
	done
	return 1
}

# Same-family fallback: an unavailable H.264 encoder must be replaced by another
# H.264 encoder (never by a different codec family).
H264_MISSING="$(pick_missing h264_nvenc h264_qsv h264_amf h264_vulkan h264_vaapi)" ||
	fail "every H.264 hardware encoder is available; cannot exercise the fallback path"

LOG="$TMP_ROOT/fallback.log"
set +e
"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
	-i "$MEDIA" -c:v "$H264_MISSING" "$TMP_ROOT/fallback.mp4" 2>"$LOG"
RC=$?
set -e
expect_rc "$RC" 0 "same-family fallback rc"
grep -qE "^\[rffmpeg\] $H264_MISSING unavailable, fallback to [A-Za-z0-9_.-]+$" "$LOG" ||
	fail "missing same-family fallback line for $H264_MISSING"
CODE="$(ffprobe -v error -select_streams v:0 -show_entries stream=codec_name -of default=nw=1:nk=1 "$TMP_ROOT/fallback.mp4")"
[[ "$CODE" == "h264" ]] || fail "fallback changed the codec family: $CODE"

# Cross-format refusal: an unavailable AV1 encoder resolves only inside AV1.
AV1_MISSING="$(pick_missing av1_nvenc av1_qsv av1_amf av1_vaapi av1_vulkan)" || AV1_MISSING=""
if [[ -n "$AV1_MISSING" ]]; then
	LOG2="$TMP_ROOT/av1.log"
	set +e
	"$RFFMPEG_BIN/rffmpeg" --server "$(acceptance_server_url)" --token "$AUTH_TOKEN" \
		-i "$MEDIA" -c:v "$AV1_MISSING" "$TMP_ROOT/av1.mp4" 2>"$LOG2"
	RC2=$?
	set -e
	expect_rc "$RC2" 0 "av1 family fallback rc"
	grep -qE "^\[rffmpeg\] $AV1_MISSING unavailable, fallback to (libaom-av1|libsvtav1|av1_[A-Za-z0-9_.-]+)$" "$LOG2" ||
		fail "AV1 request did not fall back inside the AV1 family"
	grep -qE '^\[rffmpeg\] (libx264|libx265|libvpx) ' "$LOG2" &&
		fail "cross-format fallback occurred (F6)"
	CODE2="$(ffprobe -v error -select_streams v:0 -show_entries stream=codec_name -of default=nw=1:nk=1 "$TMP_ROOT/av1.mp4")"
	[[ "$CODE2" == "av1" ]] || fail "AV1 request produced codec $CODE2"
else
	echo "NOTE: every AV1 hardware encoder is available; cross-format path not exercised"
fi

echo "PASS: same-family fallback observed, no cross-format substitution"
