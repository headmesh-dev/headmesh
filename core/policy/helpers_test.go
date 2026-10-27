package policy

import (
	"strings"
	"time"
)

const base32Alphabet = "abcdefghijklmnopqrstuvwxyz234567"

// sid builds a valid 10-character short id from a one-letter prefix and an index.
func sid(prefix byte, i int) string {
	var b strings.Builder
	b.WriteByte(prefix)
	digits := make([]byte, 9)
	for k := 8; k >= 0; k-- {
		digits[k] = base32Alphabet[i%32]
		i /= 32
	}
	b.Write(digits)
	return b.String()
}

var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func ports(ps ...string) []Port {
	out := make([]Port, len(ps))
	for i, p := range ps {
		out[i] = Port(p)
	}
	return out
}
