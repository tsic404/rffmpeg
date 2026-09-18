package client

import "strings"

// noticePrefix marks rffmpeg's own notification lines — cache hit, encoder
// rewrite, software fallback, and the worker's other [rffmpeg]-prefixed
// observability messages. These travel the same stderr stream as ffmpeg's raw
// output, but --quiet must not suppress them: they are the project's
// notification channel, and a quiet run still needs them (e.g. cache-hit
// confirmation for automation). Under --quiet every other stderr line is
// dropped — ffmpeg's own log, warnings and errors included; hard failures are
// still surfaced through the CLI's terminal "Job failed:" verdict.
const noticePrefix = "[rffmpeg]"

// noticeFilter passes only the complete [rffmpeg]-prefixed lines of a stderr
// chunk stream, buffering a trailing partial line across chunks so a notice
// split across a delivery boundary is never dropped. Each wait/stream entry
// point owns one instance for the lifetime of its WebSocket session; the zero
// value is ready to use.
type noticeFilter struct {
	pending string
}

// filter returns the [rffmpeg]-prefixed lines carried by chunk, each
// newline-terminated, or "" when chunk carries none.
func (f *noticeFilter) filter(chunk string) string {
	rest := f.pending + chunk
	f.pending = ""

	// The worker always adds a notice as a complete newline-terminated line,
	// but the batched stream may still end mid-line (a partial ffmpeg line).
	// Buffer everything after the last newline so the next chunk can complete
	// it; a chunk with no newline is entirely partial.
	lastNL := strings.LastIndexByte(rest, '\n')
	if lastNL < 0 {
		f.pending = rest
		return ""
	}
	f.pending = rest[lastNL+1:]
	rest = rest[:lastNL+1]

	var sb strings.Builder
	start := 0
	for start < len(rest) {
		nl := strings.IndexByte(rest[start:], '\n')
		if nl < 0 {
			break
		}
		line := rest[start : start+nl]
		if strings.HasPrefix(line, noticePrefix) {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
		start += nl + 1
	}
	return sb.String()
}
