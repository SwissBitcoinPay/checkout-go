// Package b64 reads and writes base64 (RFC 4648) strictly (SPEC.md §2): one value has one
// spelling, anything else is refused. encoding/base64 skips line breaks even in its strict mode,
// and accepts either padding in some decoders: each text is checked against its alphabet, decoded,
// then encoded again and compared with itself.
package b64

import "encoding/base64"

var (
	urlEncoding = base64.RawURLEncoding.Strict()
	stdEncoding = base64.StdEncoding.Strict()
)

// EncodeURL writes bytes as base64url without padding (RFC 4648 §5).
func EncodeURL(b []byte) string { return urlEncoding.EncodeToString(b) }

// DecodeURL reads unpadded base64url text: its bytes, or false unless it is the canonical
// encoding of some bytes (only `A–Z a–z 0–9 - _`, no padding, unused trailing bits zero).
func DecodeURL(s string) ([]byte, bool) {
	for i := 0; i < len(s); i++ {
		if !isURLChar(s[i]) {
			return nil, false
		}
	}
	b, err := urlEncoding.DecodeString(s)
	if err != nil || urlEncoding.EncodeToString(b) != s {
		return nil, false
	}
	return b, true
}

// EncodeStd writes bytes as standard base64 with padding (RFC 4648 §4), as NIP-44 payloads are.
func EncodeStd(b []byte) string { return stdEncoding.EncodeToString(b) }

// DecodeStd reads padded standard base64 text: its bytes, or false unless it is the canonical
// encoding of some bytes (`A–Z a–z 0–9 + /`, `=` padding exactly where RFC 4648 §4 puts it).
func DecodeStd(s string) ([]byte, bool) {
	if len(s)%4 != 0 {
		return nil, false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !isStdChar(c) && c != '=' {
			return nil, false
		}
	}
	b, err := stdEncoding.DecodeString(s)
	if err != nil || stdEncoding.EncodeToString(b) != s {
		return nil, false
	}
	return b, true
}

func isAlnum(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

func isURLChar(c byte) bool { return isAlnum(c) || c == '-' || c == '_' }

func isStdChar(c byte) bool { return isAlnum(c) || c == '+' || c == '/' }
