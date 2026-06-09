package relay

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"

	"github.com/kaandikec/internetmerge/internal/version"
)

// GenerateKey returns a fresh base64-encoded 32-byte shared relay key.
func GenerateKey() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b[:]), nil
}

// DecodeKey base64-decodes a shared key and verifies it is at least 16 bytes.
// It returns distinct errors for empty, non-base64, and too-short keys.
func DecodeKey(b64 string) ([]byte, error) {
	if b64 == "" {
		return nil, errors.New("relay: no key provided (--key or INTERNETMERGE_RELAY_KEY)")
	}
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("relay: key is not valid base64: %w", err)
	}
	if len(key) < 16 {
		return nil, errors.New("relay: key too short (need >= 16 bytes)")
	}
	return key, nil
}

// ListenAndServe decodes the base64 key, listens on addr, and serves the relay
// until the listener fails. logger may be nil (defaults to the standard logger).
func ListenAndServe(addr, b64key string, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	key, err := DecodeKey(b64key)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("relay: listen %s: %w", addr, err)
	}
	logger.Printf("internetmerge relay %s listening on %s", version.Version, addr)
	return New(key).Serve(ln)
}
