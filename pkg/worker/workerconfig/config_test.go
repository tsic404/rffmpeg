package workerconfig

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()

	if config.ServerURL == "" {
		t.Error("DefaultConfig() ServerURL should not be empty")
	}
	if config.FFmpegPath == "" {
		t.Error("DefaultConfig() FFmpegPath should not be empty")
	}
	if config.Timeout == 0 {
		t.Error("DefaultConfig() Timeout should not be zero")
	}
	if config.HeartbeatInterval == 0 {
		t.Error("DefaultConfig() HeartbeatInterval should not be zero")
	}
	if config.PollInterval == 0 {
		t.Error("DefaultConfig() PollInterval should not be zero")
	}
	if config.MaxConcurrent <= 0 {
		t.Error("DefaultConfig() MaxConcurrent should be positive")
	}
	if !config.AutoDetectGPU {
		t.Error("DefaultConfig() AutoDetectGPU should be true")
	}
	if !config.AutoDetectCodecs {
		t.Error("DefaultConfig() AutoDetectCodecs should be true")
	}
}

func TestLoadFromFile(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "worker.json")

	configContent := `{
		"server_url": "http://example.com:8080/api/v1",
		"worker_id": "test-worker-123",
		"name": "test-worker",
		"ffmpeg_path": "/usr/bin/ffmpeg",
		"timeout": "1h",
		"max_concurrent": 4,
		"auto_detect_gpu": false,
		"encoder_priority": ["h264_nvenc", "libx264"],
		"encoder_blacklist": ["libx265"]
	}`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	config, err := LoadFromFile(configPath)
	if err != nil {
		t.Fatalf("LoadFromFile() failed: %v", err)
	}

	if config.ServerURL != "http://example.com:8080/api/v1" {
		t.Errorf("ServerURL = %q, want %q", config.ServerURL, "http://example.com:8080/api/v1")
	}
	if config.WorkerID != "test-worker-123" {
		t.Errorf("WorkerID = %q, want %q", config.WorkerID, "test-worker-123")
	}
	if config.Name != "test-worker" {
		t.Errorf("Name = %q, want %q", config.Name, "test-worker")
	}
	if config.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent = %d, want 4", config.MaxConcurrent)
	}
	if config.AutoDetectGPU {
		t.Error("AutoDetectGPU should be false")
	}
	if len(config.EncoderPriority) != 2 {
		t.Errorf("EncoderPriority length = %d, want 2", len(config.EncoderPriority))
	}
	if len(config.EncoderBlacklist) != 1 {
		t.Errorf("EncoderBlacklist length = %d, want 1", len(config.EncoderBlacklist))
	}
}

func TestLoadFromFile_NotFound(t *testing.T) {
	_, err := LoadFromFile("/nonexistent/config.json")
	if err == nil {
		t.Error("LoadFromFile() should return error for nonexistent file")
	}
}

func TestLoadFromFile_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid.json")

	if err := os.WriteFile(configPath, []byte("not valid json"), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	_, err := LoadFromFile(configPath)
	if err == nil {
		t.Error("LoadFromFile() should return error for invalid JSON")
	}
}

func TestLoadFromEnv(t *testing.T) {
	// Set environment variables
	os.Setenv("RFFMPEG_SERVER_URL", "http://env-test:8080/api/v1")
	os.Setenv("RFFMPEG_WORKER_ID", "env-worker-id")
	os.Setenv("RFFMPEG_WORKER_NAME", "env-worker-name")
	os.Setenv("RFFMPEG_MAX_CONCURRENT", "8")
	os.Setenv("RFFMPEG_AUTO_DETECT_GPU", "false")
	defer func() {
		os.Unsetenv("RFFMPEG_SERVER_URL")
		os.Unsetenv("RFFMPEG_WORKER_ID")
		os.Unsetenv("RFFMPEG_WORKER_NAME")
		os.Unsetenv("RFFMPEG_MAX_CONCURRENT")
		os.Unsetenv("RFFMPEG_AUTO_DETECT_GPU")
	}()

	config := LoadFromEnv()

	if config.ServerURL != "http://env-test:8080/api/v1" {
		t.Errorf("ServerURL = %q, want %q", config.ServerURL, "http://env-test:8080/api/v1")
	}
	if config.WorkerID != "env-worker-id" {
		t.Errorf("WorkerID = %q, want %q", config.WorkerID, "env-worker-id")
	}
	if config.Name != "env-worker-name" {
		t.Errorf("Name = %q, want %q", config.Name, "env-worker-name")
	}
	if config.MaxConcurrent != 8 {
		t.Errorf("MaxConcurrent = %d, want 8", config.MaxConcurrent)
	}
	if config.AutoDetectGPU {
		t.Error("AutoDetectGPU should be false")
	}
}

