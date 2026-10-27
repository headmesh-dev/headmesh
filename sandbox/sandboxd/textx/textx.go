// Package textx has text helpers for tool output: ANSI stripping, UTF-8 safe
// truncation and binary detection.
package textx

import (
	"bytes"
	"unicode/utf8"
)

// StripANSI removes terminal escape sequences (CSI, OSC, DCS and two-byte
// escapes), turns CRLF into LF and drops lone carriage returns and other
// C0 control bytes except tab and newline.
func StripANSI(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == 0x1b && i+1 < len(b):
			i = skipEscape(b, i)
		case c == 0x1b:
			// trailing ESC: drop
		case c == '\r':
			if i+1 < len(b) && b[i+1] == '\n' {
				continue
			}
		case c == '\t' || c == '\n':
			out = append(out, c)
		case c < 0x20 || c == 0x7f:
			// other controls (BEL, BS, ...) carry no text
		default:
			out = append(out, c)
		}
	}
	return out
}

// skipEscape returns the index of the last byte of the escape starting at i.
func skipEscape(b []byte, i int) int {
	switch b[i+1] {
	case '[': // CSI: parameters, then a final byte in 0x40–0x7e
		for j := i + 2; j < len(b); j++ {
			if b[j] >= 0x40 && b[j] <= 0x7e {
				return j
			}
		}
		return len(b) - 1
	case ']', 'P', '_', '^': // OSC/DCS/APC/PM: until BEL or ST (ESC \)
		for j := i + 2; j < len(b); j++ {
			if b[j] == 0x07 {
				return j
			}
			if b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\' {
				return j + 1
			}
		}
		return len(b) - 1
	case '(', ')', '*', '+', '#', '%': // charset designations take one more byte
		return min(i+2, len(b)-1)
	default: // two-byte escape such as ESC = or ESC M
		return i + 1
	}
}

// CutUTF8 returns the longest prefix of b no longer than n bytes that does
// not end in the middle of a UTF-8 sequence.
func CutUTF8(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	cut := n
	for cut > 0 && cut > n-utf8.UTFMax && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return b[:cut]
}

// TailUTF8 returns the longest suffix of b no longer than n bytes that
// starts on a rune boundary.
func TailUTF8(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	start := len(b) - n
	for start < len(b) && start < len(b)-n+utf8.UTFMax && !utf8.RuneStart(b[start]) {
		start++
	}
	return b[start:]
}

// LooksBinary reports whether the sample has a NUL byte or is mostly
// invalid UTF-8.
func LooksBinary(sample []byte) bool {
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return true
	}
	if utf8.Valid(sample) {
		return false
	}
	bad := 0
	for len(sample) > 0 {
		r, size := utf8.DecodeRune(sample)
		if r == utf8.RuneError && size == 1 {
			bad++
		}
		sample = sample[size:]
	}
	return bad > 8
}
