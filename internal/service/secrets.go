// Package service holds Headmesh's use cases — enrollment, grants, API keys, bootstrap — on top of the store
// and the cell hub. HTTP handlers in package api are thin wrappers around it.
package service

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Secret prefixes make leaked credentials recognisable to secret scanners and humans.
const (
	PrefixEnrollmentToken = "hmj_"
	PrefixAPIKey          = "hmk_"
	PrefixCompletion      = "hmc_"
)

var lowerBase32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// newSecret returns prefix + 256 random bits, its SHA-256 (what the database stores) and a short display prefix.
// SHA-256 is the right hash here: the secret is random, so there is nothing to brute-force.
func newSecret(prefix string) (raw string, hash []byte, display string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, "", fmt.Errorf("generate secret: %w", err)
	}
	raw = prefix + lowerBase32.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	return raw, sum[:], raw[:len(prefix)+8], nil
}

func hashSecret(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

func secretMatches(raw string, hash []byte) bool {
	return len(hash) == sha256.Size && subtle.ConstantTimeCompare(hashSecret(raw), hash) == 1
}

// newShortID returns a 10-character base32 id (50 random bits) used in Headscale tags.
func newShortID() (string, error) {
	b := make([]byte, 7)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate short id: %w", err)
	}
	return lowerBase32.EncodeToString(b)[:10], nil
}

var nonName = regexp.MustCompile(`[^a-z0-9-]+`)

// deviceName turns a client-supplied hostname into a DNS-safe device name.
func deviceName(hostname string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(hostname))
	if i := strings.IndexByte(n, '.'); i > 0 {
		n = n[:i] // keep the host label only
	}
	n = strings.Trim(nonName.ReplaceAllString(n, "-"), "-")
	if len(n) > 42 {
		n = strings.TrimRight(n[:42], "-")
	}
	if n == "" {
		return "", errors.New("hostname has no usable characters")
	}
	return n, nil
}
