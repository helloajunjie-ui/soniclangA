// Package util holds small zero-dependency helpers shared across AgentHub
// internal packages (id generation, secret masking, timestamps).
package util

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// NewID returns a random 16-byte hex identifier.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// rand.Read should never fail; fall back to time-based id.
		return hex.EncodeToString([]byte(time.Now().Format("20060102150405.000000000")))
	}
	return hex.EncodeToString(b)
}

// MaskSecret masks a secret for display: keeps a short prefix and suffix,
// replaces the middle with asterisks. Never returns the plaintext.
func MaskSecret(s string) string {
	if len(s) <= 8 {
		if len(s) == 0 {
			return ""
		}
		return "****"
	}
	prefix := s[:4]
	suffix := s[len(s)-4:]
	return prefix + "****" + suffix
}

// NowUnix returns the current unix millisecond timestamp.
func NowUnix() int64 {
	return time.Now().UnixMilli()
}

// NowRFC returns the current UTC time in RFC3339.
func NowRFC() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// SlicesEqual reports whether two string slices are equal, treating nil and
// empty as equivalent. Simple comparison for small slices (skills, tags).
func SlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
