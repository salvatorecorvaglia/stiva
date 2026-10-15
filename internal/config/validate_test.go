package config

import (
	"strings"
	"testing"
	"time"
)

func validConfig() *Config {
	return &Config{
		AccessKey:        "ak",
		SecretKey:        "sk",
		DataDir:          "/data",
		S3Port:           9000,
		ConsolePort:      9001,
		Region:           "us-east-1",
		LoginRateLimit:   5,
		APIRateLimit:     60,
		LogLevel:         "info",
		LogFormat:        "text",
		TrustedProxyHops: 1,
		MultipartExpiry:  defaultMultipartExpiry,
	}
}

func TestValidateAcceptsDefaults(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("default configuration rejected: %v", err)
	}
}

// TestValidateRejectsZeroRateLimit is the regression test for the audit finding
// that STIVA_LOGIN_RATE_LIMIT=0 produced a limiter with zero tokens and zero
// refill, permanently locking every operator out of the console. Zero is now a
// startup error, and -1 is the explicit way to disable limiting.
func TestValidateRejectsZeroRateLimit(t *testing.T) {
	cfg := validConfig()
	cfg.LoginRateLimit = 0

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a login rate limit of 0 must be rejected, not silently lock out the console")
	}
	if !strings.Contains(err.Error(), "STIVA_LOGIN_RATE_LIMIT") {
		t.Errorf("error should name the offending variable, got: %v", err)
	}
}

func TestRateLimitDisabledSentinel(t *testing.T) {
	cfg := validConfig()
	cfg.LoginRateLimit = rateLimitDisabled
	cfg.APIRateLimit = rateLimitDisabled

	if err := cfg.Validate(); err != nil {
		t.Fatalf("the disable sentinel must be accepted: %v", err)
	}
	if !RateLimitDisabled(rateLimitDisabled) {
		t.Error("RateLimitDisabled should report true for the sentinel")
	}
	if RateLimitDisabled(5) {
		t.Error("RateLimitDisabled should report false for a real limit")
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"port too low", func(c *Config) { c.S3Port = 0 }, "STIVA_S3_PORT"},
		{"port too high", func(c *Config) { c.ConsolePort = 70000 }, "STIVA_CONSOLE_PORT"},
		{"ports collide", func(c *Config) { c.ConsolePort = c.S3Port }, "must differ"},
		{"empty access key", func(c *Config) { c.AccessKey = "" }, "STIVA_ACCESS_KEY"},
		{"empty secret key", func(c *Config) { c.SecretKey = "" }, "STIVA_SECRET_KEY"},
		{"empty data dir", func(c *Config) { c.DataDir = "" }, "STIVA_DATA_DIR"},
		{"bad log level", func(c *Config) { c.LogLevel = "verbose" }, "STIVA_LOG_LEVEL"},
		{"bad log format", func(c *Config) { c.LogFormat = "yaml" }, "STIVA_LOG_FORMAT"},
		{"zero proxy hops", func(c *Config) { c.TrustedProxyHops = 0 }, "STIVA_TRUSTED_PROXY_HOPS"},
		{"negative max object size", func(c *Config) { c.MaxObjectSize = -1 }, "STIVA_MAX_OBJECT_SIZE"},
		{
			"tls cert without key",
			func(c *Config) { c.TLSEnabled = true; c.TLSCert = "/tmp/c.pem" },
			"must be set together",
		},
		{
			"tls key without cert",
			func(c *Config) { c.TLSEnabled = true; c.TLSKey = "/tmp/k.pem" },
			"must be set together",
		},
		{
			"sync endpoint without bucket",
			func(c *Config) {
				c.SyncEndpoint = "http://backup:9000"
				c.SyncAccessKey = "a"
				c.SyncSecretKey = "b"
			},
			"STIVA_SYNC_BUCKET",
		},
		{
			"sync endpoint without credentials",
			func(c *Config) {
				c.SyncEndpoint = "http://backup:9000"
				c.SyncBucket = "mirror"
			},
			"STIVA_SYNC_ACCESS_KEY",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected %s to be rejected", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q should mention %q", err, tc.wantSub)
			}
		})
	}
}

// TestValidateRejectsShortJWTSecret guards the console signing key: HS256 with a
// short secret can be brute-forced offline from one captured token.
func TestValidateRejectsShortJWTSecret(t *testing.T) {
	cfg := validConfig()
	cfg.JWTSecret = "short"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a short STIVA_JWT_SECRET must be rejected")
	}
	if !strings.Contains(err.Error(), "STIVA_JWT_SECRET") {
		t.Errorf("error should name STIVA_JWT_SECRET, got: %v", err)
	}
}

func TestValidateAcceptsLongJWTSecret(t *testing.T) {
	cfg := validConfig()
	cfg.JWTSecret = strings.Repeat("x", minJWTSecretLen)

	if err := cfg.Validate(); err != nil {
		t.Fatalf("a sufficiently long secret must be accepted: %v", err)
	}
}

