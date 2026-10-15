// Package console implements the built-in web management console for Stiva.
package console

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/salvatorecorvaglia/stiva/internal/auth"
)

// sessionTokens issues and validates console session tokens (HS256 JWTs).
//
// The signing secret used to be a package-level variable set once by the
// server, which made every Handler share hidden global state. Each Handler now
// owns its issuer, built from Options.JWTSecret.
type sessionTokens struct {
	secret []byte
}

// newSessionTokens returns an issuer for secret, or for a fresh random secret
// when none is given (sessions then last only as long as the process).
func newSessionTokens(secret []byte) sessionTokens {
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			panic("failed to generate console session secret: " + err.Error())
		}
	}
	return sessionTokens{secret: secret}
}

// credentialFingerprintClaim binds a session token to the credentials that
// were valid when it was issued.
const credentialFingerprintClaim = "cfp"

// fingerprint derives a value that changes whenever the access or secret key
// does. It is an HMAC under the signing secret, so the token (whose claims
// anyone holding it can read) reveals nothing usable about the secret key.
func (s sessionTokens) fingerprint(creds *auth.Credentials) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte("stiva-console-credentials\x00" + creds.AccessKey + "\x00" + creds.SecretKey))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// generate creates a token for an authenticated console session.
func (s sessionTokens) generate(creds *auth.Credentials) (string, error) {
	claims := jwt.MapClaims{
		"sub":                      creds.AccessKey,
		credentialFingerprintClaim: s.fingerprint(creds),
		"iat":                      time.Now().Unix(),
		"exp":                      time.Now().Add(24 * time.Hour).Unix(),
		"jti":                      generateJTI(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}

// validate checks a session token against the current credentials and
// returns its access key.
//
// Checking only the signature and expiry, as this used to, meant a session
// outlived a credential rotation for as long as the signing secret persisted
// (which it does, across restarts): changing a leaked secret key left every
// session opened with it working for up to a day.
func (s sessionTokens) validate(tokenString string, creds *auth.Credentials) (string, error) {
	token, err := jwt.Parse(tokenString,
		func(*jwt.Token) (interface{}, error) { return s.secret, nil },
		// Only what generate issues is accepted: HS256, with an expiry.
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return "", err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return "", fmt.Errorf("invalid token claims")
	}
	if _, isOther := claims["typ"]; isOther {
		return "", fmt.Errorf("not a session token") // e.g. a download link
	}
	sub, _ := claims["sub"].(string)
	fp, _ := claims[credentialFingerprintClaim].(string)
	if sub != creds.AccessKey || !hmac.Equal([]byte(fp), []byte(s.fingerprint(creds))) {
		return "", fmt.Errorf("token was issued for different credentials")
	}
	return sub, nil
}

// downloadLinkTTL is how long a download link stays usable. It only has to
// outlive the browser starting the download it was issued for.
const downloadLinkTTL = time.Minute

// downloadTokenType marks a download-link token, so one can never pass as a
// session token or the other way round.
const downloadTokenType = "download"

// downloadClaims identify what a download link grants.
type downloadClaims struct {
	Bucket string
	Key    string
	ID     string // single-use identifier (jti)
	Expiry time.Time
}

// generateDownload issues a link token for one object, valid for
// downloadLinkTTL from now and bound to the current credentials like a
// session.
func (s sessionTokens) generateDownload(creds *auth.Credentials, bucket, key string, now time.Time) (string, error) {
	claims := jwt.MapClaims{
		"typ":                      downloadTokenType,
		"b":                        bucket,
		"k":                        key,
		credentialFingerprintClaim: s.fingerprint(creds),
		"iat":                      now.Unix(),
		"exp":                      now.Add(downloadLinkTTL).Unix(),
		"jti":                      generateJTI(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}

// validateDownload checks a download-link token and returns what it grants.
// Single use is enforced by the caller, which remembers spent IDs.
func (s sessionTokens) validateDownload(tokenString string, creds *auth.Credentials) (*downloadClaims, error) {
	token, err := jwt.Parse(tokenString,
		func(*jwt.Token) (interface{}, error) { return s.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid || claims["typ"] != downloadTokenType {
		return nil, fmt.Errorf("not a download link")
	}
	fp, _ := claims[credentialFingerprintClaim].(string)
	if !hmac.Equal([]byte(fp), []byte(s.fingerprint(creds))) {
		return nil, fmt.Errorf("link was issued for different credentials")
	}
	bucket, _ := claims["b"].(string)
	key, _ := claims["k"].(string)
	id, _ := claims["jti"].(string)
	exp, err := claims.GetExpirationTime()
	if err != nil || bucket == "" || key == "" || id == "" {
		return nil, fmt.Errorf("malformed download link")
	}
	return &downloadClaims{Bucket: bucket, Key: key, ID: id, Expiry: exp.Time}, nil
}

func generateJTI() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: this should never happen, but don't silently produce weak JTIs
		panic("failed to generate JTI: " + err.Error())
	}
	return hex.EncodeToString(b)
}
