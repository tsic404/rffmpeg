package client

import (
	"testing"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

func TestRenderProgressLine(t *testing.T) {
	tests := []struct {
		name    string
		payload protocol.WSProgressPayload
		want    string
	}{
		{
			name:    "positive ETA renders formatted time",
			payload: protocol.WSProgressPayload{Percent: 68.8, EtaSeconds: 61},
			want:    "Progress: 68.8% | ETA: 1:01",
		},
		{
			name:    "hour-scale ETA renders H:MM:SS",
			payload: protocol.WSProgressPayload{Percent: 10.0, EtaSeconds: 3661},
			want:    "Progress: 10.0% | ETA: 1:01:01",
		},
		{
			name:    "zero ETA renders n/a",
			payload: protocol.WSProgressPayload{Percent: 100.0, EtaSeconds: 0},
			want:    "Progress: 100.0% | ETA: n/a",
		},
		{
			name:    "negative ETA renders n/a",
			payload: protocol.WSProgressPayload{Percent: 50.0, EtaSeconds: -5},
			want:    "Progress: 50.0% | ETA: n/a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderProgressLine(tt.payload); got != tt.want {
				t.Fatalf("renderProgressLine() = %q, want %q", got, tt.want)
			}
		})
	}
}
