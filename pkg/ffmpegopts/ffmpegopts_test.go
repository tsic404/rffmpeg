package ffmpegopts

import "testing"

// TestIsBoolean_Regression pins the arity of options that previously
// disagreed between the three hand-maintained tables (parser booleanFlags,
// executor isFlagWithValue, rewrite booleanFFmpegFlags) or were misclassified.
//
//	-discard / -disposition: executor treated them as switches (wrong — both
//	take a value), which made hasOutputArg misjudge the output position and the
//	command lose its output path.
//
//	-xerror / -noautorotate at the end of an arg list must not swallow the
//	output file as a value.
func TestIsBoolean_Regression(t *testing.T) {
	cases := []struct {
		flag string
		want bool
	}{
		{"-xerror", true},
		{"-noautorotate", true},
		{"-shortest", true},
		{"-y", true},
		{"-n", true},
		{"-an", true},
		{"-vn", true},
		{"-sn", true},
		{"-dn", true},
		{"-re", true},
		{"-stdin", true},
		{"-copyts", true},
		{"-bitexact", true},
		// value-taking options that older tables wrongly called boolean:
		{"-discard", false},
		{"-disposition", false},
		{"-vol", false},
		{"-fps_mode", false},
		// stream specifier forms resolve to the base option:
		{"-c:v", false},
		{"-b:a", false},
		{"-shortest:v", true},
		// inline "=value" never consumes the next argument:
		{"-loglevel=verbose", true},
		{"-c:v=libx264", true},
	}
	for _, tc := range cases {
		if got := IsBoolean(tc.flag); got != tc.want {
			t.Errorf("IsBoolean(%q) = %v, want %v", tc.flag, got, tc.want)
		}
	}
}

func TestOverwritePolicy(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want OverwriteMode
	}{
		{name: "explicit -y forces overwrite", args: []string{"-y", "-i", "in.mp4", "out.mp4"}, want: OverwriteForce},
		{name: "explicit -n never overwrites", args: []string{"-n", "-i", "in.mp4", "out.mp4"}, want: OverwriteNever},
		{name: "no flag defaults to ask (refuse)", args: []string{"-i", "in.mp4", "-c:v", "libx264", "out.mp4"}, want: OverwriteAsk},
		{name: "-n wins over -y", args: []string{"-y", "-n", "-i", "in.mp4", "out.mp4"}, want: OverwriteNever},
		{name: "-y after -- separator ignored", args: []string{"-i", "in.mp4", "--", "-y", "out.mp4"}, want: OverwriteAsk},
		{name: "-n after -- separator ignored", args: []string{"-i", "in.mp4", "--", "-n", "out.mp4"}, want: OverwriteAsk},
		{name: "empty args", args: []string{}, want: OverwriteAsk},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OverwritePolicy(tc.args); got != tc.want {
				t.Errorf("OverwritePolicy(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
