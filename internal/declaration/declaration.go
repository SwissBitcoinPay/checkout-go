// Package declaration is the declaration of an invoice in the merchant's registry of payments
// (SPEC.md §20), as the merchant's app reads it: published on a majority of the merchant's relays
// before the invoice is shown, so that its payment counts for this order's sale and for no other.
// Its format is the app's, version 1: changing it needs a new version there first, but for an
// optional field the app ignores (as taker): rest, in a rest invoice's.
package declaration

import (
	"encoding/hex"
	"errors"
	"io"
	"regexp"

	"github.com/sattleme/checkout-go/internal/address"
	"github.com/sattleme/checkout-go/internal/bolt11"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/text"
	"github.com/sattleme/checkout-go/internal/urls"
)

// Price is the price a sale is declared with, as the declaration's JSON holds it: Minor is a JSON
// number (at most 2^53 − 1). For a sale in sats: {minor: <sats>, digits: 0, currency: "SAT"}.
type Price struct {
	Minor    int64
	Digits   int
	Currency string
}

// JSON is the price as the declaration and a checkout link's record write it.
func (p Price) JSON() jsonv.Members {
	return jsonv.O("minor", p.Minor, "digits", p.Digits, "currency", p.Currency)
}

// RestOf is the invoice a rest invoice is of: its deposit address and its end (Unix seconds).
type RestOf struct {
	Address   string
	ExpiresAt int64
}

// Input is what a declaration is made of.
type Input struct {
	Sale *derive.Sale
	// Invoice is the invoice, checked.
	Invoice     string
	VerifyURL   string
	PaymentHash string
	Price       Price
	// Note is what the merchant's app shows: the note, else the reference.
	Note      string
	CreatedAt int64
	// Rest is the invoice whose rest this invoice is (SPEC.md §31); nil for an invoice of the
	// order.
	Rest   *RestOf
	Random io.Reader
}

// ErrTooLarge is a declaration that does not fit a NIP-44 plaintext (65 535 bytes).
var ErrTooLarge = errors.New("declaration: too large for a NIP-44 plaintext")

// Inside is the declaration's JSON, as it is sealed inside.
func Inside(in Input) jsonv.Members {
	declaration := jsonv.O(
		"version", proto.DeclarationVersion,
		"invoice", in.Invoice,
		"verifyUrl", in.VerifyURL,
		"sale", jsonv.O("key", hex.EncodeToString(in.Sale.Key), "fiat", in.Price.JSON(), "note", in.Note),
		// "By" in the app: this integration.
		"taker", in.Sale.Taker,
	)
	if in.Rest != nil {
		// Only in a rest invoice's: the app ignores it, and counts the payment for the sale.
		declaration = append(declaration, jsonv.Member{Name: "rest", Value: jsonv.O("address", in.Rest.Address, "expiresAt", in.Rest.ExpiresAt)})
	}
	return declaration
}

// Event is the signed declaration of an invoice for its sale: the declaration (inside, NIP-44
// with the ECDH conversation key of the sale's secret and the merchant's reading key), then
// {"t": "registry-declaration", "v": {"sealed": …}} padded to a multiple of 256 bytes (outside,
// NIP-44 with the outer key), kind 30078, d the registry address, signed by the sale's secret.
// ErrTooLarge when it does not fit.
func Event(in Input) (*nostr.Event, error) {
	inside := string(jsonv.Marshal(Inside(in)))
	if len(inside) > 0xffff {
		return nil, ErrTooLarge
	}
	sealed, err := nip44.Encrypt(inside, in.Sale.InnerKey, in.Random)
	if err != nil {
		return nil, err
	}
	value := jsonv.O("sealed", sealed)
	if len(nostr.Plaintext(proto.DeclarationRecordType, value)) > 0xffff {
		return nil, ErrTooLarge
	}
	addr := derive.RegistryAddress(in.Sale.Registry.Key, in.PaymentHash)
	return nostr.Seal(nostr.SealOptions{
		Type:          proto.DeclarationRecordType,
		Value:         value,
		Signer:        in.Sale.SecretKey,
		EncryptionKey: derive.OuterKey(in.Sale.Registry.Key, addr, in.Sale.Author),
		Address:       addr,
		CreatedAt:     in.CreatedAt,
		Random:        in.Random,
	})
}

// Opened is a declaration of the sale, opened by its holder.
type Opened struct {
	Event     *nostr.Event
	Invoice   string
	Decoded   *bolt11.Invoice
	VerifyURL string
	Price     Price
	Note      string
	// DeclaredAt is the event's created_at.
	DeclaredAt int64
	// Rest is the invoice whose rest it is; nil for an invoice of the order.
	Rest *RestOf
}

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// ParsePrice reads a declared price: minor a safe integer from 0, digits an integer from 0 to 8,
// currency three letters A–Z.
func ParsePrice(v any) (Price, bool) {
	o, ok := jsonv.AsObject(v)
	if !ok {
		return Price{}, false
	}
	minorValue, _ := o.Get("minor")
	digitsValue, _ := o.Get("digits")
	currencyValue, _ := o.Get("currency")
	minor, ok := jsonv.AsSafeInteger(minorValue)
	if !ok || minor < 0 {
		return Price{}, false
	}
	digits, ok := jsonv.AsNumber(digitsValue)
	if !ok || !jsonv.IsInteger(digitsValue) || digits < 0 || digits > proto.MaxDeclaredDigits {
		return Price{}, false
	}
	currency, ok := currencyValue.(string)
	if !ok || !currencyPattern.MatchString(currency) {
		return Price{}, false
	}
	return Price{Minor: minor, Digits: int(digits), Currency: currency}, true
}

