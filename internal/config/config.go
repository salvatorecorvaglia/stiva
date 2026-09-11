// Package config provides configuration management for Stiva.
// All settings are read from environment variables with sensible defaults.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for the Stiva server.
type Config struct {
	// AccessKey is the S3 access key used for authentication.
	AccessKey string
	// SecretKey is the S3 secret key used for authentication.
	SecretKey string
	// DataDir is the root directory for storing buckets and objects.
	DataDir string
	// S3Port is the port the S3 API server listens on.
	S3Port int
	// ConsolePort is the port the web console listens on.
	ConsolePort int
	// Region is the S3 region reported by the server.
	Region string
	// Domain is the base domain used for virtual host S3 bucket routing.
	Domain string
	// TLSEnabled specifies whether to serve S3 and Console APIs over HTTPS/TLS.
	TLSEnabled bool
	// TLSCert is the path to the SSL certificate file.
	TLSCert string
	// TLSKey is the path to the SSL private key file.
	TLSKey string
	// LoginRateLimit specifies the maximum requests per minute per IP for the login endpoint.
	LoginRateLimit int
	// APIRateLimit specifies the maximum requests per minute per IP for other console endpoints.
	APIRateLimit int
	// LogLevel is the log level (debug, info, warn, error).
	LogLevel string
	// LogFormat is the log format (text, json).
	LogFormat string
	// SyncEndpoint is the replication destination endpoint.
	SyncEndpoint string
	// SyncBucket is the replication destination bucket.
	SyncBucket string
	// SyncAccessKey is the replication access key.
	SyncAccessKey string
	// SyncSecretKey is the replication secret key.
	SyncSecretKey string
	// SyncRegion is the replication region.
	SyncRegion string
	// WebhookURL is the webhook notifications URL destination.
	WebhookURL string
	// WebhookSecret signs outgoing webhook payloads (HMAC-SHA256) so
	// receivers can authenticate them.
	WebhookSecret string
	// S3Endpoint is the public S3 endpoint URL for presigned links.
	S3Endpoint string
	// JWTSecret signs console session tokens. Empty means a random secret is
	// generated (and persisted) at startup instead.
	JWTSecret string
	// MetricsToken, if set, is the bearer token required to read /metrics
	// independent of a console session.
	MetricsToken string
	// MaxObjectSize caps the bytes accepted for a single object/multipart
	// part, enforced by the storage engine itself regardless of signing mode
	// (a client sending UNSIGNED-PAYLOAD bypasses the SigV4 layer's own
	// size check entirely). Zero disables the cap.
	MaxObjectSize int64
	// DisableMinPartSize turns off the S3 rule that every multipart part
	// except the last must be at least 5MB. Useful for development and
	// testing; it should not be set in production.
	DisableMinPartSize bool
	// TrustProxy specifies whether to trust proxy headers like X-Forwarded-For.
	TrustProxy bool
	// TrustedProxyHops is the number of reverse proxies in front of Stiva. The
	// client address is read that many entries from the right of
	// X-Forwarded-For, so caller-supplied entries cannot be trusted.
	TrustedProxyHops int

	// invalidEnvVars records environment variables Load() could not parse
	// (e.g. STIVA_TLS_ENABLED=enabled) so Validate() can fail startup on them
	// instead of Load() silently substituting a default — which for a
	// security-relevant flag like TLSEnabled or TrustProxy can mean the
	// operator's actual intent is silently ignored with no diagnostic beyond
	// a log line. Only ever populated by Load(); zero value is fine for
	// Config literals built directly (as tests do).
	invalidEnvVars []string
}

// RateLimitDisabledSentinel is the sentinel that turns a rate limiter off. A limit of 0
// locks callers out permanently, so disabled rate limiters must be explicit.
const RateLimitDisabledSentinel = -1
const rateLimitDisabled = RateLimitDisabledSentinel

