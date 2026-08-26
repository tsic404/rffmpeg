package rewrite

import "testing"

// TestFindOutputFilePosArityConsensus verifies findOutputFilePos resolves
// option arity through the shared generated table (pkg/ffmpegopts): the
// value-taking options the old hand-maintained table misclassified as
// boolean (-discard, -disposition, -vol) must consume their value so the
// output path is found afterwards, and terminal boolean flags must not
// swallow the output file.
func TestFindOutputFilePosArityConsensus(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int // index of the output file
	}{
		// Old misclassifications: value-taking options consumed their value.
		{"discard keeps output after value", []string{"-i", "input.mp4", "-discard", "nokey", "output.mp4"}, 4},
		{"disposition keeps output after value", []string{"-i", "input.mp4", "-disposition", "attached_pic", "output.mp4"}, 4},
		{"vol keeps output after value", []string{"-i", "input.mp4", "-vol", "256", "output.wav"}, 4},
		// Terminal boolean flags must not swallow the output.
		{"xerror keeps output", []string{"-i", "input.mp4", "-xerror", "output.mp4"}, 3},
		{"noautorotate keeps output", []string{"-i", "input.mp4", "-noautorotate", "output.mp4"}, 3},
		{"nostats keeps output", []string{"-i", "input.mp4", "-nostats", "output.mp4"}, 3},
		// Shortest is a real switch; output follows directly.
		{"shortest keeps output", []string{"-i", "input.mp4", "-shortest", "output.mp4"}, 3},
		// Inline "=value" form never consumes the next arg.
		{"inline value keeps output", []string{"-i", "input.mp4", "-loglevel=verbose", "output.mp4"}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findOutputFilePos(tc.args); got != tc.want {
				t.Errorf("findOutputFilePos(%v) = %d, want %d (args=%v)", tc.args, got, tc.want, tc.args)
			}
		})
	}
}
