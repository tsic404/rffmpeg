package main

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func TestCacheEnabledFlagAcceptsParseBoolAliases(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"yes", true},
		{"on", true},
		{"no", false},
		{"off", false},
		{"TRUE", true},
		{"0", false},
		{"1", true},
	}
	for _, tt := range tests {
		_, _, _, cfg := parseAndApply(t, "-cache-enabled="+tt.value)
		if cfg.CacheEnabled != tt.want {
			t.Errorf("-cache-enabled=%s: CacheEnabled = %v, want %v", tt.value, cfg.CacheEnabled, tt.want)
		}
	}
}

func TestCacheEnabledFlagRejectsUnrecognizedValue(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	registerCacheFlags(fs)
	if err := fs.Parse([]string{"-cache-enabled=maybe"}); err == nil {
		t.Fatal("parse -cache-enabled=maybe: want error, got nil")
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

// writeWorkerConfig writes a JSON worker config for resolveConfig tests.
func writeWorkerConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

// clearEnvOverrides blanks every RFFMPEG_* variable so a test observes only the
// overrides it sets itself; LoadFromEnv treats an empty value as unset, and
// t.Setenv restores the original values when the test ends.
func clearEnvOverrides(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "RFFMPEG_") {
			t.Setenv(key, "")
		}
	}
}

// applyTestFlags parses args with the worker's real flag registrations and
// applies them to cfg exactly like main does, returning the flags that took
// effect. Production registrations keep these tests honest: a renamed flag or a
// stale entry in envFlagNames fails them instead of being papered over by a
// placeholder flag.
func applyTestFlags(t *testing.T, cfg *workerconfig.Config, args ...string) map[string]bool {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	token, inputAuthHeader, serverURL := registerConnectionFlags(fs)
	cacheEnabled, cacheTTL, cacheMaxSizeMB := registerCacheFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	applied := applyConnectionFlagOverrides(token, inputAuthHeader, serverURL, cfg)
	for name := range applyCacheFlagOverrides(fs, cacheEnabled, cacheTTL, cacheMaxSizeMB, cfg) {
		applied[name] = true
	}
	return applied
}

// TestResolveConfigEnvOverridesFile warns about the default precedence: an
// inherited RFFMPEG_SERVER_URL/RFFMPEG_TOKEN displaces the config file and must
// say so instead of staying silent.
func TestResolveConfigEnvOverridesFile(t *testing.T) {
	clearEnvOverrides(t)
	path := writeWorkerConfig(t, `{"server_url":"http://file:18080","token":"file-token"}`)
	t.Setenv("RFFMPEG_SERVER_URL", "http://env:19090")
	t.Setenv("RFFMPEG_TOKEN", "env-token")

	cfg, load, err := resolveConfig(path, false)
	if err != nil {
		t.Fatalf("resolveConfig() = %v, want nil", err)
	}
	if cfg.ServerURL != "http://env:19090" {
		t.Errorf("ServerURL = %q, want env value %q", cfg.ServerURL, "http://env:19090")
	}
	if cfg.Token != "env-token" {
		t.Errorf("Token = %q, want env value %q", cfg.Token, "env-token")
	}
	notice := load.notice(applyTestFlags(t, cfg))
	for _, want := range []string{"RFFMPEG_SERVER_URL", "RFFMPEG_TOKEN", path, "-strict-config"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q does not mention %q", notice, want)
		}
	}
}

// TestResolveConfigNoticeReportsOnlyAppliedOverrides pins the false positives
// the notice must not report: a variable set to the config file's own value
// changed nothing, and a variable whose flag applied a value never decided the
// result.
func TestResolveConfigNoticeReportsOnlyAppliedOverrides(t *testing.T) {
	clearEnvOverrides(t)
	path := writeWorkerConfig(t, `{"server_url":"http://file:18080","token":"file-token","max_concurrent":4}`)

	t.Run("value equal to the config file", func(t *testing.T) {
		t.Setenv("RFFMPEG_SERVER_URL", "http://file:18080")
		t.Setenv("RFFMPEG_MAX_CONCURRENT", "4")

		cfg, load, err := resolveConfig(path, false)
		if err != nil {
			t.Fatalf("resolveConfig() = %v, want nil", err)
		}
		if notice := load.notice(applyTestFlags(t, cfg)); notice != "" {
			t.Errorf("notice = %q, want none (neither variable changed a value)", notice)
		}
	})

	t.Run("flag replaces the variable", func(t *testing.T) {
		t.Setenv("RFFMPEG_SERVER_URL", "http://env:19090")
		t.Setenv("RFFMPEG_TOKEN", "env-token")

		cfg, load, err := resolveConfig(path, false)
		if err != nil {
			t.Fatalf("resolveConfig() = %v, want nil", err)
		}
		notice := load.notice(applyTestFlags(t, cfg, "-server-url=http://flag:18097"))
		if strings.Contains(notice, "RFFMPEG_SERVER_URL") {
			t.Errorf("notice %q blames RFFMPEG_SERVER_URL, want only what the flag did not replace", notice)
		}
		if !strings.Contains(notice, "RFFMPEG_TOKEN") {
			t.Errorf("notice %q does not mention RFFMPEG_TOKEN", notice)
		}
	})

	t.Run("empty flag value does not replace the variable", func(t *testing.T) {
		t.Setenv("RFFMPEG_SERVER_URL", "http://env:19090")

		cfg, load, err := resolveConfig(path, false)
		if err != nil {
			t.Fatalf("resolveConfig() = %v, want nil", err)
		}
		if cfg.ServerURL != "http://env:19090" {
			t.Fatalf("ServerURL = %q, want the env value: an empty flag must not write it", cfg.ServerURL)
		}
		// `-server-url "$OVERRIDE"` with an empty variable is the silent-override
		// trap this notice exists to break, so the variable must stay named.
		notice := load.notice(applyTestFlags(t, cfg, "-server-url="))
		if !strings.Contains(notice, "RFFMPEG_SERVER_URL") {
			t.Errorf("notice = %q, want it to name RFFMPEG_SERVER_URL", notice)
		}
	})
}

