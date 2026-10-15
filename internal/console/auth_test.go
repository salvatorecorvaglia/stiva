package console

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/salvatorecorvaglia/stiva/internal/auth"
)

var testCreds = auth.NewCredentials("test-access-key", "test-secret-key")

// testJWTSecret is shared by tests that need tokens several handlers accept.
var testJWTSecret = []byte("console-test-secret-0123456789abcdef")

var testTokens = newSessionTokens(testJWTSecret)

func TestGenerateAndValidateToken(t *testing.T) {
	token, err := testTokens.generate(testCreds)
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	sub, err := testTokens.validate(token, testCreds)
	if err != nil {
		t.Fatalf("ValidateToken failed: %v", err)
	}
	if sub != testCreds.AccessKey {
		t.Errorf("expected subject %q, got %q", testCreds.AccessKey, sub)
	}
}

func TestValidateTokenInvalid(t *testing.T) {
	if _, err := testTokens.validate("invalid.token.string", testCreds); err == nil {
		t.Error("expected error for invalid token")
	}
}

// signClaims signs arbitrary claims with the console secret, for building
// tokens GenerateToken would never produce.
func signClaims(t *testing.T, method jwt.SigningMethod, key interface{}, claims jwt.MapClaims) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}
	return signed
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"sub": testCreds.AccessKey,
		"cfp": testTokens.fingerprint(testCreds),
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	}
}

func TestValidateTokenWrongSecret(t *testing.T) {
	token := signClaims(t, jwt.SigningMethodHS256, []byte("wrong-secret-key-1234567890123456"), validClaims())
	if _, err := testTokens.validate(token, testCreds); err == nil {
		t.Error("expected error for token signed with wrong secret")
	}
}

func TestValidateExpiredToken(t *testing.T) {
	claims := validClaims()
	claims["iat"] = time.Now().Add(-2 * time.Hour).Unix()
	claims["exp"] = time.Now().Add(-1 * time.Hour).Unix()
	if _, err := testTokens.validate(signClaims(t, jwt.SigningMethodHS256, testTokens.secret, claims), testCreds); err == nil {
		t.Error("expected error for expired token")
	}
}

func TestTokenUniqueness(t *testing.T) {
	token1, _ := testTokens.generate(testCreds)
	token2, _ := testTokens.generate(testCreds)
	if token1 == token2 {
		t.Error("expected different tokens for same user (different JTI)")
	}
}

func TestValidateTokenWrongSigningMethod(t *testing.T) {
	token := signClaims(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, validClaims())
	if _, err := testTokens.validate(token, testCreds); err == nil {
		t.Error("expected error for token with 'none' signing method")
	}

	// Only HS256 is issued, so only HS256 is accepted — not any HMAC variant.
	token = signClaims(t, jwt.SigningMethodHS512, testTokens.secret, validClaims())
	if _, err := testTokens.validate(token, testCreds); err == nil {
		t.Error("expected error for an HS512 token")
	}
}

// TestTokenRevokedByCredentialChange is the regression test for console
// sessions outliving the credentials that created them. A token was checked
// only for its signature and expiry, and the signing secret persists across
// restarts — so after rotating STIVA_SECRET_KEY (say, because it leaked),
// every existing session stayed valid for up to 24 hours.
func TestTokenRevokedByCredentialChange(t *testing.T) {
	token, err := testTokens.generate(testCreds)
	if err != nil {
		t.Fatal(err)
	}

	for name, rotated := range map[string]*auth.Credentials{
		"secret key rotated": auth.NewCredentials(testCreds.AccessKey, "a-new-secret"),
		"access key changed": auth.NewCredentials("another-access-key", testCreds.SecretKey),
	} {
		if _, err := testTokens.validate(token, rotated); err == nil {
			t.Errorf("%s: a session issued under the old credentials is still accepted", name)
		}
	}
}

// TestValidateTokenRequiresBindingAndExpiry: tokens missing the credential
// fingerprint or an expiry are rejected even when correctly signed.
func TestValidateTokenRequiresBindingAndExpiry(t *testing.T) {
	noFingerprint := validClaims()
	delete(noFingerprint, "cfp")
	noExpiry := validClaims()
	delete(noExpiry, "exp")

	for name, claims := range map[string]jwt.MapClaims{"no fingerprint": noFingerprint, "no expiry": noExpiry} {
		if _, err := testTokens.validate(signClaims(t, jwt.SigningMethodHS256, testTokens.secret, claims), testCreds); err == nil {
			t.Errorf("%s: token accepted", name)
		}
	}
}

// TestHandlersWithoutSecretDoNotShareSessions: the signing secret was a
// package global, so every Handler in a process shared it. Each now has its
// own unless the server hands them the same one.
func TestHandlersWithoutSecretDoNotShareSessions(t *testing.T) {
	a, b := newSessionTokens(nil), newSessionTokens(nil)
	token, err := a.generate(testCreds)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.validate(token, testCreds); err == nil {
		t.Error("a token from one issuer validated under another's random secret")
	}
	if _, err := a.validate(token, testCreds); err != nil {
		t.Errorf("issuer rejected its own token: %v", err)
	}
}
