package main

import (
	"flag"
	"testing"
	"time"

	"github.com/tsic404/rffmpeg/pkg/worker/workerconfig"
)

// parseAndApply registers the cache flags, parses args, and applies overrides
// onto a fresh config, returning the flag values alongside the result.
func parseAndApply(t *testing.T, args ...string) (*bool, *time.Duration, *int64, *workerconfig.Config) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	enabled, ttl, maxSize := registerCacheFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	cfg := workerconfig.DefaultConfig()
	applyCacheFlagOverrides(fs, enabled, ttl, maxSize, cfg)
	return enabled, ttl, maxSize, cfg
}

func TestCacheFlagsDefaultToConfigValues(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	enabled, ttl, maxSize := registerCacheFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}

	cfg := &workerconfig.Config{
		CacheEnabled:   false,
		CacheTTL:       workerconfig.Duration(time.Hour),
		CacheMaxSizeMB: 42,
	}
	applyCacheFlagOverrides(fs, enabled, ttl, maxSize, cfg)

	// Absent flags must not clobber values loaded from config file/env,
	// including the -cache-enabled true default.
	if cfg.CacheEnabled {
		t.Errorf("CacheEnabled = true, want false (flag default must not override)")
	}
	if cfg.CacheTTL.ToDuration() != time.Hour {
		t.Errorf("CacheTTL = %v, want 1h", cfg.CacheTTL.ToDuration())
	}
	if cfg.CacheMaxSizeMB != 42 {
		t.Errorf("CacheMaxSizeMB = %d, want 42", cfg.CacheMaxSizeMB)
	}
}

func TestCacheFlagsOverrideConfig(t *testing.T) {
	_, _, _, cfg := parseAndApply(t, "-cache-enabled=false", "-cache-ttl=2h", "-cache-max-size-mb=500")

	if cfg.CacheEnabled {
		t.Errorf("CacheEnabled = true, want false")
	}
	if cfg.CacheTTL.ToDuration() != 2*time.Hour {
		t.Errorf("CacheTTL = %v, want 2h", cfg.CacheTTL.ToDuration())
	}
	if cfg.CacheMaxSizeMB != 500 {
		t.Errorf("CacheMaxSizeMB = %d, want 500", cfg.CacheMaxSizeMB)
	}
}

func TestCacheFlagBareBoolEnables(t *testing.T) {
	cfg := &workerconfig.Config{CacheEnabled: false}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	enabled, ttl, maxSize := registerCacheFlags(fs)
	if err := fs.Parse([]string{"-cache-enabled"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	applyCacheFlagOverrides(fs, enabled, ttl, maxSize, cfg)

	if !cfg.CacheEnabled {
		t.Errorf("CacheEnabled = false, want true (bare -cache-enabled enables)")
	}
}

func TestCacheFlagPartialOverride(t *testing.T) {
	cfg := &workerconfig.Config{
		CacheEnabled:   true,
		CacheTTL:       workerconfig.Duration(time.Hour),
		CacheMaxSizeMB: 42,
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	enabled, ttl, maxSize := registerCacheFlags(fs)
	if err := fs.Parse([]string{"-cache-max-size-mb=500"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	applyCacheFlagOverrides(fs, enabled, ttl, maxSize, cfg)

	// Only the explicitly-set flag changes; the others keep their values.
	if !cfg.CacheEnabled {
		t.Errorf("CacheEnabled = false, want true")
	}
	if cfg.CacheTTL.ToDuration() != time.Hour {
		t.Errorf("CacheTTL = %v, want 1h", cfg.CacheTTL.ToDuration())
	}
	if cfg.CacheMaxSizeMB != 500 {
		t.Errorf("CacheMaxSizeMB = %d, want 500", cfg.CacheMaxSizeMB)
	}
}

func TestCacheFlagInvalidValueFailsValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"negative ttl", []string{"-cache-ttl=-1h"}},
		{"negative max size", []string{"-cache-max-size-mb=-500"}},
		{"max size int64 overflow", []string{"-cache-max-size-mb=9223372036854775807"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			enabled, ttl, maxSize := registerCacheFlags(fs)
			if err := fs.Parse(tt.args); err != nil {
				t.Fatalf("parse %v: %v", tt.args, err)
			}
			cfg := workerconfig.DefaultConfig()
			applyCacheFlagOverrides(fs, enabled, ttl, maxSize, cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want error for %v", tt.args)
			}
		})
	}
}
