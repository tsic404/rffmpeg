package config

import (
	"os"
	"testing"
	"time"
)

// TestDurationEnvInvalidLogged verifies that an unparsable duration env var
// keeps the default (TSI-2365) instead of silently producing a zero duration.
func TestDurationEnvInvalidLogged(t *testing.T) {
	os.Setenv("JOB_TIMEOUT", "not-a-duration")
	defer os.Unsetenv("JOB_TIMEOUT")

	cfg := LoadFromEnv()

	if cfg.JobTimeout != 30*time.Minute {
		t.Errorf("JOB_TIMEOUT=garbage should keep default 30m, got %v", cfg.JobTimeout)
	}
}

// TestNegativeNoWorkerTimeoutRejected verifies a negative NO_WORKER_JOB_TIMEOUT
// does not disable the starvation check silently — it falls back to the default.
func TestNegativeNoWorkerTimeoutRejected(t *testing.T) {
	os.Setenv("NO_WORKER_JOB_TIMEOUT", "-5m")
	defer os.Unsetenv("NO_WORKER_JOB_TIMEOUT")

	cfg := LoadFromEnv()

	if cfg.NoWorkerJobTimeout != 2*time.Minute {
		t.Errorf("negative NO_WORKER_JOB_TIMEOUT should keep default 2m, got %v", cfg.NoWorkerJobTimeout)
	}
}

// TestRateLimitEnabledSymmetric verifies RATE_LIMIT_ENABLED accepts enabling
// values too, not only "false"/"0" (TSI-2365).
func TestRateLimitEnabledSymmetric(t *testing.T) {
	os.Setenv("RATE_LIMIT_ENABLED", "true")
	cfg := LoadFromEnv()
	if !cfg.RateLimitEnabled {
		t.Error("RATE_LIMIT_ENABLED=true should enable rate limiting")
	}

	os.Setenv("RATE_LIMIT_ENABLED", "garbage")
	cfg = LoadFromEnv()
	if !cfg.RateLimitEnabled {
		t.Error("RATE_LIMIT_ENABLED=garbage should keep the default (enabled)")
	}
	os.Unsetenv("RATE_LIMIT_ENABLED")
}

// TestTLSEnabledSymmetric verifies TLS_ENABLED can also be disabled explicitly
// and rejects garbage (TSI-2365).
func TestTLSEnabledSymmetric(t *testing.T) {
	os.Setenv("TLS_ENABLED", "true")
	cfg := LoadFromEnv()
	if !cfg.TLS.Enabled {
		t.Error("TLS_ENABLED=true should enable TLS")
	}

	os.Setenv("TLS_ENABLED", "0")
	cfg = LoadFromEnv()
	if cfg.TLS.Enabled {
		t.Error("TLS_ENABLED=0 should disable TLS")
	}

	os.Setenv("TLS_ENABLED", "banana")
	cfg = LoadFromEnv()
	if cfg.TLS.Enabled {
		t.Error("TLS_ENABLED=banana should keep the default (disabled)")
	}
	os.Unsetenv("TLS_ENABLED")
}

// TestParseDurationOrLogRejectsNegative covers the helper directly.
func TestParseDurationOrLogRejectsNegative(t *testing.T) {
	fallback := 7 * time.Second
	if got := parseDurationOrLog("X", "-3s", fallback); got != fallback {
		t.Errorf("negative duration should return fallback %v, got %v", fallback, got)
	}
	if got := parseDurationOrLog("X", "3s", fallback); got != 3*time.Second {
		t.Errorf("valid duration should parse, got %v", got)
	}
}
