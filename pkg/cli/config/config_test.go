package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// clearEnv blanks every configuration-plane variable so ambient values from
// the developer's shell or another test cannot leak into a case. An empty
// value is the documented "unset" spelling for all of them.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		EnvServerURL, EnvToken, EnvSharedFS, EnvQuiet, EnvAutoHW, EnvRetry,
		EnvJobTimeout, EnvPollTimeout, EnvMaxRetries, EnvServerLossTimeout,
		EnvInfoFormat, EnvLocalFallback, EnvLogFile, EnvJobID,
	} {
		t.Setenv(name, "")
	}
}

// captureStderr redirects os.Stderr for the duration of a load and returns the
// collected text once the caller closes over it.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	return func() string {
		t.Helper()
		os.Stderr = old
		if err := w.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return string(data)
	}
}

// writeLayer writes a config file with mode and returns its path.
func writeLayer(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	// WriteFile honors the umask; force the exact mode the case needs.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	return path
}

func emptyPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		CurrentDir: filepath.Join(dir, "cwd.json"),
		Home:       filepath.Join(dir, "home.json"),
		System:     filepath.Join(dir, "system.json"),
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := load(emptyPaths(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ServerURL != DefaultServerURL {
		t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, DefaultServerURL)
	}
	if cfg.ServerURLSource != ServerURLSourceDefault {
		t.Errorf("ServerURLSource = %q, want %q", cfg.ServerURLSource, ServerURLSourceDefault)
	}
	if cfg.InfoFormat != InfoFormatText {
		t.Errorf("InfoFormat = %q, want %q", cfg.InfoFormat, InfoFormatText)
	}
	if cfg.Quiet || cfg.AutoHW || cfg.Retry || cfg.SharedFS || cfg.LocalFallback {
		t.Errorf("boolean defaults are not all off: %+v", cfg)
	}
	if cfg.JobTimeout != nil || cfg.PollTimeout != nil || cfg.MaxRetries != nil || cfg.ServerLossTimeout != nil {
		t.Errorf("budget defaults are not unset: %+v", cfg)
	}
}

// TestLoadLayerPrecedence pins the documented order: environment, current
// directory, user home, system, defaults — each layer overriding only the keys
// it sets.
func TestLoadLayerPrecedence(t *testing.T) {
	clearEnv(t)
	paths := emptyPaths(t)
	writeLayer(t, filepath.Dir(paths.System), "system.json",
		`{"server_url":"http://system","token":"system-token","quiet":true,"max_retries":3}`, 0o644)
	writeLayer(t, filepath.Dir(paths.Home), "home.json",
		`{"server_url":"http://home","max_retries":7,"auto_hw":true}`, 0o644)
	writeLayer(t, filepath.Dir(paths.CurrentDir), "cwd.json",
		`{"poll_timeout":"3m","retry":true}`, 0o644)

	cfg, err := load(paths)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ServerURL != "http://home" || cfg.ServerURLSource != ServerURLSourceHome {
		t.Errorf("server URL = %q (%q), want http://home (%q)", cfg.ServerURL, cfg.ServerURLSource, ServerURLSourceHome)
	}
	if cfg.Token != "system-token" {
		t.Errorf("Token = %q, want the system layer's value (no higher layer sets it)", cfg.Token)
	}
	if !cfg.Quiet {
		t.Error("Quiet = false, want the system layer's true")
	}
	if !cfg.AutoHW {
		t.Error("AutoHW = false, want the home layer's true")
	}
	if cfg.MaxRetries == nil || *cfg.MaxRetries != 7 {
		t.Errorf("MaxRetries = %v, want the home layer's 7", cfg.MaxRetries)
	}
	if !cfg.Retry {
		t.Error("Retry = false, want the current-directory layer's true")
	}
	if cfg.PollTimeout == nil || time.Duration(*cfg.PollTimeout) != 3*time.Minute {
		t.Errorf("PollTimeout = %v, want the current-directory layer's 3m", cfg.PollTimeout)
	}

	t.Setenv(EnvServerURL, "http://env")
	cfg, err = load(paths)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ServerURL != "http://env" || cfg.ServerURLSource != ServerURLSourceEnv {
		t.Errorf("server URL = %q (%q), want the env layer", cfg.ServerURL, cfg.ServerURLSource)
	}
}