// Validate reports configuration that would leave the server broken or
// insecure. It is called before anything binds a port.
func (c *Config) Validate() error {
	var errs []error

	if err := validatePort("STIVA_S3_PORT", c.S3Port); err != nil {
		errs = append(errs, err)
	}
	if err := validatePort("STIVA_CONSOLE_PORT", c.ConsolePort); err != nil {
		errs = append(errs, err)
	}
	if c.S3Port == c.ConsolePort {
		errs = append(errs, fmt.Errorf("STIVA_S3_PORT and STIVA_CONSOLE_PORT must differ (both are %d)", c.S3Port))
	}

	if c.AccessKey == "" {
		errs = append(errs, errors.New("STIVA_ACCESS_KEY must not be empty"))
	}
	if c.SecretKey == "" {
		errs = append(errs, errors.New("STIVA_SECRET_KEY must not be empty"))
	}
	if c.DataDir == "" {
		errs = append(errs, errors.New("STIVA_DATA_DIR must not be empty"))
	}

	// TLS needs both halves of the pair or neither; one alone silently falls
	// back to a self-signed certificate, which is rarely what was intended.
	if c.TLSEnabled && (c.TLSCert != "") != (c.TLSKey != "") {
		errs = append(errs, errors.New("STIVA_TLS_CERT and STIVA_TLS_KEY must be set together"))
	}

	if c.JWTSecret != "" && len(c.JWTSecret) < minJWTSecretLen {
		errs = append(errs, fmt.Errorf(
			"STIVA_JWT_SECRET must be at least %d characters (got %d); a short secret makes console session tokens brute-forceable",
			minJWTSecretLen, len(c.JWTSecret)))
	}

	if err := validateRateLimit("STIVA_LOGIN_RATE_LIMIT", c.LoginRateLimit); err != nil {
		errs = append(errs, err)
	}
	if err := validateRateLimit("STIVA_API_RATE_LIMIT", c.APIRateLimit); err != nil {
		errs = append(errs, err)
	}

	if c.TrustedProxyHops < 1 {
		errs = append(errs, errors.New("STIVA_TRUSTED_PROXY_HOPS must be at least 1"))
	}

	if c.MaxObjectSize < 0 {
		errs = append(errs, errors.New("STIVA_MAX_OBJECT_SIZE must not be negative (0 disables the cap)"))
	}

	for _, msg := range c.invalidEnvVars {
		errs = append(errs, errors.New(msg))
	}

	switch strings.ToLower(c.LogFormat) {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("STIVA_LOG_FORMAT must be 'text' or 'json' (got %q)", c.LogFormat))
	}

	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("STIVA_LOG_LEVEL must be one of debug, info, warn, error (got %q)", c.LogLevel))
	}

	// Replication needs a complete set of credentials to be usable at all.
	if c.SyncEndpoint != "" {
		if c.SyncBucket == "" {
			errs = append(errs, errors.New("STIVA_SYNC_BUCKET is required when STIVA_SYNC_ENDPOINT is set"))
		}
		if c.SyncAccessKey == "" || c.SyncSecretKey == "" {
			errs = append(errs, errors.New("STIVA_SYNC_ACCESS_KEY and STIVA_SYNC_SECRET_KEY are required when STIVA_SYNC_ENDPOINT is set"))
		}
	}

	return errors.Join(errs...)
}

// MinJWTSecretLen is the shortest console signing secret we accept. HS256 with
// less entropy can be cracked offline once one session token is observed.
const MinJWTSecretLen = 32
const minJWTSecretLen = MinJWTSecretLen

// EnvBool returns true if an environment variable holds a common truthy value.
func EnvBool(key string) bool {
	return envBool(key)
}

func validatePort(name string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s must be between 1 and 65535 (got %d)", name, port)
	}
	return nil
}

func validateRateLimit(name string, limit int) error {
	if limit == rateLimitDisabled || limit > 0 {
		return nil
	}
	return fmt.Errorf("%s must be positive, or %d to disable the limit (got %d)", name, rateLimitDisabled, limit)
}

// RateLimitDisabled reports whether a configured limit means "no limiting".
func RateLimitDisabled(limit int) bool {
	return limit == rateLimitDisabled
}

