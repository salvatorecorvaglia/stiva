// Package console implements the built-in web management console for Stiva.
package console

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// jwtSecret is generated at startup for signing console session tokens.
var jwtSecret []byte

// SetJWTSecret sets the console JWT token signing key.
func SetJWTSecret(secret []byte) {
	jwtSecret = secret
}

// GenerateToken creates a JWT token for authenticated console sessions.
func GenerateToken(accessKey string) (string, error) {
	claims := jwt.MapClaims{
		"sub": accessKey,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(24 * time.Hour).Unix(),
		"jti": generateJTI(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(jwtSecret)
}

// ValidateToken validates a JWT token and returns the access key.
func ValidateToken(tokenString string) (string, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return jwtSecret, nil
	})

	if err != nil {
		return "", err
	}

	if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
		if sub, ok := claims["sub"].(string); ok {
			return sub, nil
		}
	}

	return "", fmt.Errorf("invalid token claims")
}

func generateJTI() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: this should never happen, but don't silently produce weak JTIs
		panic("failed to generate JTI: " + err.Error())
	}
	return hex.EncodeToString(b)
}
