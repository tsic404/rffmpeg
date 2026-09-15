package worker

import (
	"reflect"
	"strings"
	"testing"
)

// movflagsSet parses an ffmpeg -movflags value into the set of flags it
// enables. ffmpeg treats -movflags as an unordered bitmask separated by '+'
// (add) and '-' (remove); the order of flags in the value is not meaningful,
// so tests must assert set membership rather than a literal string.
func movflagsSet(value string) map[string]bool {
	set := map[string]bool{}
	for _, tok := range strings.FieldsFunc(value, func(r rune) bool { return r == '+' || r == '-' }) {
		if tok != "" {
			set[tok] = true
		}
	}
	return set
}

// extractMovflagsValue returns the value of the injected -movflags flag, or
// fails the test if it is absent.
func extractMovflagsValue(t *testing.T, args []string) string {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-movflags" {
			return args[i+1]
		}
	}
	t.Fatalf("-movflags not found in args: %v", args)
	return ""
}

func TestApplyStreamableFormat(t *testing.T) {
	// The fragmented-output flags the worker must inject for streamable
	// mp4/mov output. Asserted as a set: ffmpeg parses -movflags as an
	// unordered bitmask, so only membership — never order — is a contract.
	fragmentedFlags := []string{"empty_moov", "frag_keyframe", "default_base_moof"}

	injectTests := []struct {
		name       string
		args       []string
		outputPath string
	}{
		{
			name:       "mp4 to stdout gets fragmentation flags",
			args:       []string{"-y", "-i", "in.mp4", "-c:v", "h264_qsv", "-f", "mp4", "-"},
			outputPath: "-",
		},
		{
			name:       "mov to stdout gets fragmentation flags",
			args:       []string{"-y", "-i", "in.mp4", "-f", "mov", "-"},
			outputPath: "-",
		},
		{
			name:       "streaming with lavfi input only output mp4 gets flags",
			args:       []string{"-y", "-f", "lavfi", "-i", "testsrc=duration=0.1", "-c:v", "libx264", "-f", "mp4", "-"},
			outputPath: "-",
		},
	}

	for _, tt := range injectTests {
		t.Run(tt.name, func(t *testing.T) {
			got, note := applyStreamableFormat(tt.args, tt.outputPath)
			if note == "" {
				t.Fatalf("expected an injection note, got empty")
			}

			movflags := extractMovflagsValue(t, got)
			set := movflagsSet(movflags)
			for _, flag := range fragmentedFlags {
				if !set[flag] {
					t.Errorf("injected movflags %q missing flag %q", movflags, flag)
				}
			}

			// The output token must remain the trailing argument so ffmpeg
			// still writes to stdout rather than a stray file.
			if got[len(got)-1] != "-" {
				t.Errorf("output token = %q, want trailing \"-\" (args=%v)", got[len(got)-1], got)
			}
		})
	}

	unchangedTests := []struct {
		name       string
		args       []string
		outputPath string
	}{
		{
			name:       "explicit movflags are preserved",
			args:       []string{"-y", "-i", "in.mp4", "-f", "mp4", "-movflags", "+faststart", "-"},
			outputPath: "-",
		},
		{
			name:       "mpegts untouched",
			args:       []string{"-y", "-i", "in.mp4", "-f", "mpegts", "-"},
			outputPath: "-",
		},
		{
			name:       "input-only lavfi format not mistaken for output mp4",
			args:       []string{"-y", "-f", "lavfi", "-i", "testsrc=duration=0.1", "-c:v", "libx264", "out.mp4"},
			outputPath: "/tmp/jobs/job-1/out.mp4",
		},
		{
			name:       "non-streaming output untouched even with -f mp4",
			args:       []string{"-y", "-i", "in.mp4", "-c:v", "libx264", "-f", "mp4", "out.mp4"},
			outputPath: "/tmp/jobs/job-1/out.mp4",
		},
		{
			name:       "no format flag untouched",
			args:       []string{"-y", "-i", "in.mp4", "-c:v", "libx264", "-"},
			outputPath: "-",
		},
	}

	for _, tt := range unchangedTests {
		t.Run(tt.name, func(t *testing.T) {
			got, note := applyStreamableFormat(tt.args, tt.outputPath)
			if note != "" {
				t.Errorf("note = %q, want empty", note)
			}
			if !reflect.DeepEqual(got, tt.args) {
				t.Errorf("args = %v, want unchanged %v", got, tt.args)
			}
		})
	}
}

func TestApplyStreamableFormatDoesNotMutateInput(t *testing.T) {
	original := []string{"-y", "-i", "in.mp4", "-f", "mp4", "-"}
	before := append([]string(nil), original...)

	got, _ := applyStreamableFormat(original, "-")

	if !reflect.DeepEqual(original, before) {
		t.Errorf("input args mutated: %v, want %v", original, before)
	}
	if len(got) != len(original)+2 {
		t.Errorf("expected 2 injected flags, got %d extra", len(got)-len(original))
	}
}