// TestCurrentDirTrust covers the current-directory adoption rules: regular
// file, owner, then group/other writability, judged by a symlink's target.
func TestCurrentDirTrust(t *testing.T) {
	clearEnv(t)

	trusted := `{"server_url":"http://trusted"}`
	cases := []struct {
		name    string
		prepare func(t *testing.T, dir string, path string)
		wantURL string
		wantSrc ServerURLSource
		warn    string // empty = no warning expected
	}{
		{
			name: "regular file is adopted",
			prepare: func(t *testing.T, dir, path string) {
				writeLayer(t, dir, filepath.Base(path), trusted, 0o644)
			},
			wantURL: "http://trusted",
			wantSrc: ServerURLSourceCWD,
		},
		{
			name: "group writable is skipped",
			prepare: func(t *testing.T, dir, path string) {
				writeLayer(t, dir, filepath.Base(path), trusted, 0o664)
			},
			wantURL: DefaultServerURL,
			wantSrc: ServerURLSourceDefault,
			warn:    reasonWritable,
		},
		{
			name: "world writable is skipped",
			prepare: func(t *testing.T, dir, path string) {
				writeLayer(t, dir, filepath.Base(path), trusted, 0o646)
			},
			wantURL: DefaultServerURL,
			wantSrc: ServerURLSourceDefault,
			warn:    reasonWritable,
		},
		{
			name: "directory is skipped",
			prepare: func(t *testing.T, dir, path string) {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
			wantURL: DefaultServerURL,
			wantSrc: ServerURLSourceDefault,
			warn:    reasonNotRegularFile,
		},
		{
			name: "fifo is skipped",
			prepare: func(t *testing.T, dir, path string) {
				if err := syscall.Mkfifo(path, 0o644); err != nil {
					t.Fatalf("mkfifo: %v", err)
				}
			},
			wantURL: DefaultServerURL,
			wantSrc: ServerURLSourceDefault,
			warn:    reasonNotRegularFile,
		},
		{
			name: "dangling symlink is skipped as not a regular file",
			prepare: func(t *testing.T, dir, path string) {
				if err := os.Symlink(filepath.Join(dir, "missing"), path); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			wantURL: DefaultServerURL,
			wantSrc: ServerURLSourceDefault,
			warn:    reasonNotRegularFile,
		},
		{
			name: "symlink to an untrusted target is judged by the target",
			prepare: func(t *testing.T, dir, path string) {
				target := writeLayer(t, dir, "target.json", trusted, 0o664)
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			wantURL: DefaultServerURL,
			wantSrc: ServerURLSourceDefault,
			warn:    reasonWritable,
		},
		{
			name: "symlink to a trusted target is adopted",
			prepare: func(t *testing.T, dir, path string) {
				target := writeLayer(t, dir, "target.json", trusted, 0o644)
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			wantURL: "http://trusted",
			wantSrc: ServerURLSourceCWD,
		},
		{
			name:    "missing file is an absent layer",
			prepare: func(t *testing.T, dir, path string) {},
			wantURL: DefaultServerURL,
			wantSrc: ServerURLSourceDefault,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "cwd.json")
			tc.prepare(t, dir, path)

			done := captureStderr(t)
			cfg, err := load(Paths{CurrentDir: path, Home: filepath.Join(dir, "home.json"), System: filepath.Join(dir, "system.json")})
			stderr := done()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.ServerURL != tc.wantURL || cfg.ServerURLSource != tc.wantSrc {
				t.Errorf("server URL = %q (%q), want %q (%q)", cfg.ServerURL, cfg.ServerURLSource, tc.wantURL, tc.wantSrc)
			}
			if tc.warn == "" {
				if stderr != "" {
					t.Errorf("unexpected warning: %q", stderr)
				}
				return
			}
			want := "Warning: ignoring config file " + path + ": " + tc.warn + "\n"
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr = %q, want the warning %q", stderr, want)
			}
			if strings.Count(stderr, "\n") != 1 {
				t.Errorf("stderr has more than one line: %q", stderr)
			}
		})
	}
}

// TestCurrentDirOwnerMismatch pins the owner condition. Creating a file owned
// by another user needs root, so the case swaps the effective uid instead.
func TestCurrentDirOwnerMismatch(t *testing.T) {
	clearEnv(t)
	owner, ok := fileOwnerUID(mustStat(t, writeLayer(t, t.TempDir(), "probe.json", "{}", 0o644)))
	if !ok {
		t.Skip("this platform reports no file owner")
	}
	original := currentEUID
	currentEUID = func() uint32 { return owner + 1 }
	t.Cleanup(func() { currentEUID = original })

	dir := t.TempDir()
	path := writeLayer(t, dir, "cwd.json", `{"server_url":"http://trusted"}`, 0o644)

	done := captureStderr(t)
	cfg, err := load(Paths{CurrentDir: path, Home: filepath.Join(dir, "home.json"), System: filepath.Join(dir, "system.json")})
	stderr := done()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ServerURL != DefaultServerURL {
		t.Errorf("ServerURL = %q, want the built-in default", cfg.ServerURL)
	}
	if !strings.Contains(stderr, reasonOwnerMismatch) {
		t.Errorf("stderr = %q, want the %q reason", stderr, reasonOwnerMismatch)
	}
}

// mustStat is a helper for the owner-check fixture.
func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi
}

