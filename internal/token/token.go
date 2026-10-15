// Package token generates and verifies the high-entropy bearer tokens used to
// authenticate agents and beacons against the coordinator.
//
// Tokens are random 256-bit secrets rendered as URL-safe base64 with a type
// prefix. Only a SHA-256 hash of the token is ever persisted, so a database
// leak does not leak usable credentials.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// Prefixes make tokens self-describing so a leaked token is recognizable and
// so the coordinator can reject a beacon token used as an agent token.
const (
	PrefixAgent  = "ipca_"
	PrefixBeacon = "ipcb_"
)

// Kind enumerates the token classes.
type Kind int

const (
	KindAgent Kind = iota
	KindBeacon
)

func (k Kind) prefix() string {
	switch k {
	case KindBeacon:
		return PrefixBeacon
	default:
		return PrefixAgent
	}
}

func (k Kind) String() string {
	switch k {
	case KindBeacon:
		return "beacon"
	default:
		return "agent"
	}
}

// Generate returns a fresh token for the given kind. The returned string is
// the plaintext secret to hand to the operator; it is never stored.
func Generate(k Kind) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("token: read randomness: %w", err)
	}
	return k.prefix() + base64.RawURLEncoding.EncodeToString(buf), nil
}

// GenerateAdmin returns a fresh admin bearer token. Unlike agent/beacon tokens
// it has no kind prefix (the admin token is a single static secret compared
// directly, not looked up), but it is equally high-entropy.
func GenerateAdmin() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("token: read randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Hash returns the hex-encoded SHA-256 digest of a plaintext token. This is
// what gets persisted.
func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// KindOf best-effort classifies a plaintext token by its prefix. It returns an
// error if the prefix is not recognized.
func KindOf(plaintext string) (Kind, error) {
	switch {
	case strings.HasPrefix(plaintext, PrefixAgent):
		return KindAgent, nil
	case strings.HasPrefix(plaintext, PrefixBeacon):
		return KindBeacon, nil
	default:
		return KindAgent, fmt.Errorf("token: unrecognized prefix")
	}
}

// Verify reports whether plaintext matches the stored hash, using a constant
// time comparison. Both arguments should be the hex hash form for a fair
// comparison, so Verify hashes the plaintext internally.
func Verify(plaintext, storedHashHex string) bool {
	computed := Hash(plaintext)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(storedHashHex)) == 1
}

// VerifyHash compares two hex hashes in constant time. Useful when both sides
// are already hashed.
func VerifyHash(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
