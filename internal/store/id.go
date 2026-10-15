package store

import (
	"crypto/rand"
	"encoding/base64"
)

// newID returns a short, URL-safe, collision-resistant identifier suitable for
// primary keys. 16 bytes of randomness => 22 base64 chars.
func newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
