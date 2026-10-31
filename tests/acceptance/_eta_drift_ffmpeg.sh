#!/usr/bin/env bash
# Fake encoder for the ETA drift cases of TC-observability-03.
#
# A real encode cannot be asked for a specific throughput profile: the rate
# follows the content and the host, so a case meant to exercise a warm-up or a
# dip reproduces it on one machine and not on the next. This script replays a
# measured profile as ffmpeg stats lines at wall-clock pace instead, which is
# what the ETA estimator reads; every other invocation (the worker's capability
# probe) is delegated to the real encoder.
#
# Environment:
#   RFFMPEG_REAL_FFMPEG     the real ffmpeg binary
#   RFFMPEG_DRIFT_PROFILE   warmup | dip | burst
#   RFFMPEG_DRIFT_DURATION  media length of the input, in seconds
set -euo pipefail

real="${RFFMPEG_REAL_FFMPEG:?}"
profile="${RFFMPEG_DRIFT_PROFILE:?}"
duration="${RFFMPEG_DRIFT_DURATION:?}"

# An encode is the only invocation carrying an input file.
encode=0
for arg in "$@"; do
	[[ "$arg" == "-i" ]] && encode=1
done
if ((encode == 0)); then
	exec "$real" "$@"
fi
out="${*: -1}"

# Throughput of the captured real 4K runs, in media seconds per wall second,
# one value per 5s of wall clock — the curves the old estimator was measured
# 3% and 6 lines outside the tolerance on. warmup is a slow start that ramps
# to its peak at ~25s; dip falls to a trough at ~40s and recovers above the
# level it started from.
case "$profile" in
warmup) table="5.19 5.70 5.55 7.05 7.20 7.41 6.36 6.96 6.90 7.59" ;;
dip) table="5.25 6.06 4.95 5.01 5.01 4.56 4.50 4.20 4.05 5.25 6.21 6.06" ;;
# burst is the dip curve whose closing stats line publishes the pipeline's
# tail at once (5s of media in one frame), the shape etaOutputLagBoundSeconds
# exists for.
burst) table="5.25 6.06 4.95 5.01 5.01 4.56 4.50 4.20 4.05 5.25 6.21 6.06" ;;
*)
	echo "unknown drift profile: $profile" >&2
	exit 2
	;;
esac

# wall, media at that wall, pace — one line per 0.5s of the replay.
schedule="$(mktemp)"
trap 'rm -f "$schedule"' EXIT
awk -v d="$duration" -v table="$table" -v burst="$([[ "$profile" == burst ]] && echo 1 || echo 0)" '
BEGIN {
	n = split(table, rate, " ")
	w = 0; m = 0
	while (m < d) {
		idx = int(w / 5) + 1
		if (idx > n) idx = n
		r = rate[idx]
		# The closing line publishes the pipeline tail at once.
		if (burst && m + r*0.5 >= d - 1.0) { m += 5.0 }
		printf "%.2f %.3f %.3f\n", w, m, r
		w += 0.5; m += r*0.5
	}
}' >"$schedule"

while read -r wall media rate; do
	sec="${media%.*}"
	cs="$(awk -v m="$media" 'BEGIN { printf "%02d", int((m-int(m))*100) }')"
	printf 'frame=%d fps=24 q=28.0 size=%dkB time=%02d:%02d:%02d.%s bitrate=1200.0kbits/s speed=%.2fx\r' \
		"$(awk -v m="$media" 'BEGIN { printf "%d", m*24 }')" \
		"$(awk -v m="$media" 'BEGIN { printf "%d", m*30 }')" \
		$((sec / 3600)) $((sec % 3600 / 60)) $((sec % 60)) "$cs" "$rate" >&2
	sleep 0.5
done <"$schedule"
printf '\n' >&2

# The worker rejects a zero-byte output; nothing downstream decodes it.
printf 'eta-drift-profile:%s\n' "$profile" >"$out"