// TestEnvFlagNamesExclusions drives every envFlagNames entry end to end: once
// the mapped flag applies a value the variable must drop out of the notice, so a
// renamed flag or a mistyped entry fails here instead of regressing silently.
func TestEnvFlagNamesExclusions(t *testing.T) {
	clearEnvOverrides(t)
	path := writeWorkerConfig(t, `{"server_url":"http://file:18080","token":"file-token","input_auth_header":"","cache_enabled":true,"cache_ttl":"1h","cache_max_size_mb":10}`)

	envValues := map[string]string{
		"RFFMPEG_SERVER_URL":        "http://env:19090",
		"RFFMPEG_TOKEN":             "env-token",
		"RFFMPEG_INPUT_AUTH_HEADER": "Bearer env-token",
		"RFFMPEG_CACHE_ENABLED":     "false",
		"RFFMPEG_CACHE_TTL":         "3h",
		"RFFMPEG_CACHE_MAX_SIZE_MB": "43",
	}
	flagArgs := map[string]string{
		"RFFMPEG_SERVER_URL":        "-server-url=http://flag:18097",
		"RFFMPEG_TOKEN":             "-token=flag-token",
		"RFFMPEG_INPUT_AUTH_HEADER": "-input-auth-header=Bearer flag-token",
		"RFFMPEG_CACHE_ENABLED":     "-cache-enabled=true",
		"RFFMPEG_CACHE_TTL":         "-cache-ttl=2h",
		"RFFMPEG_CACHE_MAX_SIZE_MB": "-cache-max-size-mb=42",
	}
	for env, value := range envValues {
		t.Setenv(env, value)
	}

	cfg, load, err := resolveConfig(path, false)
	if err != nil {
		t.Fatalf("resolveConfig() = %v, want nil", err)
	}

	for env, arg := range flagArgs {
		t.Run(env, func(t *testing.T) {
			// applyTestFlags mutates cfg as main does; only the returned set
			// feeds the notice, so sharing cfg across subtests is safe.
			notice := load.notice(applyTestFlags(t, cfg, arg))
			if strings.Contains(notice, env) {
				t.Errorf("notice = %q, want no mention of %s: its flag applied a value", notice, env)
			}
		})
	}
}

// TestResolveConfigStrictKeepsFileValues covers -strict-config: every
// RFFMPEG_* override is dropped, not just the connection settings.
func TestResolveConfigStrictKeepsFileValues(t *testing.T) {
	clearEnvOverrides(t)
	path := writeWorkerConfig(t, `{"server_url":"http://file:18080","token":"file-token","max_concurrent":3}`)
	t.Setenv("RFFMPEG_SERVER_URL", "http://env:19090")
	t.Setenv("RFFMPEG_TOKEN", "env-token")
	t.Setenv("RFFMPEG_MAX_CONCURRENT", "8")

	cfg, load, err := resolveConfig(path, true)
	if err != nil {
		t.Fatalf("resolveConfig() = %v, want nil", err)
	}
	if cfg.ServerURL != "http://file:18080" {
		t.Errorf("ServerURL = %q, want file value %q", cfg.ServerURL, "http://file:18080")
	}
	if cfg.Token != "file-token" {
		t.Errorf("Token = %q, want file value %q", cfg.Token, "file-token")
	}
	if cfg.MaxConcurrent != 3 {
		t.Errorf("MaxConcurrent = %d, want file value 3", cfg.MaxConcurrent)
	}

	notice := load.notice(applyTestFlags(t, cfg))
	for _, want := range []string{"authoritative", "RFFMPEG_SERVER_URL", "RFFMPEG_TOKEN", "RFFMPEG_MAX_CONCURRENT"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q does not mention %q", notice, want)
		}
	}
	// A flag still outranks the config file, so the notice must not claim it
	// silenced a variable that flag replaced.
	flagNotice := load.notice(applyTestFlags(t, cfg, "-server-url=http://flag:18097"))
	if strings.Contains(flagNotice, "RFFMPEG_SERVER_URL") {
		t.Errorf("notice %q blames RFFMPEG_SERVER_URL, want only what the flag did not replace", flagNotice)
	}
}

