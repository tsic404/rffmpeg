package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMergeExecEnvReplacesInheritedKeys locks the override semantics: a job's
// protocol proxy must win over the worker's own environment, not sit behind a
// duplicate key that getenv would ignore.
func TestMergeExecEnvReplacesInheritedKeys(t *testing.T) {
	got := mergeExecEnv(
		[]string{"PATH=/bin", "http_proxy=http://worker-proxy", "HOME=/home/w"},
		[]string{"http_proxy=http://job-proxy", "no_proxy="},
	)
	want := []string{"PATH=/bin", "HOME=/home/w", "http_proxy=http://job-proxy", "no_proxy="}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("mergeExecEnv = %q, want %q", got, want)
	}
}

// TestExecutorAppliesPerJobEnvironment proves the override reaches the child
// process, not just the merge helper.
func TestExecutorAppliesPerJobEnvironment(t *testing.T) {
	out := filepath.Join(t.TempDir(), "env.txt")
	script := filepath.Join(t.TempDir(), "ffmpeg-env")
	body := "printf '%s\\n%s\\n' \"$http_proxy\" \"$FFREPORT\" > " + shellSingleQuote(out) + "\nexit 0\n"
	if err := writeShellFixture(script, body); err != nil {
		t.Fatal(err)
	}

	ctx := WithExecEnv(context.Background(), []string{
		"http_proxy=http://127.0.0.1:1234",
		"FFREPORT=file=/tmp/report.log:level=32",
	})
	result := NewExecutor(script, 30*time.Second).Execute(ctx, nil)
	if result.Error != nil || result.ExitCode != 0 {
		t.Fatalf("executor result = %+v, want success", result)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimSpace(string(data)), "\n"); len(got) != 2 ||
		got[0] != "http://127.0.0.1:1234" || got[1] != "file=/tmp/report.log:level=32" {
		t.Errorf("child environment = %q, want the job overrides", got)
	}
}
