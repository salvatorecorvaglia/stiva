package config

import (
	"strings"
	"testing"
)

func TestConfigLoadDefaults(t *testing.T) {
	// Clear any overrides
	t.Setenv("STIVA_ACCESS_KEY", "")
	t.Setenv("STIVA_SECRET_KEY", "")
	t.Setenv("STIVA_S3_PORT", "")
	t.Setenv("STIVA_TLS_ENABLED", "")

	cfg := Load()

	if cfg.AccessKey != "stiva" {
		t.Errorf("expected default access key 'stiva', got '%s'", cfg.AccessKey)
	}
	if cfg.SecretKey != "stiva123" {
		t.Errorf("expected default secret key 'stiva123', got '%s'", cfg.SecretKey)
	}
	if cfg.S3Port != 9000 {
		t.Errorf("expected default S3 port 9000, got %d", cfg.S3Port)
	}
	if cfg.ConsolePort != 9001 {
		t.Errorf("expected default console port 9001, got %d", cfg.ConsolePort)
	}
	if cfg.TLSEnabled {
		t.Errorf("expected TLS to be disabled by default")
	}
}

func TestConfigEnvOverrides(t *testing.T) {
	t.Setenv("STIVA_ACCESS_KEY", "customaccess")
	t.Setenv("STIVA_SECRET_KEY", "customsecret")
	t.Setenv("STIVA_S3_PORT", "9900")
	t.Setenv("STIVA_CONSOLE_PORT", "9901")
	t.Setenv("STIVA_TLS_ENABLED", "true")
	t.Setenv("STIVA_LOG_LEVEL", "debug")

	cfg := Load()

	if cfg.AccessKey != "customaccess" {
		t.Errorf("expected access key 'customaccess', got '%s'", cfg.AccessKey)
	}
	if cfg.SecretKey != "customsecret" {
		t.Errorf("expected secret key 'customsecret', got '%s'", cfg.SecretKey)
	}
	if cfg.S3Port != 9900 {
		t.Errorf("expected S3 port 9900, got %d", cfg.S3Port)
	}
	if cfg.ConsolePort != 9901 {
		t.Errorf("expected console port 9901, got %d", cfg.ConsolePort)
	}
	if !cfg.TLSEnabled {
		t.Errorf("expected TLS to be enabled")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("expected log level 'debug', got '%s'", cfg.LogLevel)
	}
}

// TestLoadThenValidateRejectsMalformedBoolEnvVar guards against a gap where
// an unparsable boolean env var (e.g. a typo like "enabled" instead of
// "true") was silently treated as false by Load(), with no diagnostic beyond
// a log line — for a security-relevant flag like STIVA_TLS_ENABLED, that
// meant the operator's actual intent (serve over TLS) was silently ignored
// and the server came up serving plaintext HTTP instead.
func TestLoadThenValidateRejectsMalformedBoolEnvVar(t *testing.T) {
	t.Setenv("STIVA_ACCESS_KEY", "ak")
	t.Setenv("STIVA_SECRET_KEY", "sk")
	t.Setenv("STIVA_TLS_ENABLED", "enabled")

	cfg := Load()
	if cfg.TLSEnabled {
		t.Fatal("malformed value should not parse as true")
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Load() then Validate() must reject an unparsable STIVA_TLS_ENABLED value instead of silently defaulting to false")
	}
	if !strings.Contains(err.Error(), "STIVA_TLS_ENABLED") {
		t.Errorf("error should name STIVA_TLS_ENABLED, got: %v", err)
	}
}

// TestLoadThenValidateRejectsMalformedIntEnvVar mirrors the boolean case for
// integer env vars: an unparsable STIVA_S3_PORT used to silently fall back
// to the default port with no diagnostic beyond a log line.
func TestLoadThenValidateRejectsMalformedIntEnvVar(t *testing.T) {
	t.Setenv("STIVA_ACCESS_KEY", "ak")
	t.Setenv("STIVA_SECRET_KEY", "sk")
	t.Setenv("STIVA_S3_PORT", "not-a-number")

	cfg := Load()
	if cfg.S3Port != 9000 {
		t.Fatalf("expected fallback to default port 9000, got %d", cfg.S3Port)
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Load() then Validate() must reject an unparsable STIVA_S3_PORT value instead of silently defaulting")
	}
	if !strings.Contains(err.Error(), "STIVA_S3_PORT") {
		t.Errorf("error should name STIVA_S3_PORT, got: %v", err)
	}
}
