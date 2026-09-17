// Package auth implements AWS Signature V4 authentication for the S3 API.
package auth

// Credentials holds the access key and secret key pair.
type Credentials struct {
	AccessKey string
	SecretKey string
}

// NewCredentials creates a new Credentials instance.
func NewCredentials(accessKey, secretKey string) *Credentials {
	return &Credentials{
		AccessKey: accessKey,
		SecretKey: secretKey,
	}
}

// IsValid checks if the credentials are non-empty.
func (c *Credentials) IsValid() bool {
	return c.AccessKey != "" && c.SecretKey != ""
}
