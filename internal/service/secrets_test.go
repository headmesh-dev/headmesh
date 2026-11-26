package service

import (
	"strings"
	"testing"
)

// TestNewSecretFormat: every issued secret carries its prefix (so scanners recognise a leak), the stored hash is
// the SHA-256 of exactly that secret, and the display prefix is a prefix of it that can't reveal the rest.
func TestNewSecretFormat(t *testing.T) {
	for _, prefix := range []string{PrefixEnrollmentToken, PrefixAPIKey, PrefixCompletion} {
		raw, hash, display, err := newSecret(prefix)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(raw, prefix) || len(raw) != len(prefix)+52 {
			t.Fatalf("newSecret(%q) = %q: want the prefix and 52 base32 characters", prefix, raw)
		}
		if !secretMatches(raw, hash) {
			t.Fatalf("newSecret(%q): the hash does not match the secret", prefix)
		}
		if !strings.HasPrefix(raw, display) || len(display) != len(prefix)+8 {
			t.Fatalf("newSecret(%q): display %q is not the prefix plus 8 characters", prefix, display)
		}
	}
}

// FuzzSecretMatches: a stored hash matches its own secret and no other string, and a hash of the wrong length
// (a corrupt row) matches nothing.
func FuzzSecretMatches(f *testing.F) {
	for _, s := range []string{"hmj_abc", "hmk_abc", "hmc_abc", "", "hmk_"} {
		f.Add(s, s+"x", 32)
	}
	f.Fuzz(func(t *testing.T, raw, other string, n int) {
		h := hashSecret(raw)
		if !secretMatches(raw, h) {
			t.Fatalf("hash of %q does not match it", raw)
		}
		if other != raw && secretMatches(other, h) {
			t.Fatalf("hash of %q matches %q", raw, other)
		}
		n %= 64
		if n < 0 {
			n = -n
		}
		wrong := h[:min(n, len(h))]
		if n > len(h) {
			wrong = append(wrong, make([]byte, n-len(h))...)
		}
		if n != len(h) && secretMatches(raw, wrong) {
			t.Fatalf("a %d-byte hash must not match", n)
		}
	})
}