// restOf reads a declaration's rest: absent (nil, true), or the invoice it names; false when it
// is invalid.
func restOf(v any, present bool) (*RestOf, bool) {
	if !present {
		return nil, true
	}
	o, ok := jsonv.AsObject(v)
	if !ok {
		return nil, false
	}
	addr, _ := o.Get("address")
	expiresAtValue, _ := o.Get("expiresAt")
	if !address.IsDeposit(addr) {
		return nil, false
	}
	expiresAt, ok := jsonv.AsSafeInteger(expiresAtValue)
	if !ok || expiresAt < 0 {
		return nil, false
	}
	return &RestOf{Address: addr.(string), ExpiresAt: expiresAt}, true
}

// Open reads what a relay sent, if it is a declaration of the sale: author, d tag (the address of
// its own invoice), outer and inner encryption, signature, and the sale's own key inside. newer
// for an authentic one of a newer format; ok false otherwise.
func Open(v any, sale *derive.Sale, memo *nostr.SigMemo) (opened *Opened, newer, ok bool) {
	d, hasD := firstDTag(v)
	if !hasD || !text.IsHex32(d) {
		return nil, false, false
	}
	saleKey := hex.EncodeToString(sale.Key)
	result, ok := nostr.Open(v, nostr.OpenOptions{
		Type:          proto.DeclarationRecordType,
		EncryptionKey: derive.OuterKey(sale.Registry.Key, d, sale.Author),
		Address:       d,
		Author:        sale.Author,
		Memo:          memo,
	}, func(outside any, event *nostr.Event) (*Opened, error) {
		sealedValue, _ := jsonv.Field(outside, "sealed")
		sealed, isText := sealedValue.(string)
		if !isText {
			return nil, nostr.ErrInvalid
		}
		plaintext, err := nip44.Decrypt(sealed, sale.InnerKey)
		if err != nil {
			return nil, nostr.ErrInvalid
		}
		insideValue, err := jsonv.ParseString(plaintext)
		if err != nil {
			return nil, nostr.ErrInvalid
		}
		inside, isObject := jsonv.AsObject(insideValue)
		if !isObject {
			return nil, nostr.ErrInvalid
		}
		version, _ := inside.Get("version")
		if n, isNumber := version.(float64); isNumber && n > proto.DeclarationVersion {
			return nil, nostr.ErrNewer
		}
		invoiceValue, _ := inside.Get("invoice")
		invoice, isText := invoiceValue.(string)
		if version != float64(proto.DeclarationVersion) || !isText {
			return nil, nostr.ErrInvalid
		}
		verifyValue, _ := inside.Get("verifyUrl")
		verifyURL, isText := verifyValue.(string)
		if _, isURL := urls.ParseHTTPS(verifyURL); !isText || !isURL {
			return nil, nostr.ErrInvalid
		}
		saleValue, _ := inside.Get("sale")
		saleObject, isObject := jsonv.AsObject(saleValue)
		if !isObject {
			return nil, nostr.ErrInvalid
		}
		keyValue, _ := saleObject.Get("key")
		noteValue, _ := saleObject.Get("note")
		note, isText := noteValue.(string)
		if keyValue != saleKey || !isText {
			return nil, nostr.ErrInvalid
		}
		fiatValue, _ := saleObject.Get("fiat")
		price, isPrice := ParsePrice(fiatValue)
		if !isPrice {
			return nil, nostr.ErrInvalid
		}
		restValue, hasRest := inside.Get("rest")
		rest, isRest := restOf(restValue, hasRest)
		if !isRest {
			return nil, nostr.ErrInvalid
		}
		decoded, isInvoice := bolt11.Decode(invoice)
		if !isInvoice || decoded.Network != "bc" || !decoded.HasAmount || decoded.AmountMsat <= 0 || decoded.AmountMsat%1000 != 0 {
			return nil, nostr.ErrInvalid
		}
		if d != derive.RegistryAddress(sale.Registry.Key, decoded.PaymentHash) {
			return nil, nostr.ErrInvalid
		}
		return &Opened{
			Event:      event,
			Invoice:    invoice,
			Decoded:    decoded,
			VerifyURL:  verifyURL,
			Price:      price,
			Note:       note,
			DeclaredAt: event.CreatedAt,
			Rest:       rest,
		}, nil
	})
	if !ok {
		return nil, false, false
	}
	if result.Newer {
		return nil, true, false
	}
	return result.Value, false, true
}

// firstDTag is the value of the first d tag of what a relay sent, when it is a string.
func firstDTag(v any) (string, bool) {
	tags, _ := jsonv.Field(v, "tags")
	items, isArray := tags.([]any)
	if !isArray {
		return "", false
	}
	for _, item := range items {
		tag, isArray := item.([]any)
		if !isArray || len(tag) == 0 || tag[0] != "d" {
			continue
		}
		if len(tag) < 2 {
			return "", false
		}
		d, isText := tag[1].(string)
		return d, isText
	}
	return "", false
}

// RegistryOf reads the registry of a channel's hex fields.
func RegistryOf(registryKey, registryReader string) derive.Registry {
	key, _ := hex.DecodeString(registryKey)
	return derive.Registry{Key: key, Reader: registryReader}
}