// TestEnvBoolAcceptsCommonSpellings covers the finding that only the exact
// string "true" enabled boolean flags, silently ignoring "1", "TRUE" and "yes".
// TestBoolFlagsAcceptCommonSpellings covers the boolean parsing Load applies
// to every flag (checked here through STIVA_TRUST_PROXY). Unrecognised
// spellings are startup errors; see TestDisableMinPartSizeRejectsGarbage.
func TestBoolFlagsAcceptCommonSpellings(t *testing.T) {
	truthy := []string{"1", "t", "true", "TRUE", "True", "y", "yes", "YES", "on", " true "}
	for _, v := range truthy {
		t.Setenv("STIVA_TRUST_PROXY", v)
		if !Load().TrustProxy {
			t.Errorf("STIVA_TRUST_PROXY=%q parsed as false, want true", v)
		}
	}

	falsy := []string{"", "0", "f", "false", "n", "no", "off"}
	for _, v := range falsy {
		t.Setenv("STIVA_TRUST_PROXY", v)
		if Load().TrustProxy {
			t.Errorf("STIVA_TRUST_PROXY=%q parsed as true, want false", v)
		}
	}
}

// TestDisableMinPartSizeReadsCommonSpellings covers the one documented
// environment variable that bypassed Load entirely: it was read with a
// bare os.Getenv deep inside CompleteMultipartUpload and compared against the
// exact string "true", so it was never validated and silently ignored the
// 1/yes/on spellings every other flag accepts.
func TestDisableMinPartSizeReadsCommonSpellings(t *testing.T) {
	for _, v := range []string{"1", "t", "true", "TRUE", "y", "yes", "on", "On"} {
		t.Setenv("STIVA_DISABLE_MIN_PART_SIZE", v)
		if !Load().DisableMinPartSize {
			t.Errorf("STIVA_DISABLE_MIN_PART_SIZE=%q should enable the flag", v)
		}
	}
	for _, v := range []string{"0", "f", "false", "no", "off", ""} {
		t.Setenv("STIVA_DISABLE_MIN_PART_SIZE", v)
		if Load().DisableMinPartSize {
			t.Errorf("STIVA_DISABLE_MIN_PART_SIZE=%q should leave the flag off", v)
		}
	}
}

// TestDisableMinPartSizeRejectsGarbage confirms the flag now participates in
// startup validation like every other boolean, rather than quietly defaulting.
func TestDisableMinPartSizeRejectsGarbage(t *testing.T) {
	t.Setenv("STIVA_DISABLE_MIN_PART_SIZE", "enabled")
	cfg := Load()
	if err := cfg.Validate(); err == nil {
		t.Error("STIVA_DISABLE_MIN_PART_SIZE=enabled should fail validation")
	} else if !strings.Contains(err.Error(), "STIVA_DISABLE_MIN_PART_SIZE") {
		t.Errorf("error should name the offending variable, got: %v", err)
	}
}

// TestMultipartExpiryParsing covers STIVA_MULTIPART_EXPIRY. Incomplete
// multipart uploads used to be aborted after a hardcoded 24 hours, which
// killed slow uploads of very large objects partway through.
func TestMultipartExpiryParsing(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", defaultMultipartExpiry},
		{"36h", 36 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"2d12h", 60 * time.Hour},
	}
	for _, tc := range cases {
		t.Setenv("STIVA_MULTIPART_EXPIRY", tc.raw)
		cfg := Load()
		if cfg.MultipartExpiry != tc.want {
			t.Errorf("STIVA_MULTIPART_EXPIRY=%q parsed as %v, want %v", tc.raw, cfg.MultipartExpiry, tc.want)
		}
		if err := cfg.Validate(); err != nil && strings.Contains(err.Error(), "STIVA_MULTIPART_EXPIRY") {
			t.Errorf("STIVA_MULTIPART_EXPIRY=%q rejected: %v", tc.raw, err)
		}
	}
}

func TestMultipartExpiryRejectsBadValues(t *testing.T) {
	for _, raw := range []string{"soon", "-2h", "30m", "0"} {
		t.Setenv("STIVA_MULTIPART_EXPIRY", raw)
		err := Load().Validate()
		if err == nil || !strings.Contains(err.Error(), "STIVA_MULTIPART_EXPIRY") {
			t.Errorf("STIVA_MULTIPART_EXPIRY=%q: err = %v, want a startup error naming the variable", raw, err)
		}
	}
}

// TestConsoleAllowedOrigins covers STIVA_CONSOLE_ALLOWED_ORIGINS: a
// comma-separated list of exact origins (scheme://host[:port]) the console
// answers besides its own.
func TestConsoleAllowedOrigins(t *testing.T) {
	t.Setenv("STIVA_CONSOLE_ALLOWED_ORIGINS", " https://Admin.Example.com , http://localhost:5173/ ")
	cfg := Load()
	want := []string{"https://admin.example.com", "http://localhost:5173"}
	if strings.Join(cfg.ConsoleAllowedOrigins, ",") != strings.Join(want, ",") {
		t.Errorf("parsed %q, want %q", cfg.ConsoleAllowedOrigins, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid origins rejected: %v", err)
	}

	for _, raw := range []string{"not-an-origin", "https://x.example.com/path", "ftp://x.example.com", "https://", "*"} {
		t.Setenv("STIVA_CONSOLE_ALLOWED_ORIGINS", raw)
		err := Load().Validate()
		if err == nil || !strings.Contains(err.Error(), "STIVA_CONSOLE_ALLOWED_ORIGINS") {
			t.Errorf("STIVA_CONSOLE_ALLOWED_ORIGINS=%q: err = %v, want a startup error naming the variable", raw, err)
		}
	}
}
