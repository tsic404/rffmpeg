package worker

import "testing"

// TestHasOutputArgArityConsensus verifies hasOutputArg resolves option arity
// through the shared generated table (pkg/ffmpegopts): value-taking options
// that the old hand-maintained executor table wrongly called boolean
// (-discard, -disposition, -vol) must consume their value so the output path
// is detected, and terminal boolean flags (-xerror, -noautorotate) must not
// swallow the output file.
func TestHasOutputArgArityConsensus(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		// Old misclassifications: -discard/-disposition/-vol took values.
		{"discard consumes its value, output follows", []string{"-i", "input.mp4", "-discard", "nokey", "output.mp4"}, true},
		{"disposition consumes its value, output follows", []string{"-i", "input.mp4", "-disposition", "attached_pic", "output.mp4"}, true},
		{"vol consumes its value, output follows", []string{"-i", "input.mp4", "-vol", "256", "output.wav"}, true},
		// Terminal boolean flags must not swallow the output.
		{"xerror keeps output", []string{"-i", "input.mp4", "-xerror", "output.mp4"}, true},
		{"noautorotate keeps output", []string{"-i", "input.mp4", "-noautorotate", "output.mp4"}, true},
		{"nostats keeps output", []string{"-i", "input.mp4", "-nostats", "output.mp4"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasOutputArg(tc.args); got != tc.want {
				t.Errorf("hasOutputArg(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
