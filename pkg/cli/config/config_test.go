package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefault(t *testing.T) {
	// Isolate test environment from external config files
	t.Setenv("HOME", t.TempDir())

	// Clear env vars
	os.Unsetenv("RFFMPEG_SERVER_URL")
	os.Unsetenv("RFFMPEG_TOKEN")
	os.Unsetenv("RFFMPEG_SHARED_FS")
	os.Unsetenv("RFFMPEG_MAX_RETRIES")
	os.Unsetenv("RFFMPEG_SERVER_LOSS_TIMEOUT")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ServerURL != DefaultServerURL {
		t.Errorf("Load() ServerURL = %v, want %v", cfg.ServerURL, DefaultServerURL)
	}
	if cfg.SharedFS {
		t.Errorf("Load() SharedFS = true, want false (default)")
	}
}

func TestLoadEnvOverride(t *testing.T) {
	os.Setenv("RFFMPEG_SERVER_URL", "http://test.example.com")
	os.Setenv("RFFMPEG_TOKEN", "test-token")
	defer func() {
		os.Unsetenv("RFFMPEG_SERVER_URL")
		os.Unsetenv("RFFMPEG_TOKEN")
	}()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ServerURL != "http://test.example.com" {
		t.Errorf("Load() ServerURL = %v, want http://test.example.com", cfg.ServerURL)
	}
	if cfg.Token != "test-token" {
		t.Errorf("Load() Token = %v, want test-token", cfg.Token)
	}
}

func TestLoadMaxRetriesEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	os.Unsetenv("RFFMPEG_MAX_RETRIES")

	if cfg, _ := Load(); cfg.MaxRetries != nil {
		t.Errorf("Load() MaxRetries = %v, want nil when unset", cfg.MaxRetries)
	}

	os.Setenv("RFFMPEG_MAX_RETRIES", "7")
	defer os.Unsetenv("RFFMPEG_MAX_RETRIES")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.MaxRetries == nil || *cfg.MaxRetries != 7 {
		t.Errorf("Load() MaxRetries = %v, want 7", cfg.MaxRetries)
	}

	// Non-numeric values are ignored, leaving the field unset.
	os.Setenv("RFFMPEG_MAX_RETRIES", "abc")
	if cfg, _ := Load(); cfg.MaxRetries != nil {
		t.Errorf("Load() MaxRetries = %v, want nil for non-numeric env", cfg.MaxRetries)
	}

	// 0 = no retries: it must be honored, not ignored.
	os.Setenv("RFFMPEG_MAX_RETRIES", "0")
	if cfg, _ := Load(); cfg.MaxRetries == nil || *cfg.MaxRetries != 0 {
		t.Errorf("Load() MaxRetries = %v, want 0 for zero env", cfg.MaxRetries)
	}
}

func TestLoadServerLossTimeoutEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	os.Unsetenv("RFFMPEG_SERVER_LOSS_TIMEOUT")

	// Unset -> nil (the caller falls back to DefaultServerLossTimeout).
	if cfg, _ := Load(); cfg.ServerLossTimeout != nil {
		t.Errorf("Load() ServerLossTimeout = %v, want nil when unset", cfg.ServerLossTimeout)
	}

	// Valid duration overrides.
	os.Setenv("RFFMPEG_SERVER_LOSS_TIMEOUT", "30s")
	defer os.Unsetenv("RFFMPEG_SERVER_LOSS_TIMEOUT")
	if cfg, _ := Load(); cfg.ServerLossTimeout == nil || time.Duration(*cfg.ServerLossTimeout) != 30*time.Second {
		t.Errorf("Load() ServerLossTimeout = %v, want 30s", cfg.ServerLossTimeout)
	}

	// 0 = cap disabled: it must be honored, not ignored.
	os.Setenv("RFFMPEG_SERVER_LOSS_TIMEOUT", "0s")
	if cfg, _ := Load(); cfg.ServerLossTimeout == nil || *cfg.ServerLossTimeout != 0 {
		t.Errorf("Load() ServerLossTimeout = %v, want 0 (cap disabled) for zero env", cfg.ServerLossTimeout)
	}

	// Invalid or negative durations are ignored, leaving the field unset.
	for _, v := range []string{"abc", "-5s"} {
		os.Setenv("RFFMPEG_SERVER_LOSS_TIMEOUT", v)
		if cfg, _ := Load(); cfg.ServerLossTimeout != nil {
			t.Errorf("Load() ServerLossTimeout = %v for env %q, want nil", cfg.ServerLossTimeout, v)
		}
	}
}

func TestLoadPollTimeoutEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	os.Unsetenv("RFFMPEG_POLL_TIMEOUT")

	// Unset -> nil (the caller falls back to DefaultPollTimeout).
	if cfg, _ := Load(); cfg.PollTimeout != nil {
		t.Errorf("Load() PollTimeout = %v, want nil when unset", cfg.PollTimeout)
	}

	// Valid duration overrides.
	os.Setenv("RFFMPEG_POLL_TIMEOUT", "25m")
	defer os.Unsetenv("RFFMPEG_POLL_TIMEOUT")
	if cfg, _ := Load(); cfg.PollTimeout == nil || time.Duration(*cfg.PollTimeout) != 25*time.Minute {
		t.Errorf("Load() PollTimeout = %v, want 25m", cfg.PollTimeout)
	}

	// 0 = opt out: it must be honored, not ignored.
	os.Setenv("RFFMPEG_POLL_TIMEOUT", "0s")
	if cfg, _ := Load(); cfg.PollTimeout == nil || *cfg.PollTimeout != 0 {
		t.Errorf("Load() PollTimeout = %v, want 0 (opt out) for zero env", cfg.PollTimeout)
	}

	// Invalid or negative durations are ignored, leaving the field unset.
	for _, v := range []string{"abc", "-5m"} {
		os.Setenv("RFFMPEG_POLL_TIMEOUT", v)
		if cfg, _ := Load(); cfg.PollTimeout != nil {
			t.Errorf("Load() PollTimeout = %v for env %q, want nil", cfg.PollTimeout, v)
		}
	}
}

func TestLoadConfigFile(t *testing.T) {
	// Create temp config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "rffmpeg.json")
	configContent := `{"server_url": "http://config.example.com", "token": "config-token", "max_retries": 7}`
	if err := os.WriteFile(configPath, []byte(configContent), 0600); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	// Change to temp dir
	oldDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldDir)

	os.Unsetenv("RFFMPEG_SERVER_URL")
	os.Unsetenv("RFFMPEG_TOKEN")
	os.Unsetenv("RFFMPEG_SHARED_FS")
	os.Unsetenv("RFFMPEG_MAX_RETRIES")
	os.Unsetenv("RFFMPEG_SERVER_LOSS_TIMEOUT")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ServerURL != "http://config.example.com" {
		t.Errorf("Load() ServerURL = %v, want http://config.example.com", cfg.ServerURL)
	}
	if cfg.MaxRetries == nil || *cfg.MaxRetries != 7 {
		t.Errorf("Load() MaxRetries = %v, want 7 from config file", cfg.MaxRetries)
	}
}

func TestLoadPollTimeoutConfigFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "rffmpeg.json")

	oldDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldDir)
	os.Unsetenv("RFFMPEG_POLL_TIMEOUT")

	// String form: "5m" must parse, not silently no-op.
	if err := os.WriteFile(configPath, []byte(`{"poll_timeout": "5m"}`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.PollTimeout == nil || time.Duration(*cfg.PollTimeout) != 5*time.Minute {
		t.Errorf("Load() PollTimeout = %v, want 5m from config file", cfg.PollTimeout)
	}

	// 0 in the config file is the opt-out value, not "unset".
	if err := os.WriteFile(configPath, []byte(`{"poll_timeout": "0s"}`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.PollTimeout == nil || *cfg.PollTimeout != 0 {
		t.Errorf("Load() PollTimeout = %v, want 0 (opt out) from config file", cfg.PollTimeout)
	}

	// Env overrides the config-file value.
	os.Setenv("RFFMPEG_POLL_TIMEOUT", "30m")
	defer os.Unsetenv("RFFMPEG_POLL_TIMEOUT")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.PollTimeout == nil || time.Duration(*cfg.PollTimeout) != 30*time.Minute {
		t.Errorf("Load() PollTimeout = %v, want 30m from env override", cfg.PollTimeout)
	}
}

func TestSave(t *testing.T) {
	cfg := &Config{
		ServerURL: "http://save.example.com",
		Token:     "save-token",
	}

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	if err := cfg.Save(configPath); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Read back and verify
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	_ = loaded // Config file in temp dir won't be loaded by default Load()
}

func TestLoadSharedFS_EnvSetTo1(t *testing.T) {
	os.Unsetenv("RFFMPEG_SERVER_URL")
	os.Unsetenv("RFFMPEG_TOKEN")
	os.Setenv("RFFMPEG_SHARED_FS", "1")
	defer os.Unsetenv("RFFMPEG_SHARED_FS")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.SharedFS {
		t.Errorf("Load() SharedFS = false, want true (RFFMPEG_SHARED_FS=1)")
	}
	if !cfg.IsSharedFS() {
		t.Errorf("IsSharedFS() = false, want true")
	}
}

func TestLoadSharedFS_EnvSetToTrue(t *testing.T) {
	os.Unsetenv("RFFMPEG_SERVER_URL")
	os.Unsetenv("RFFMPEG_TOKEN")
	os.Setenv("RFFMPEG_SHARED_FS", "true")
	defer os.Unsetenv("RFFMPEG_SHARED_FS")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.SharedFS {
		t.Errorf("Load() SharedFS = false, want true (RFFMPEG_SHARED_FS=true)")
	}
}

func TestLoadSharedFS_EnvSetTo0(t *testing.T) {
	os.Unsetenv("RFFMPEG_SERVER_URL")
	os.Unsetenv("RFFMPEG_TOKEN")
	os.Setenv("RFFMPEG_SHARED_FS", "0")
	defer os.Unsetenv("RFFMPEG_SHARED_FS")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SharedFS {
		t.Errorf("Load() SharedFS = true, want false (RFFMPEG_SHARED_FS=0)")
	}
}

func TestLoadSharedFS_EnvUnset(t *testing.T) {
	os.Unsetenv("RFFMPEG_SERVER_URL")
	os.Unsetenv("RFFMPEG_TOKEN")
	os.Unsetenv("RFFMPEG_SHARED_FS")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SharedFS {
		t.Errorf("Load() SharedFS = true, want false (RFFMPEG_SHARED_FS unset)")
	}
}

func TestLoadSharedFS_FromConfigFile(t *testing.T) {
	tmpDir := t.TempDir()
	configContent := `{"server_url": "http://localhost:8080", "shared_fs": true}`
	if err := os.WriteFile(filepath.Join(tmpDir, "rffmpeg.json"), []byte(configContent), 0600); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	oldDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldDir)

	os.Unsetenv("RFFMPEG_SERVER_URL")
	os.Unsetenv("RFFMPEG_TOKEN")
	os.Unsetenv("RFFMPEG_SHARED_FS")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.SharedFS {
		t.Errorf("Load() SharedFS = false, want true (from config file)")
	}
}

func TestLoadServerURLSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	os.Unsetenv("RFFMPEG_SERVER_URL")

	dir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	defer os.Chdir(oldDir)

	// No config file, no env -> default.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ServerURLSource != ServerURLSourceDefault {
		t.Errorf("ServerURLSource = %q, want %q", cfg.ServerURLSource, ServerURLSourceDefault)
	}

	// Config file sets server_url. The source must read "config file" even
	// when the value equals the default, or the banner would mislabel it.
	configContent := `{"server_url": "` + DefaultServerURL + `"}`
	if err := os.WriteFile(filepath.Join(dir, "rffmpeg.json"), []byte(configContent), 0600); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ServerURLSource != ServerURLSourceConfig {
		t.Errorf("ServerURLSource = %q, want %q", cfg.ServerURLSource, ServerURLSourceConfig)
	}

	// An explicit empty server_url must still read as "config file", never
	// "default". "" overwrites the default with an empty URL; null is a
	// JSON no-op for a string field and leaves the default value in place, but
	// both are key-present and so share the "config file" label.
	for _, tc := range []struct {
		content string
		wantURL string
	}{
		{`{"server_url": ""}`, ""},
		{`{"server_url": null}`, DefaultServerURL},
	} {
		if err := os.WriteFile(filepath.Join(dir, "rffmpeg.json"), []byte(tc.content), 0600); err != nil {
			t.Fatalf("Failed to write config file: %v", err)
		}
		cfg, err = Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.ServerURL != tc.wantURL {
			t.Errorf("ServerURL = %q, want %q for %s", cfg.ServerURL, tc.wantURL, tc.content)
		}
		if cfg.ServerURLSource != ServerURLSourceConfig {
			t.Errorf("ServerURLSource = %q, want %q for %s", cfg.ServerURLSource, ServerURLSourceConfig, tc.content)
		}
	}

	// Env overrides config file -> env.
	os.Setenv("RFFMPEG_SERVER_URL", "http://env.example.com")
	defer os.Unsetenv("RFFMPEG_SERVER_URL")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ServerURLSource != ServerURLSourceEnv {
		t.Errorf("ServerURLSource = %q, want %q", cfg.ServerURLSource, ServerURLSourceEnv)
	}
}
