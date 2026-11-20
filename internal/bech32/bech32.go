// Package bech32 decodes bech32 (BIP-173) and bech32m (BIP-350, which differs by its checksum's
// constant alone), without the 90-character limit: BOLT11 invoices are longer. ASCII only: either
// all lowercase or all uppercase (never mixed), case changed for A–Z alone (SPEC.md §6: never
// Unicode case mapping, which would read U+212A KELVIN SIGN as "k").
package bech32

import "strings"

const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var generator = [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}

// The checksum's constant: BIP-173's bech32, BIP-350's bech32m.
const (
	Bech32  uint32 = 1
	Bech32m uint32 = 0x2bc830a3
)

func polymod(values []byte) uint32 {
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>i)&1 == 1 {
				chk ^= generator[i]
			}
		}
	}
	return chk
}

func expandHRP(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

// Decode reads a bech32 string (or bech32m, with the constant Bech32m): its human-readable part,
// lowercase, and its 5-bit words without the checksum. False when it is not valid.
func Decode(input string, constant uint32) (hrp string, words []byte, ok bool) {
	hasLower, hasUpper := false, false
	for i := 0; i < len(input); i++ {
		c := input[i]
		if c < 33 || c > 126 {
			return "", nil, false
		}
		hasLower = hasLower || c >= 'a' && c <= 'z'
		hasUpper = hasUpper || c >= 'A' && c <= 'Z'
	}
	if hasLower && hasUpper {
		return "", nil, false
	}
	text := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, input)
	separator := strings.LastIndexByte(text, '1')
	if separator < 1 || separator+7 > len(text) {
		return "", nil, false
	}
	hrp = text[:separator]
	data := text[separator+1:]
	values := make([]byte, len(data))
	for i := 0; i < len(data); i++ {
		v := strings.IndexByte(charset, data[i])
		if v < 0 {
			return "", nil, false
		}
		values[i] = byte(v)
	}
	if polymod(append(expandHRP(hrp), values...)) != constant {
		return "", nil, false
	}
	return hrp, values[:len(values)-6], true
}

// WordsToBytes regroups 5-bit words into bytes; the trailing padding bits (fewer than 8) are
// dropped.
func WordsToBytes(words []byte) []byte {
	out := make([]byte, 0, len(words)*5/8)
	acc, bits := uint32(0), 0
	for _, w := range words {
		acc = (acc<<5 | uint32(w)) & 0xfff
		bits += 5
		if bits >= 8 {
			bits -= 8
			out = append(out, byte(acc>>bits))
		}
	}
	return out
}

// WordsToInteger is the big-endian unsigned integer of 5-bit words (at most 12 of them).
func WordsToInteger(words []byte) int64 {
	var v int64
	for _, w := range words {
		v = v*32 + int64(w)
	}
	return v
}
