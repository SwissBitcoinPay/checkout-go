// Package text applies the text rules every implementation applies alike (SPEC.md §6): lengths in
// Unicode code points, trimming and case of ASCII only, strict UTF-8, lone surrogates invalid. Go's
// own strings.TrimSpace and strings.ToLower follow other rules (Unicode spaces, the Kelvin sign
// lowered to "k"), which the other implementations do not share.
package text

import (
	"unicode/utf8"

	"github.com/sattleme/checkout-go/internal/jsonv"
)

// IsWellFormed tells whether s is valid UTF-8: no invalid byte, no lone surrogate (a JSON escape
// makes one, which jsonv keeps as WTF-8).
func IsWellFormed(s string) bool {
	return utf8.ValidString(s)
}

// CodePoints returns the number of Unicode code points of s, a lone surrogate counting as one (as
// JavaScript's Array.from counts it).
func CodePoints(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if _, ok := jsonv.IsSurrogateAt(s, i); ok {
			i += 3
		} else {
			_, size := utf8.DecodeRuneInString(s[i:])
			i += size
		}
		n++
	}
	return n
}

// isASCIISpace tells whether c is ASCII whitespace: space, tab, line feed, carriage return.
func isASCIISpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// TrimASCII returns s without its leading and trailing ASCII whitespace.
func TrimASCII(s string) string {
	start, end := 0, len(s)
	for start < end && isASCIISpace(s[start]) {
		start++
	}
	for end > start && isASCIISpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

// TrimSpaces returns s without its leading and trailing spaces (U+0020) alone: a note's rule.
func TrimSpaces(s string) string {
	start, end := 0, len(s)
	for start < end && s[start] == ' ' {
		start++
	}
	for end > start && s[end-1] == ' ' {
		end--
	}
	return s[start:end]
}

// LowerASCII returns s with A–Z lowered to a–z, and nothing else changed.
func LowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// EqualFoldASCII tells whether a and b are equal once their ASCII letters are lowered.
func EqualFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x >= 'A' && x <= 'Z' {
			x += 'a' - 'A'
		}
		if y >= 'A' && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// HasControl tells whether s holds a C0 control character (U+0000 to U+001F) or DEL (U+007F).
func HasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}

// IsLowerHex tells whether s is n characters of lowercase hex.
func IsLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// IsHex32 tells whether s is the lowercase hex of 32 bytes.
func IsHex32(s string) bool { return IsLowerHex(s, 64) }

// IsHex64 tells whether s is the lowercase hex of 64 bytes.
func IsHex64(s string) bool { return IsLowerHex(s, 128) }

// IsHex32Value tells whether a JSON value is the lowercase hex of 32 bytes.
func IsHex32Value(v any) bool {
	s, ok := v.(string)
	return ok && IsHex32(s)
}

// IsDigits tells whether s is 1 or more ASCII digits.
func IsDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
