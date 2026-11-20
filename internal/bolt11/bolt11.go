// Package bolt11 decodes the fields of a BOLT11 invoice the package checks (SPEC.md §19):
// network, amount, payment hash, description hash, date and expiry. The signature is not
// verified: the payee is the LNURL server's node, which nothing here can check it against; the
// description hash ties the invoice to the merchant's Lightning address instead. The first p and
// h fields count and the last x, as in the merchant's app; an x field of more than 7 words or an
// amount above 21 million bitcoins makes the invoice invalid.
package bolt11

import (
	"encoding/hex"
	"math/big"
	"regexp"

	"github.com/sattleme/checkout-go/internal/bech32"
	"github.com/sattleme/checkout-go/internal/proto"
)

// Invoice holds the fields of a BOLT11 invoice the package checks.
type Invoice struct {
	// Network is "bc" mainnet, "tb" testnet, "tbs" signet, "bcrt" regtest.
	Network string
	// AmountMsat is the exact amount in millisatoshis; HasAmount is false for an invoice without
	// an amount.
	AmountMsat int64
	HasAmount  bool
	// Timestamp is in Unix seconds.
	Timestamp int64
	// ExpirySeconds is the time after Timestamp (3600 when the invoice does not say).
	ExpirySeconds int64
	// PaymentHash is hex, 32 bytes.
	PaymentHash string
	// DescriptionHash is hex, 32 bytes; "" when the invoice carries a plain description.
	DescriptionHash string
}

var hrpPattern = regexp.MustCompile(`^ln(bcrt|bc|tbs|tb)([0-9]+)?([munp])?$`)

// msatPerUnit is the millisatoshis per unit of the amount, by multiplier (1 BTC = 10^11 msat).
var msatPerUnit = map[string]int64{"": 100_000_000_000, "m": 100_000_000, "u": 100_000, "n": 100}

const (
	timestampWords = 7
	// signatureWords is a 65-byte signature, as 5-bit words.
	signatureWords     = 104
	tagPaymentHash     = 1
	tagDescriptionHash = 23
	tagExpiry          = 6
	// hashWords is 256 bits as 5-bit words.
	hashWords            = 52
	defaultExpirySeconds = 3600
)

var maxAmount = big.NewInt(proto.MaxBolt11AmountMsat)

// amountMsat reads the amount of the human-readable part: false when it is invalid.
func amountMsat(digits, multiplier string) (msat int64, present, ok bool) {
	if digits == "" {
		return 0, false, multiplier == ""
	}
	if len(digits) > 1 && digits[0] == '0' {
		return 0, false, false
	}
	// Longer than any amount up to 21 million bitcoins, whatever the multiplier.
	if len(digits) > 22 {
		return 0, false, false
	}
	value, _ := new(big.Int).SetString(digits, 10)
	if multiplier == "p" {
		// A pico-bitcoin is a tenth of a millisatoshi: only whole millisatoshis are valid.
		quotient, remainder := new(big.Int).QuoRem(value, big.NewInt(10), new(big.Int))
		if remainder.Sign() != 0 {
			return 0, false, false
		}
		value = quotient
	} else {
		value.Mul(value, big.NewInt(msatPerUnit[multiplier]))
	}
	if value.Cmp(maxAmount) > 0 {
		return 0, false, false
	}
	return value.Int64(), true, true
}

// Decode reads a BOLT11 invoice: false when it is invalid.
func Decode(input string) (*Invoice, bool) {
	hrp, words, ok := bech32.Decode(input, bech32.Bech32)
	if !ok {
		return nil, false
	}
	m := hrpPattern.FindStringSubmatch(hrp)
	if m == nil {
		return nil, false
	}
	amount, hasAmount, ok := amountMsat(m[2], m[3])
	if !ok {
		return nil, false
	}
	if len(words) < timestampWords+signatureWords {
		return nil, false
	}
	timestamp := bech32.WordsToInteger(words[:timestampWords])
	fields := words[timestampWords : len(words)-signatureWords]

	paymentHash, descriptionHash := "", ""
	expiry, hasExpiry := int64(0), false
	for i := 0; i < len(fields); {
		if i+3 > len(fields) {
			return nil, false
		}
		tag := fields[i]
		length := int(fields[i+1])*32 + int(fields[i+2])
		if i+3+length > len(fields) {
			return nil, false
		}
		data := fields[i+3 : i+3+length]
		// Fields of an unexpected length are skipped, as BOLT11 requires for p and h; the first
		// of each counts, and the last x (as the app reads them).
		switch {
		case tag == tagPaymentHash && length == hashWords && paymentHash == "":
			paymentHash = hex.EncodeToString(bech32.WordsToBytes(data))
		case tag == tagDescriptionHash && length == hashWords && descriptionHash == "":
			descriptionHash = hex.EncodeToString(bech32.WordsToBytes(data))
		case tag == tagExpiry:
			if length > proto.MaxBolt11ExpiryWords {
				return nil, false
			}
			expiry, hasExpiry = bech32.WordsToInteger(data), true
		}
		i += 3 + length
	}
	if paymentHash == "" {
		return nil, false
	}
	if !hasExpiry {
		expiry = defaultExpirySeconds
	}
	return &Invoice{
		Network:         m[1],
		AmountMsat:      amount,
		HasAmount:       hasAmount,
		Timestamp:       timestamp,
		ExpirySeconds:   expiry,
		PaymentHash:     paymentHash,
		DescriptionHash: descriptionHash,
	}, true
}
