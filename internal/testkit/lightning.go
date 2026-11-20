package testkit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// BOLT11 invoices built for tests, so that a test chooses their fields. The signature is 65 zero
// bytes: nothing verifies it.

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var bech32Generator = [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}

func polymod(values []int) uint32 {
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>i)&1 == 1 {
				chk ^= bech32Generator[i]
			}
		}
	}
	return chk
}

func expandHrp(hrp string) []int {
	var out []int
	for i := 0; i < len(hrp); i++ {
		out = append(out, int(hrp[i])>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, int(hrp[i])&31)
	}
	return out
}

// EncodeBech32 writes words under hrp with the checksum constant (1: bech32, 0x2bc830a3:
// bech32m).
func EncodeBech32(hrp string, words []int, constant uint32) string {
	values := append(append(expandHrp(hrp), words...), 0, 0, 0, 0, 0, 0)
	mod := polymod(values) ^ constant
	var b bytes.Buffer
	b.WriteString(hrp)
	b.WriteByte('1')
	for _, w := range words {
		b.WriteByte(bech32Charset[w])
	}
	for i := 0; i < 6; i++ {
		b.WriteByte(bech32Charset[(mod>>(5*(5-i)))&31])
	}
	return b.String()
}

// BytesToWords is bytes as 5-bit words, the last padded with zeros.
func BytesToWords(data []byte) []int {
	var words []int
	accumulator, bits := 0, 0
	for _, c := range data {
		accumulator = accumulator<<8 | int(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			words = append(words, (accumulator>>bits)&31)
		}
	}
	if bits > 0 {
		words = append(words, (accumulator<<(5-bits))&31)
	}
	return words
}

// IntegerWords is value as 5-bit words, big-endian, at least length of them.
func IntegerWords(value int64, length int) []int {
	var words []int
	rest := value
	for {
		words = append([]int{int(rest % 32)}, words...)
		rest /= 32
		if rest == 0 {
			break
		}
	}
	for len(words) < length {
		words = append([]int{0}, words...)
	}
	return words
}

func taggedField(tag int, data []int) []int {
	return append([]int{tag, len(data) / 32, len(data) % 32}, data...)
}

// PreimagePair is a 32-byte preimage made of seed, and its payment hash, hex.
func PreimagePair(seed int) (preimage, paymentHash string) {
	p := bytes.Repeat([]byte{byte(seed & 0xff)}, 32)
	h := sha256.Sum256(p)
	return hex.EncodeToString(p), hex.EncodeToString(h[:])
}

// MetadataHash is the SHA-256 of a pay request's metadata, hex.
func MetadataHash(metadata string) string {
	h := sha256.Sum256([]byte(metadata))
	return hex.EncodeToString(h[:])
}

func amountPart(msat int64) string {
	switch {
	case msat%100_000_000_000 == 0:
		return strconv.FormatInt(msat/100_000_000_000, 10)
	case msat%100_000_000 == 0:
		return strconv.FormatInt(msat/100_000_000, 10) + "m"
	case msat%100_000 == 0:
		return strconv.FormatInt(msat/100_000, 10) + "u"
	case msat%100 == 0:
		return strconv.FormatInt(msat/100, 10) + "n"
	}
	return strconv.FormatInt(msat*10, 10) + "p"
}

// Tagged is a tagged field of an invoice: its tag and its 5-bit words.
type Tagged struct {
	Tag  int
	Data []int
}

// Invoice is what MakeInvoice writes.
type Invoice struct {
	// AmountMsat is its amount (HasAmount); Network is "bc" when "".
	AmountMsat int64
	HasAmount  bool
	Network    string
	// PaymentHash is hex; DescriptionHash hex, or "" for none.
	PaymentHash     string
	DescriptionHash string
	Timestamp       int64
	// ExpirySeconds is its expiry (HasExpiry).
	ExpirySeconds int64
	HasExpiry     bool
	// Extra are more tagged fields, after the others.
	Extra []Tagged
}

// MakeInvoice writes a BOLT11 invoice of the fields given.
func MakeInvoice(in Invoice) string {
	network := in.Network
	if network == "" {
		network = "bc"
	}
	hrp := "ln" + network
	if in.HasAmount {
		hrp += amountPart(in.AmountMsat)
	}
	words := IntegerWords(in.Timestamp, 7)
	hash, _ := hex.DecodeString(in.PaymentHash)
	words = append(words, taggedField(1, BytesToWords(hash))...)
	if in.DescriptionHash != "" {
		description, _ := hex.DecodeString(in.DescriptionHash)
		words = append(words, taggedField(23, BytesToWords(description))...)
	}
	if in.HasExpiry {
		words = append(words, taggedField(6, IntegerWords(in.ExpirySeconds, 0))...)
	}
	for _, t := range in.Extra {
		words = append(words, taggedField(t.Tag, t.Data)...)
	}
	words = append(words, make([]int, 104)...)
	return EncodeBech32(hrp, words, 1)
}
