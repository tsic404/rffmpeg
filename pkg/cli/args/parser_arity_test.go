package args

import "testing"

// TestParseArityConsensus verifies that Parse resolves option arity through
// the shared generated table (pkg/ffmpegopts) consistently for the options
// the three hand-maintained tables previously disagreed on:
//
//   - terminal boolean flags (-xerror, -noautorotate, -nostats, -nostdin)
//     must not swallow the output file as a value;
//   - value-taking options (-discard, -disposition, -vol) must consume their
//     value so the following positional arg remains the output file.
func TestParseArityConsensus(t *testing.T) {
	p := NewParser()

	t.Run("terminal boolean flags keep output", func(t *testing.T) {
		for _, flag := range []string{"-xerror", "-noautorotate", "-nostats", "-nostdin", "-re"} {
			result, err := p.Parse([]string{"-i", "input.mp4", flag, "output.mp4"})
			if err != nil {
				t.Errorf("Parse with %s: error = %v, want output preserved", flag, err)
				continue
			}
			if result.OutputFile != "output.mp4" {
				t.Errorf("flag %s swallowed output: got OutputFile=%q", flag, result.OutputFile)
			}
		}
	})

	t.Run("value flags consume their value", func(t *testing.T) {
		cases := [][]string{
			{"-i", "input.mp4", "-discard", "nokey", "output.mp4"},
			{"-i", "input.mp4", "-disposition", "attached_pic", "output.mp4"},
			{"-i", "input.mp4", "-vol", "256", "output.wav"},
			{"-i", "input.mp4", "-fps_mode", "cfr", "output.mp4"},
		}
		for _, args := range cases {
			result, err := p.Parse(args)
			if err != nil {
				t.Errorf("Parse(%v): error = %v", args, err)
				continue
			}
			if result.OutputFile != "output.mp4" && result.OutputFile != "output.wav" {
				t.Errorf("Parse(%v): OutputFile = %q, want the real output path", args, result.OutputFile)
			}
			// The value must be carried into AllArgs next to its flag.
			found := false
			for i, a := range result.AllArgs {
				if (a == "-discard" || a == "-disposition" || a == "-vol" || a == "-fps_mode") && i+1 < len(result.AllArgs) {
					if result.AllArgs[i+1] != "<INPUT_FILE>" {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("Parse(%v): value flag lost its argument in AllArgs %v", args, result.AllArgs)
			}
		}
	})
}
