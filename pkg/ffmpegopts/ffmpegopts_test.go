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