// Load reads configuration from environment variables, falling back to
// defaults. A value that's set but fails to parse (e.g.
// STIVA_TLS_ENABLED=enabled) is recorded rather than silently discarded, so
// Validate() can turn it into a startup error instead of the operator's
// intent being silently ignored.
func Load() *Config {
	var invalid []string

	boolVar := func(key string) bool {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			return false
		}
		switch strings.ToLower(raw) {
		case "1", "t", "true", "y", "yes", "on":
			return true
		case "0", "f", "false", "n", "no", "off":
			return false
		default:
			invalid = append(invalid, fmt.Sprintf(
				"%s=%q is not a recognized boolean (use true/false, 1/0, yes/no, or on/off)", key, raw))
			return false
		}
	}

	intVar := func(key string, defaultVal int) int {
		raw := os.Getenv(key)
		if raw == "" {
			return defaultVal
		}
		i, err := strconv.Atoi(raw)
		if err != nil {
			invalid = append(invalid, fmt.Sprintf("%s=%q is not a valid integer", key, raw))
			return defaultVal
		}
		return i
	}

	int64Var := func(key string, defaultVal int64) int64 {
		raw := os.Getenv(key)
		if raw == "" {
			return defaultVal
		}
		i, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			invalid = append(invalid, fmt.Sprintf("%s=%q is not a valid integer", key, raw))
			return defaultVal
		}
		return i
	}

	cfg := &Config{
		AccessKey:      envOrDefault("STIVA_ACCESS_KEY", "stiva"),
		SecretKey:      envOrDefault("STIVA_SECRET_KEY", "stiva123"),
		DataDir:        envOrDefault("STIVA_DATA_DIR", "/data"),
		S3Port:         intVar("STIVA_S3_PORT", 9000),
		ConsolePort:    intVar("STIVA_CONSOLE_PORT", 9001),
		Region:         envOrDefault("STIVA_REGION", "us-east-1"),
		Domain:         envOrDefault("STIVA_DOMAIN", ""),
		TLSEnabled:     boolVar("STIVA_TLS_ENABLED"),
		TLSCert:        envOrDefault("STIVA_TLS_CERT", ""),
		TLSKey:         envOrDefault("STIVA_TLS_KEY", ""),
		LoginRateLimit: intVar("STIVA_LOGIN_RATE_LIMIT", 5),
		APIRateLimit:   intVar("STIVA_API_RATE_LIMIT", 60),
		LogLevel:       envOrDefault("STIVA_LOG_LEVEL", "info"),
		LogFormat:      envOrDefault("STIVA_LOG_FORMAT", "text"),
		SyncEndpoint:   envOrDefault("STIVA_SYNC_ENDPOINT", ""),
		SyncBucket:     envOrDefault("STIVA_SYNC_BUCKET", ""),
		SyncAccessKey:  envOrDefault("STIVA_SYNC_ACCESS_KEY", ""),
		SyncSecretKey:  envOrDefault("STIVA_SYNC_SECRET_KEY", ""),
		SyncRegion:     envOrDefault("STIVA_SYNC_REGION", "us-east-1"),
		WebhookURL:     envOrDefault("STIVA_WEBHOOK_URL", ""),
		WebhookSecret:  envOrDefault("STIVA_WEBHOOK_SECRET", ""),
		S3Endpoint:     envOrDefault("STIVA_S3_ENDPOINT", ""),
		JWTSecret:      envOrDefault("STIVA_JWT_SECRET", ""),
		MetricsToken:   envOrDefault("STIVA_METRICS_TOKEN", ""),
		MaxObjectSize:  int64Var("STIVA_MAX_OBJECT_SIZE", 5*1024*1024*1024), // 5GiB, the S3 single-PUT maximum
		TrustProxy:     boolVar("STIVA_TRUST_PROXY"),

		// Read through boolVar like every other flag. This was the one
		// documented variable read directly with os.Getenv, deep in
		// CompleteMultipartUpload and compared against the exact string
		// "true" — so it was never validated and silently ignored the
		// 1/yes/on spellings accepted everywhere else.
		DisableMinPartSize: boolVar("STIVA_DISABLE_MIN_PART_SIZE"),

		TrustedProxyHops: intVar("STIVA_TRUSTED_PROXY_HOPS", 1),
	}
	cfg.invalidEnvVars = invalid
	return cfg
}

// envBool accepts the usual truthy spellings rather than only the exact string
// "true", which silently ignored values like "1", "TRUE" and "yes".
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
