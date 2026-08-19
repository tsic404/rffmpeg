package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	tlspkg "github.com/tsix404/rffmpeg/pkg/tls"
)

func TestDefaultServerConfig(t *testing.T) {
	cfg := DefaultServerConfig()

	if cfg.Port != "8080" {
		t.Errorf("Default port should be 8080, got %s", cfg.Port)
	}
	if cfg.DataDir != "./data" {
		t.Errorf("Default data dir should be ./data, got %s", cfg.DataDir)
	}
	if cfg.TLS == nil {
		t.Error("TLS config should not be nil")
	}
	if cfg.TLS.Enabled {
		t.Error("TLS should be disabled by default")
	}
}

func TestLoadFromFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	// Create a test config file
	configContent := map[string]interface{}{
		"port":     "9090",
		"data_dir": "/data",
		"version":  "2.0.0",
		"tls": map[string]interface{}{
			"enabled":        true,
			"cert_file":      "/certs/server.crt",
			"key_file":       "/certs/server.key",
			"mtls":           true,
			"client_ca_file": "/certs/ca.crt",
		},
	}

	data, err := json.Marshal(configContent)
	if err != nil {
		t.Fatalf("Failed to marshal config: %v", err)
	}

	if err := os.WriteFile(configPath, data, 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg, err := LoadFromFile(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.Port != "9090" {
		t.Errorf("Port should be 9090, got %s", cfg.Port)
	}
	if cfg.DataDir != "/data" {
		t.Errorf("Data dir should be /data, got %s", cfg.DataDir)
	}
	if cfg.Version != "2.0.0" {
		t.Errorf("Version should be 2.0.0, got %s", cfg.Version)
	}
	if !cfg.TLS.Enabled {
		t.Error("TLS should be enabled")
	}
	if !cfg.TLS.MTLS {
		t.Error("mTLS should be enabled")
	}
}

func TestLoadFromEnv(t *testing.T) {
	// Set environment variables
	os.Setenv("PORT", "7070")
	os.Setenv("DATA_DIR", "/envdata")
	os.Setenv("TLS_ENABLED", "true")
	os.Setenv("TLS_CERT_FILE", "/env/cert.pem")
	os.Setenv("TLS_KEY_FILE", "/env/key.pem")
	os.Setenv("TLS_MTLS", "true")

	defer func() {
		os.Unsetenv("PORT")
		os.Unsetenv("DATA_DIR")
		os.Unsetenv("TLS_ENABLED")
		os.Unsetenv("TLS_CERT_FILE")
		os.Unsetenv("TLS_KEY_FILE")
		os.Unsetenv("TLS_MTLS")
	}()

	cfg := LoadFromEnv()

	if cfg.Port != "7070" {
		t.Errorf("Port should be 7070, got %s", cfg.Port)
	}
	if cfg.DataDir != "/envdata" {
		t.Errorf("Data dir should be /envdata, got %s", cfg.DataDir)
	}
	if !cfg.TLS.Enabled {
		t.Error("TLS should be enabled")
	}
	if cfg.TLS.CertFile != "/env/cert.pem" {
		t.Errorf("Cert file should be /env/cert.pem, got %s", cfg.TLS.CertFile)
	}
	if !cfg.TLS.MTLS {
		t.Error("mTLS should be enabled")
	}
}

func TestMerge(t *testing.T) {
	cfg := DefaultServerConfig()
	flags := &Flags{
		Port:        "6060",
		DataDir:     "/flagdata",
		TLSEnabled:  true,
		TLSCertFile: "/flag/cert.pem",
		TLSKeyFile:  "/flag/key.pem",
		TLSMTLS:     true,
	}

	cfg.Merge(flags)

	if cfg.Port != "6060" {
		t.Errorf("Port should be 6060, got %s", cfg.Port)
	}
	if cfg.DataDir != "/flagdata" {
		t.Errorf("Data dir should be /flagdata, got %s", cfg.DataDir)
	}
	if !cfg.TLS.Enabled {
		t.Error("TLS should be enabled")
	}
	if cfg.TLS.CertFile != "/flag/cert.pem" {
		t.Errorf("Cert file should be /flag/cert.pem, got %s", cfg.TLS.CertFile)
	}
	if !cfg.TLS.MTLS {
		t.Error("mTLS should be enabled")
	}
}

func TestValidate(t *testing.T) {
	// Test valid config
	cfg := DefaultServerConfig()
	if err := cfg.Validate(); err != nil {
		t.Errorf("Valid config should pass: %v", err)
	}

	// Test missing port
	cfg = DefaultServerConfig()
	cfg.Port = ""
	if err := cfg.Validate(); err == nil {
		t.Error("Missing port should fail validation")
	}

	// Test missing data dir
	cfg = DefaultServerConfig()
	cfg.DataDir = ""
	if err := cfg.Validate(); err == nil {
		t.Error("Missing data dir should fail validation")
	}
}

func TestResolvePaths(t *testing.T) {
	cfg := &ServerConfig{
		DataDir: "./relative/data",
		TLS: &tlspkg.Config{
			CertFile:     "./relative/cert.pem",
			KeyFile:      "/absolute/key.pem",
			ClientCAFile: "./relative/ca.pem",
		},
	}

	baseDir := "/base"
	if err := cfg.ResolvePaths(baseDir); err != nil {
		t.Fatalf("Failed to resolve paths: %v", err)
	}

	// Check that relative paths are resolved
	if cfg.DataDir == "./relative/data" {
		t.Error("Data dir should be resolved to absolute path")
	}
	if cfg.TLS.CertFile == "./relative/cert.pem" {
		t.Error("Cert file should be resolved to absolute path")
	}

	// Check that absolute paths are preserved
	if cfg.TLS.KeyFile != "/absolute/key.pem" {
		t.Error("Absolute key file path should be preserved")
	}
}

func TestParseTLSVersion(t *testing.T) {
	tests := []struct {
		input    string
		expected uint16
	}{
		{"TLS1.0", 0x0301},
		{"TLS10", 0x0301},
		{"TLS1.1", 0x0302},
		{"TLS11", 0x0302},
		{"TLS1.2", 0x0303},
		{"TLS12", 0x0303},
		{"TLS1.3", 0x0304},
		{"TLS13", 0x0304},
		{"unknown", 0x0303}, // Default to TLS 1.2
	}

	for _, tt := range tests {
		result := parseTLSVersion(tt.input)
		if result != tt.expected {
			t.Errorf("parseTLSVersion(%s) = %x, expected %x", tt.input, result, tt.expected)
		}
	}
}

func TestString(t *testing.T) {
	cfg := DefaultServerConfig()
	str := cfg.String()

	if str == "" {
		t.Error("String representation should not be empty")
	}

	// Test with TLS enabled
	cfg.TLS.Enabled = true
	str = cfg.String()
	if str == "" {
		t.Error("String representation with TLS should not be empty")
	}

	// Test with mTLS enabled
	cfg.TLS.MTLS = true
	str = cfg.String()
	if str == "" {
		t.Error("String representation with mTLS should not be empty")
	}
}

func TestLoadFromFileWithNullTLS(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config_null_tls.json")

	// Create a config file with "tls": null
	configContent := `{
		"port": "9090",
		"data_dir": "/data",
		"version": "2.0.0",
		"tls": null
	}`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg, err := LoadFromFile(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// TLS should be initialized with defaults, not nil
	if cfg.TLS == nil {
		t.Fatal("TLS config should not be nil when 'tls': null is in config file")
	}
	if cfg.TLS.Enabled {
		t.Error("TLS should be disabled by default")
	}
	if cfg.TLS.MinVersion != 0x0303 {
		t.Errorf("Default TLS version should be TLS 1.2 (0x0303), got %x", cfg.TLS.MinVersion)
	}
}

func TestMergeWithNilTLS(t *testing.T) {
	// Create a config with nil TLS
	cfg := &ServerConfig{
		Port:    "8080",
		DataDir: "./data",
		Version: "1.0.0",
		TLS:     nil,
	}

	flags := &Flags{
		TLSEnabled:  true,
		TLSCertFile: "/cert.pem",
		TLSKeyFile:  "/key.pem",
	}

	cfg.Merge(flags)

	// TLS should be initialized, not nil
	if cfg.TLS == nil {
		t.Fatal("TLS config should not be nil after Merge")
	}
	if !cfg.TLS.Enabled {
		t.Error("TLS should be enabled after Merge")
	}
	if cfg.TLS.CertFile != "/cert.pem" {
		t.Errorf("Cert file should be /cert.pem, got %s", cfg.TLS.CertFile)
	}
}

func TestValidateWithNilTLS(t *testing.T) {
	// Create a config with nil TLS
	cfg := &ServerConfig{
		Port:    "8080",
		DataDir: "./data",
		Version: "1.0.0",
		TLS:     nil,
	}

	// Should not panic
	if err := cfg.Validate(); err != nil {
		t.Errorf("Config with nil TLS should pass validation: %v", err)
	}
}

func TestResolvePathsWithNilTLS(t *testing.T) {
	// Create a config with nil TLS
	cfg := &ServerConfig{
		Port:    "8080",
		DataDir: "./data",
		Version: "1.0.0",
		TLS:     nil,
	}

	// Should not panic
	if err := cfg.ResolvePaths("/base"); err != nil {
		t.Errorf("ResolvePaths with nil TLS should not fail: %v", err)
	}
}

func TestStringWithNilTLS(t *testing.T) {
	// Create a config with nil TLS
	cfg := &ServerConfig{
		Port:    "8080",
		DataDir: "./data",
		Version: "1.0.0",
		TLS:     nil,
	}

	// Should not panic
	str := cfg.String()
	if str == "" {
		t.Error("String representation should not be empty")
	}
	expected := "ServerConfig{port=8080, dataDir=./data, tls=disabled, auth=disabled, workerHeartbeatTimeout=0s, workerOfflineThreshold=0s, workerHealthCheckInterval=0s, jobTimeout=0s, scheduleInterval=0s, timeoutCheckInterval=0s, maxJobsPerWorker=0, rateLimitEnabled=false, maxConcurrentJobsPerClient=0}"
	if str != expected {
		t.Errorf("Unexpected string representation: %s", str)
	}
}

func TestAuthTokenFromEnv(t *testing.T) {
	// Set environment variable
	os.Setenv("RFFMPEG_SERVER_TOKEN", "test-token-from-env")
	defer os.Unsetenv("RFFMPEG_SERVER_TOKEN")

	cfg := LoadFromEnv()

	if cfg.AuthToken != "test-token-from-env" {
		t.Errorf("Expected auth token 'test-token-from-env', got '%s'", cfg.AuthToken)
	}
}

func TestAuthTokenFromFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	// Create a test config file with auth_token
	configContent := `{
		"port": "9090",
		"data_dir": "/data",
		"version": "2.0.0",
		"auth_token": "test-token-from-file"
	}`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg, err := LoadFromFile(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.AuthToken != "test-token-from-file" {
		t.Errorf("Expected auth token 'test-token-from-file', got '%s'", cfg.AuthToken)
	}
}

func TestAuthTokenMerge(t *testing.T) {
	cfg := DefaultServerConfig()
	flags := &Flags{
		AuthToken: "test-token-from-flag",
	}

	cfg.Merge(flags)

	if cfg.AuthToken != "test-token-from-flag" {
		t.Errorf("Expected auth token 'test-token-from-flag', got '%s'", cfg.AuthToken)
	}
}

func TestStringWithAuthToken(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.AuthToken = "my-secret-token"

	str := cfg.String()
	if str == "" {
		t.Error("String representation should not be empty")
	}

	// Check that auth=enabled is in the string
	if !contains(str, "auth=enabled") {
		t.Errorf("String representation should contain 'auth=enabled', got: %s", str)
	}
}

// contains is a helper function to check if a string contains a substring
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestDefaultRateLimitConfig(t *testing.T) {
	cfg := DefaultServerConfig()

	if cfg.RateLimitEnabled != true {
		t.Error("RateLimitEnabled should be true by default")
	}
	if cfg.MaxConcurrentJobsPerClient != 10 {
		t.Errorf("MaxConcurrentJobsPerClient should be 10 by default, got %d", cfg.MaxConcurrentJobsPerClient)
	}
}

func TestRateLimitFromEnv(t *testing.T) {
	os.Setenv("RATE_LIMIT_ENABLED", "false")
	os.Setenv("MAX_CONCURRENT_JOBS_PER_CLIENT", "5")
	defer func() {
		os.Unsetenv("RATE_LIMIT_ENABLED")
		os.Unsetenv("MAX_CONCURRENT_JOBS_PER_CLIENT")
	}()

	cfg := LoadFromEnv()

	if cfg.RateLimitEnabled != false {
		t.Error("RateLimitEnabled should be false")
	}
	if cfg.MaxConcurrentJobsPerClient != 5 {
		t.Errorf("MaxConcurrentJobsPerClient should be 5, got %d", cfg.MaxConcurrentJobsPerClient)
	}
}

func TestRateLimitMerge(t *testing.T) {
	cfg := DefaultServerConfig()
	flags := &Flags{
		RateLimitEnabled:           true,
		MaxConcurrentJobsPerClient: 20,
	}

	cfg.Merge(flags)

	if cfg.RateLimitEnabled != true {
		t.Error("RateLimitEnabled should be true after merge")
	}
	if cfg.MaxConcurrentJobsPerClient != 20 {
		t.Errorf("MaxConcurrentJobsPerClient should be 20 after merge, got %d", cfg.MaxConcurrentJobsPerClient)
	}
}

func TestRedisEnv(t *testing.T) {
	os.Setenv("REDIS_ADDR", "localhost:6379")
	os.Setenv("REDIS_PASSWORD", "secret")
	os.Setenv("REDIS_DB", "1")
	defer func() {
		os.Unsetenv("REDIS_ADDR")
		os.Unsetenv("REDIS_PASSWORD")
		os.Unsetenv("REDIS_DB")
	}()

	cfg := LoadFromEnv()

	if cfg.RedisAddr != "localhost:6379" {
		t.Errorf("RedisAddr should be localhost:6379, got %s", cfg.RedisAddr)
	}
	if cfg.RedisPassword != "secret" {
		t.Errorf("RedisPassword should be secret, got %s", cfg.RedisPassword)
	}
	if cfg.RedisDB != 1 {
		t.Errorf("RedisDB should be 1, got %d", cfg.RedisDB)
	}
}