func TestLoadEnvValues(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvServerURL, "http://env")
	t.Setenv(EnvToken, "env-token")
	t.Setenv(EnvQuiet, "true")
	t.Setenv(EnvAutoHW, "1")
	t.Setenv(EnvRetry, "1")
	t.Setenv(EnvJobTimeout, "45m")
	t.Setenv(EnvPollTimeout, "0")
	t.Setenv(EnvMaxRetries, "0")
	t.Setenv(EnvServerLossTimeout, "0")
	t.Setenv(EnvInfoFormat, "json")
	t.Setenv(EnvLocalFallback, "1")
	t.Setenv(EnvSharedFS, "true")
	t.Setenv(EnvLogFile, "/tmp/diag.log")

	cfg, err := load(emptyPaths(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ServerURL != "http://env" || cfg.ServerURLSource != ServerURLSourceEnv {
		t.Errorf("server URL = %q (%q)", cfg.ServerURL, cfg.ServerURLSource)
	}
	if cfg.Token != "env-token" {
		t.Errorf("Token = %q", cfg.Token)
	}
	if !cfg.Quiet || !cfg.AutoHW || !cfg.Retry || !cfg.SharedFS || !cfg.LocalFallback {
		t.Errorf("boolean env values not applied: %+v", cfg)
	}
	if cfg.JobTimeout == nil || time.Duration(*cfg.JobTimeout) != 45*time.Minute {
		t.Errorf("JobTimeout = %v, want 45m", cfg.JobTimeout)
	}
	if cfg.PollTimeout == nil || time.Duration(*cfg.PollTimeout) != 0 {
		t.Errorf("PollTimeout = %v, want an explicit 0", cfg.PollTimeout)
	}
	if cfg.MaxRetries == nil || *cfg.MaxRetries != 0 {
		t.Errorf("MaxRetries = %v, want an explicit 0", cfg.MaxRetries)
	}
	if cfg.ServerLossTimeout == nil || time.Duration(*cfg.ServerLossTimeout) != 0 {
		t.Errorf("ServerLossTimeout = %v, want an explicit 0", cfg.ServerLossTimeout)
	}
	if !cfg.IsJSON() {
		t.Errorf("InfoFormat = %q, want json", cfg.InfoFormat)
	}
	if cfg.LogFile != "/tmp/diag.log" {
		t.Errorf("LogFile = %q", cfg.LogFile)
	}
}

// TestLoadRejectsInvalidValues pins C-13: an unusable job budget is rejected
// before anything is submitted, never silently replaced by a default.
func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name  string
		env   string
		value string
		want  string
	}{
		{"job timeout zero", EnvJobTimeout, "0", "positive duration"},
		{"job timeout negative", EnvJobTimeout, "-5m", "positive duration"},
		{"job timeout unparsable", EnvJobTimeout, "not-a-duration", "positive duration"},
		{"poll timeout negative", EnvPollTimeout, "-1s", "non-negative duration"},
		{"max retries negative", EnvMaxRetries, "-1", "non-negative integer"},
		{"server loss negative", EnvServerLossTimeout, "-1s", "non-negative duration"},
		{"info format unknown", EnvInfoFormat, "yaml", "info_format"},
		{"quiet not boolean", EnvQuiet, "maybe", "must be one of"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(tc.env, tc.value)
			_, err := load(emptyPaths(t))
			if err == nil {
				t.Fatalf("load accepted %s=%q", tc.env, tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestJobTimeoutConfigFileValidation covers the same C-13 rejection for the
// file layer.
func TestJobTimeoutConfigFileValidation(t *testing.T) {
	clearEnv(t)
	paths := emptyPaths(t)
	writeLayer(t, filepath.Dir(paths.Home), "home.json", `{"job_timeout":"0s"}`, 0o644)
	if _, err := load(paths); err == nil || !strings.Contains(err.Error(), "job_timeout") {
		t.Fatalf("load error = %v, want a job_timeout rejection", err)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "saved.json")
	cfg := &Config{ServerURL: "http://saved", Token: "t", InfoFormat: InfoFormatJSON, Quiet: true}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{`"server_url": "http://saved"`, `"quiet": true`, `"info_format": "json"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("saved file lacks %q:\n%s", want, data)
		}
	}
	if strings.Contains(string(data), `"server_url_source"`) {
		t.Errorf("saved file carries the derived source:\n%s", data)
	}
}
