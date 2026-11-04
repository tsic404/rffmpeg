package client

import (
	"fmt"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// formatETA renders a remaining-seconds estimate as H:MM:SS (or M:SS under
// one hour). Negative or absurd values collapse to 0 — ffmpeg's speed
// estimate briefly spikes around encoder warm-up and would otherwise print
// a negative ETA.
func formatETA(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	h := seconds / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// renderProgressLine formats a WS progress payload as the CLI stderr
// progress line: "Progress: X% | ETA: <formatted or n/a>". The ETA segment is
// always present so monitoring scripts can parse a stable shape even on short
// tasks where the worker never produced an ETA. Shared by every
// WebSocket wait/stream entry point.
func renderProgressLine(p protocol.WSProgressPayload) string {
	eta := "n/a"
	if p.EtaSeconds > 0 {
		eta = formatETA(p.EtaSeconds)
	}
	return fmt.Sprintf("Progress: %.1f%% | ETA: %s", p.Percent, eta)
}
