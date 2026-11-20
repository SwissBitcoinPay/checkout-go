// Package address holds deposit addresses (SPEC.md §31, "Deposit addresses", "Payment URIs"):
// the taproot addresses the merchant's wallet hands out, in their one spelling, and what the
// shop's page shows of an invoice as one QR code.
package address

import (
	"strconv"
	"strings"

	"github.com/sattleme/checkout-go/internal/bech32"
	"github.com/sattleme/checkout-go/internal/proto"
)

const (
	// depositAddressLength is "bc1p" and 58 more characters.
	depositAddressLength = 62
	// programWords is the witness version (1) and the 32-byte program, as 5-bit words.
	programWords = 53
)

// IsDeposit tells whether v is a deposit address: the bech32m encoding (BIP-350) of the
// human-readable part "bc", witness version 1 and a 32-byte program (BIP-341), in lowercase.
// Another network, witness version or length, bech32 instead of bech32m, upper case or a padding
// with bits that are not zero is not one.
func IsDeposit(v any) bool {
	s, ok := v.(string)
	if !ok || len(s) != depositAddressLength || !strings.HasPrefix(s, "bc1") {
		return false
	}
	for i := 3; i < len(s); i++ {
		c := s[i]
		// The bech32 alphabet, lowercase: no 1, b, i, o.
		if !(c == '0' || c >= '2' && c <= '9' || c == 'a' || c >= 'c' && c <= 'h' || c >= 'j' && c <= 'n' || c >= 'p' && c <= 'z') {
			return false
		}
	}
	hrp, words, ok := bech32.Decode(s, bech32.Bech32m)
	if !ok || hrp != "bc" {
		return false
	}
	// 52 words after the version: 256 bits of program and 4 bits of padding, which are zeros.
	return len(words) == programWords && words[0] == 1 && words[len(words)-1]&0b1111 == 0
}

// BTCAmount writes sats in bitcoins, exact, as BIP-21's amount: no trailing zero in the fraction,
// no fraction when whole (49 000 sats is "0.00049", 100 000 000 sats is "1").
func BTCAmount(sats int64) string {
	whole := sats / proto.SatsPerBTC
	fraction := strconv.FormatInt(sats%proto.SatsPerBTC, 10)
	fraction = strings.Repeat("0", 8-len(fraction)) + fraction
	fraction = strings.TrimRight(fraction, "0")
	if fraction == "" {
		return strconv.FormatInt(whole, 10)
	}
	return strconv.FormatInt(whole, 10) + "." + fraction
}

// PaymentURI is what the shop's page shows as one QR code: bitcoin:<address>?amount=<BTC>, with
// &lightning=<invoice> when there is one (BIP-21, the "unified QR" wallets read), or
// lightning:<invoice> without an address ("" for none). Both are written as they are
// (lowercase).
func PaymentURI(deposit string, sats int64, bolt11 string) string {
	if deposit == "" {
		return "lightning:" + bolt11
	}
	uri := "bitcoin:" + deposit + "?amount=" + BTCAmount(sats)
	if bolt11 == "" {
		return uri
	}
	return uri + "&lightning=" + bolt11
}
