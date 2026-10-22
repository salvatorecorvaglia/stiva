package config

import (
	"strings"
	"testing"
)

func TestProductionRequiresStrongSecrets(t *testing.T) {
	t.Setenv("STIVA_PRODUCTION", "true")
	cfg := Load()
	if err := cfg.Validate(); err == nil {
		t.Fatal("production accepted defaults")
	}
	cfg.AccessKey = "production-access"
	cfg.SecretKey = strings.Repeat("s", 32)
	cfg.JWTSecret = strings.Repeat("j", 32)
	cfg.MetricsToken = strings.Repeat("m", 32)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.DisableMinPartSize = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("production accepted development multipart mode")
	}
}