func TestMerge(t *testing.T) {
	fileConfig := &Config{
		ServerURL:       "http://file:8080/api/v1",
		WorkerID:        "file-worker-id",
		MaxConcurrent:   2,
		AutoDetectGPU:   true,
		EncoderPriority: []string{"h264_nvenc", "libx264"},
	}

	envConfig := &Config{
		ServerURL:     "http://env:8080/api/v1",
		WorkerID:      "env-worker-id",
		MaxConcurrent: 4,
		AutoDetectGPU: false,
	}

	merged := Merge(fileConfig, envConfig)

	// Env should override file
	if merged.ServerURL != "http://env:8080/api/v1" {
		t.Errorf("ServerURL = %q, want %q", merged.ServerURL, "http://env:8080/api/v1")
	}
	if merged.WorkerID != "env-worker-id" {
		t.Errorf("WorkerID = %q, want %q", merged.WorkerID, "env-worker-id")
	}
	if merged.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent = %d, want 4", merged.MaxConcurrent)
	}
	if merged.AutoDetectGPU {
		t.Error("AutoDetectGPU should be false (env override)")
	}
	// File values should be preserved if not overridden
	if len(merged.EncoderPriority) != 2 {
		t.Errorf("EncoderPriority length = %d, want 2", len(merged.EncoderPriority))
	}
}

func TestMerge_NilFileConfig(t *testing.T) {
	envConfig := DefaultConfig()
	envConfig.WorkerID = "env-worker-id"

	merged := Merge(nil, envConfig)

	if merged.WorkerID != "env-worker-id" {
		t.Errorf("WorkerID = %q, want %q", merged.WorkerID, "env-worker-id")
	}
}

func TestResolvePaths(t *testing.T) {
	tmpDir := t.TempDir()
	absTmpDir, err := filepath.Abs(tmpDir)
	if err != nil {
		t.Fatalf("Failed to get absolute path: %v", err)
	}

	config := &Config{
		TempDir:    "relative/temp",
		FFmpegPath: "relative/ffmpeg",
	}

	if err := config.ResolvePaths(tmpDir); err != nil {
		t.Fatalf("ResolvePaths() failed: %v", err)
	}

	expectedTempDir := filepath.Join(absTmpDir, "relative/temp")
	if config.TempDir != expectedTempDir {
		t.Errorf("TempDir = %q, want %q", config.TempDir, expectedTempDir)
	}

	expectedFFmpegPath := filepath.Join(absTmpDir, "relative/ffmpeg")
	if config.FFmpegPath != expectedFFmpegPath {
		t.Errorf("FFmpegPath = %q, want %q", config.FFmpegPath, expectedFFmpegPath)
	}
}

func TestResolvePaths_AbsolutePath(t *testing.T) {
	config := &Config{
		TempDir:    "/absolute/temp",
		FFmpegPath: "/absolute/ffmpeg",
	}

	if err := config.ResolvePaths("/some/base"); err != nil {
		t.Fatalf("ResolvePaths() failed: %v", err)
	}

	// Absolute paths should remain unchanged
	if config.TempDir != "/absolute/temp" {
		t.Errorf("TempDir = %q, want %q", config.TempDir, "/absolute/temp")
	}
	if config.FFmpegPath != "/absolute/ffmpeg" {
		t.Errorf("FFmpegPath = %q, want %q", config.FFmpegPath, "/absolute/ffmpeg")
	}
}

func TestConfig_DurationParsing(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "worker.json")

	configContent := `{
		"timeout": "30m",
		"heartbeat_interval": "10s",
		"poll_interval": "2s"
	}`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	config, err := LoadFromFile(configPath)
	if err != nil {
		t.Fatalf("LoadFromFile() failed: %v", err)
	}

	if config.Timeout.ToDuration() != 30*time.Minute {
		t.Errorf("Timeout = %v, want %v", config.Timeout.ToDuration(), 30*time.Minute)
	}
	if config.HeartbeatInterval.ToDuration() != 10*time.Second {
		t.Errorf("HeartbeatInterval = %v, want %v", config.HeartbeatInterval.ToDuration(), 10*time.Second)
	}
	if config.PollInterval.ToDuration() != 2*time.Second {
		t.Errorf("PollInterval = %v, want %v", config.PollInterval.ToDuration(), 2*time.Second)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{
			name:    "defaults valid",
			mutate:  func(*Config) {},
			wantErr: false,
		},
		{
			name:    "zero heartbeat interval",
			mutate:  func(c *Config) { c.HeartbeatInterval = 0 },
			wantErr: true,
		},
		{
			name:    "negative timeout",
			mutate:  func(c *Config) { c.Timeout = -1 },
			wantErr: true,
		},
		{
			name:    "zero poll interval",
			mutate:  func(c *Config) { c.PollInterval = 0 },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.mutate(cfg)
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}
