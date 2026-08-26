package ffmpegopts

import (
	"os/exec"
	"strings"
	"testing"
)

// TestConsensusWithNativeFFmpeg cross-checks every table entry (generated
// and hand-written) against real ffmpeg.
//
// Detection: run `ffmpeg ... -opt /dev/null`. If -opt is a true switch, the
// trailing /dev/null is the output file and ffmpeg proceeds past option
// parsing (muxer/format complaints are fine). If -opt consumes a value,
// /dev/null is swallowed and ffmpeg fails with "At least one output file
// must be specified" — that message is the misclassification signal, plus
// explicit value errors like "Missing argument" / "Expected number".
//
// Options absent from the local build ("Unrecognized option") are skipped:
// CI builds differ from the generating host; arity of a nonexistent option
// cannot be tested here.
func TestConsensusWithNativeFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("requires ffmpeg binary")
	}
	for name, isBool := range booleanFlags {
		args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=duration=0.05:size=64x64:rate=5", "-" + name, "/dev/null"}
		out, err := exec.Command("ffmpeg", args...).CombinedOutput()
		if err != nil {
			msg := string(out)
			switch {
			case strings.Contains(msg, "Unrecognized option"):
				t.Logf("skip -%s: not present in this ffmpeg build", name)
				continue
			case strings.Contains(msg, "At least one output file must be specified"),
				strings.Contains(msg, "Missing argument"),
				strings.Contains(msg, "Expected number"),
				strings.Contains(msg, "Expected"),
				strings.Contains(msg, "expects a value"),
				strings.Contains(msg, "requires a value"):
				if isBool {
					t.Errorf("option -%s classified boolean but it consumed /dev/null as a value:\n%s", name, msg)
				} else {
					t.Logf("ok -%s: correctly value-taking (hand-written override)", name)
				}
			default:
				// Option parsed fine or failed for unrelated reasons (codec /
				// muxer availability) — consistent with boolean classification.
				if !isBool {
					t.Errorf("option -%s classified value-taking but ffmpeg accepted '-%s /dev/null' with no value:\n%s", name, name, msg)
				}
			}
		} else if !isBool {
			// Hand-written false entries must actually consume a value.
			t.Errorf("option -%s marked value-taking but 'ffmpeg -%s /dev/null' succeeded without consuming a value", name, name)
		}
	}
}

// TestNoPrefixNegation pins the "-no<flag>" negation form: negating a known
// boolean flag stays boolean and must not swallow the output path.
func TestNoPrefixNegation(t *testing.T) {
	cases := map[string]string{ // negated → base
		"nostats":     "stats",
		"nostdin":     "stdin",
		"nobenchmark": "benchmark",
		"nocopyts":    "copyts",
	}
	for negated, base := range cases {
		if !booleanFlags[base] {
			t.Fatalf("precondition: base %q must be boolean", base)
		}
		if got := IsBoolean("-" + negated); !got {
			t.Errorf("IsBoolean(-%q) = false, want true (negation of %q)", negated, base)
		}
	}
	// A "-no" prefix over an unknown/value-taking base stays value-taking.
	for _, n := range []string{"nodiscard", "nodisposition", "novalue_unknown_opt"} {
		if IsBoolean("-" + n) {
			t.Errorf("IsBoolean(-%q) = true, want false", n)
		}
	}
}