// TestResolveConfigStrictWithoutEnvOverrideStaysQuiet pins that strict mode
// alone is not noise: with nothing to silence, only the mode itself is reported.
func TestResolveConfigStrictWithoutEnvOverrideStaysQuiet(t *testing.T) {
	clearEnvOverrides(t)
	path := writeWorkerConfig(t, `{"server_url":"http://file:18080"}`)

	cfg, load, err := resolveConfig(path, true)
	if err != nil {
		t.Fatalf("resolveConfig() = %v, want nil", err)
	}
	if cfg.ServerURL != "http://file:18080" {
		t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, "http://file:18080")
	}
	if notice := load.notice(applyTestFlags(t, cfg)); !strings.Contains(notice, path) || !strings.Contains(notice, "Strict config mode") {
		t.Errorf("notice = %q, want the strict-mode notice naming %q", notice, path)
	}

	// Without -strict-config a config file that nothing overrides logs nothing.
	plainCfg, plain, err := resolveConfig(path, false)
	if err != nil {
		t.Fatalf("resolveConfig() = %v, want nil", err)
	}
	if notice := plain.notice(applyTestFlags(t, plainCfg)); notice != "" {
		t.Errorf("notice = %q, want none when the environment overrides nothing", notice)
	}
}

// TestResolveConfigStrictRequiresConfigFile rejects the contradictory request
// instead of silently falling back to environment-only config.
func TestResolveConfigStrictRequiresConfigFile(t *testing.T) {
	clearEnvOverrides(t)
	t.Setenv("RFFMPEG_SERVER_URL", "http://env:19090")

	if _, _, err := resolveConfig("", true); err == nil {
		t.Fatal("resolveConfig(strict, no file) = nil, want error")
	}
	cfg, load, err := resolveConfig("", false)
	if err != nil {
		t.Fatalf("resolveConfig(env only) = %v, want nil", err)
	}
	if cfg.ServerURL != "http://env:19090" {
		t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, "http://env:19090")
	}
	if notice := load.notice(applyTestFlags(t, cfg)); notice != "" {
		t.Errorf("notice = %q, want none for an environment-only config", notice)
	}
}

// TestResolveConfigUnreadableFile covers both readings of a broken -config
// path: strict aborts, the default keeps the environment as a last resort.
func TestResolveConfigUnreadableFile(t *testing.T) {
	clearEnvOverrides(t)
	missing := filepath.Join(t.TempDir(), "missing.json")
	t.Setenv("RFFMPEG_SERVER_URL", "http://env:19090")

	if _, _, err := resolveConfig(missing, true); err == nil {
		t.Fatal("resolveConfig(strict, missing file) = nil, want error")
	}

	cfg, load, err := resolveConfig(missing, false)
	if err != nil {
		t.Fatalf("resolveConfig(missing file) = %v, want nil", err)
	}
	if cfg.ServerURL != "http://env:19090" {
		t.Errorf("ServerURL = %q, want env fallback %q", cfg.ServerURL, "http://env:19090")
	}
	if notice := load.notice(applyTestFlags(t, cfg)); !strings.Contains(notice, "Failed to load config file") {
		t.Errorf("notice = %q, want the load-failure warning", notice)
	}
}

// TestEnvFlagNamesExistInWorkerconfig guards the notice mapping against drift:
// every variable it maps must be one workerconfig actually applies.
func TestEnvFlagNamesExistInWorkerconfig(t *testing.T) {
	values := map[string]string{
		"RFFMPEG_SERVER_URL":        "http://env:19090",
		"RFFMPEG_TOKEN":             "env-token",
		"RFFMPEG_INPUT_AUTH_HEADER": "Bearer env-token",
		"RFFMPEG_CACHE_ENABLED":     "false",
		"RFFMPEG_CACHE_TTL":         "2h",
		"RFFMPEG_CACHE_MAX_SIZE_MB": "42",
	}
	for env, value := range values {
		t.Setenv(env, value)
	}

	_, applied := workerconfig.MergeAndReport(workerconfig.DefaultConfig(), workerconfig.LoadFromEnv())

	for env := range envFlagNames {
		if !slices.Contains(applied, env) {
			t.Errorf("applied = %v, want it to contain %q", applied, env)
		}
	}
}
