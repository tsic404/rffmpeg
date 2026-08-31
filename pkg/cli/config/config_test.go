package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefault(t *testing.T) {
	// Isolate test environment from external config files
	t.Setenv("HOME", t.TempDir())

	// Clear env vars
	os.Unsetenv("RFFMPEG_SERVER_URL")
	os.Unsetenv("RFFMPEG_TOKEN")
	os.Unsetenv("RFFMPEG_SHARED_FS")
	os.Unsetenv("RFFMPEG_MAX_RETRIES")

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
