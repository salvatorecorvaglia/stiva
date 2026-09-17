package console

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestGenerateAndValidateToken(t *testing.T) {
	accessKey := "test-access-key"

	token, err := GenerateToken(accessKey)
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	// Validate it
	sub, err := ValidateToken(token)
	if err != nil {
		t.Fatalf("ValidateToken failed: %v", err)
	}
	if sub != accessKey {
		t.Errorf("expected subject %q, got %q", accessKey, sub)
	}
}

func TestValidateTokenInvalid(t *testing.T) {
	_, err := ValidateToken("invalid.token.string")
	if err == nil {
		t.Error("expected error for invalid token")
	}
}

func TestValidateTokenWrongSecret(t *testing.T) {
	// Create a token with a different secret
	claims := jwt.MapClaims{
		"sub": "test-user",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	}

	wrongSecret := []byte("wrong-secret-key-1234567890123456")
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signedToken, err := token.SignedString(wrongSecret)
	if err != nil {
		t.Fatalf("failed to sign token with wrong secret: %v", err)
	}

	_, err = ValidateToken(signedToken)
	if err == nil {
		t.Error("expected error for token signed with wrong secret")
	}
}

func TestValidateExpiredToken(t *testing.T) {
	// Create an already-expired token
	claims := jwt.MapClaims{
		"sub": "test-user",
		"iat": time.Now().Add(-2 * time.Hour).Unix(),
		"exp": time.Now().Add(-1 * time.Hour).Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signedToken, err := token.SignedString(jwtSecret)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}

	_, err = ValidateToken(signedToken)
	if err == nil {
		t.Error("expected error for expired token")
	}
}

func TestTokenUniqueness(t *testing.T) {
	// Generate multiple tokens and ensure JTIs are unique
	token1, _ := GenerateToken("user")
	token2, _ := GenerateToken("user")

	if token1 == token2 {
		t.Error("expected different tokens for same user (different JTI)")
	}
}

func TestValidateTokenWrongSigningMethod(t *testing.T) {
	// Try to validate a token with a non-HMAC signing method
	claims := jwt.MapClaims{
		"sub": "test-user",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	}

	// Use "none" method (unsigned)
	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	signedToken, _ := token.SignedString(jwt.UnsafeAllowNoneSignatureType)

	_, err := ValidateToken(signedToken)
	if err == nil {
		t.Error("expected error for token with 'none' signing method")
	}
}
