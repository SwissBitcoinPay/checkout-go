package checkout_test

import (
	"encoding/json"
	"testing"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/jsonv"
)

// SPEC.md §3: every value an integrator holds is written by encoding/json as the specification
// writes it, as the README says to store it (json.Marshal): each type, each state of a status,
// whether it is given as itself, as a pointer, as an interface (any), or inside a struct, a slice
// or a map of the application's.
func TestEveryTypeOfTheDataModelIsWrittenAsSpecMdWritesIt(t *testing.T) {
	const hash = "9c41d2b5e1f0a7c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f6a7b8c9d0e1"
	const txid = "f0e1d2c3b4a5968778695a4b3c2d1e0ff0e1d2c3b4a5968778695a4b3c2d1e0f"
	const address = "bc1px22v5j99gdwhxqyelg2p6tdcfajjd3njhzn22c2gura2dvxg2xlqw2wte4"
	const uri = "bitcoin:" + address + "?amount=0.00049&lightning=lnbc490u1p"
	at := int64(1_791_378_000)
	fiat := &checkout.Fiat{Amount: "24.50", Currency: "CHF", Minor: 2450, Digits: 2}
	fiatJSON := jsonv.O("amount", "24.50", "currency", "CHF", "minor", "2450", "digits", 2)
	rate := &checkout.RateInfo{Source: "coingecko", Currency: "CHF", Price: "69012.5", FetchedAt: at, Time: &at}
	rateJSON := jsonv.O("source", "coingecko", "currency", "CHF", "price", "69012.5", "fetchedAt", at, "time", at)
	rest := &checkout.Rest{Sats: 29_000, Bolt11: "lnbc290u1p", PaymentURI: "bitcoin:" + address + "?amount=0.00029&lightning=lnbc290u1p"}
	restJSON := jsonv.O("sats", "29000", "bolt11", "lnbc290u1p", "paymentUri", "bitcoin:"+address+"?amount=0.00029&lightning=lnbc290u1p")
	for _, c := range []struct {
		name  string
		value any
		want  any
	}{
		{"Fiat", *fiat, fiatJSON},
		{"RateInfo", *rate, rateJSON},
		{"RateInfo undated", checkout.RateInfo{Source: "kraken", Currency: "CHF", Price: "69000", FetchedAt: at},
			jsonv.O("source", "kraken", "currency", "CHF", "price", "69000", "fetchedAt", at, "time", nil)},
		{"Rest", *rest, restJSON},
		{"Rest without an invoice", checkout.Rest{Sats: 500}, jsonv.O("sats", "500", "bolt11", nil, "paymentUri", nil)},
		{"open", checkout.PaymentStatus{State: checkout.StateOpen, ExpiresAt: at + 900},
			jsonv.O("state", "open", "expiresAt", at+900, "received", "0", "rest", nil)},
		{"open with a rest", checkout.PaymentStatus{State: checkout.StateOpen, ExpiresAt: at + 900, Received: 20_000, Rest: rest},
			jsonv.O("state", "open", "expiresAt", at+900, "received", "20000", "rest", restJSON)},
		{"unconfirmed", checkout.PaymentStatus{State: checkout.StateUnconfirmed, Received: 49_000, Confirmations: 1, Txids: []string{txid}},
			jsonv.O("state", "unconfirmed", "received", "49000", "confirmations", 1, "txids", []string{txid})},
		{"paid in Lightning", checkout.PaymentStatus{State: checkout.StatePaid, Method: checkout.MethodLightning, Preimage: hash},
			jsonv.O("state", "paid", "method", "lightning", "preimage", hash)},
		{"paid on-chain", checkout.PaymentStatus{State: checkout.StatePaid, Method: checkout.MethodOnchain, Received: 49_000, Txids: []string{txid}},
			jsonv.O("state", "paid", "method", "onchain", "received", "49000", "txids", []string{txid})},
		{"paid both ways", checkout.PaymentStatus{State: checkout.StatePaid, Method: checkout.MethodMixed, Received: 49_000, Txids: []string{txid}, Preimages: []string{hash}},
			jsonv.O("state", "paid", "method", "mixed", "received", "49000", "txids", []string{txid}, "preimages", []string{hash})},
		{"paid by the rest alone", checkout.PaymentStatus{State: checkout.StatePaid, Method: checkout.MethodMixed, Received: 49_000, Preimages: []string{hash}},
			jsonv.O("state", "paid", "method", "mixed", "received", "49000", "txids", []string{}, "preimages", []string{hash})},
		{"expired", checkout.PaymentStatus{State: checkout.StateExpired, Received: 20_000}, jsonv.O("state", "expired", "received", "20000")},
		{"Merchant", checkout.Merchant{BusinessName: "Sunrise Bakery", MinSats: 1, MaxSats: 100_000_000, OnchainAddresses: 3},
			jsonv.O("businessName", "Sunrise Bakery", "minSats", "1", "maxSats", "100000000", "onchainAddresses", 3)},
		{"Invoice", checkout.Invoice{Ref: "ORD-1042", Bolt11: "lnbc490u1p", PaymentHash: hash, VerifyURL: "https://breez.tips/lnurlp/verify/" + hash,
			OnchainAddress: address, PaymentURI: uri, Sats: 49_000, Fiat: fiat, Rate: rate, Note: "Order 1042", CreatedAt: at, ExpiresAt: at + 900,
			CheckoutURL: "https://pay.example/pay#c.a.b.c", RedirectURL: "myshop://orders/1042"},
			jsonv.O("ref", "ORD-1042", "bolt11", "lnbc490u1p", "paymentHash", hash, "verifyUrl", "https://breez.tips/lnurlp/verify/"+hash,
				"onchainAddress", address, "paymentUri", uri, "sats", "49000", "fiat", fiatJSON, "rate", rateJSON, "note", "Order 1042",
				"createdAt", at, "expiresAt", at+900, "checkoutUrl", "https://pay.example/pay#c.a.b.c", "redirectUrl", "myshop://orders/1042")},
		{"Invoice on-chain only", checkout.Invoice{Ref: "ORD-1043", OnchainAddress: address, PaymentURI: "bitcoin:" + address + "?amount=0.00049",
			Sats: 49_000, Note: "ORD-1043", CreatedAt: at, ExpiresAt: at + 900},
			jsonv.O("ref", "ORD-1043", "bolt11", nil, "paymentHash", nil, "verifyUrl", nil, "onchainAddress", address,
				"paymentUri", "bitcoin:"+address+"?amount=0.00049", "sats", "49000", "fiat", nil, "rate", nil, "note", "ORD-1043",
				"createdAt", at, "expiresAt", at+900, "checkoutUrl", nil, "redirectUrl", nil)},
		{"DeclaredInvoice", checkout.DeclaredInvoice{Ref: "ORD-1042", Bolt11: "lnbc490u1p", PaymentHash: hash, VerifyURL: "https://breez.tips/lnurlp/verify/" + hash,
			PaymentURI: "lightning:lnbc490u1p", Sats: 49_000, Fiat: fiat, Note: "Order 1042", CreatedAt: at, ExpiresAt: at + 900, DeclaredAt: at + 2},
			jsonv.O("ref", "ORD-1042", "bolt11", "lnbc490u1p", "paymentHash", hash, "verifyUrl", "https://breez.tips/lnurlp/verify/"+hash,
				"onchainAddress", nil, "paymentUri", "lightning:lnbc490u1p", "sats", "49000", "fiat", fiatJSON, "note", "Order 1042",
				"createdAt", at, "expiresAt", at+900, "declaredAt", at+2)},
		{"InvoiceToCheck", checkout.InvoiceToCheck{Ref: "ORD-1042", Bolt11: "lnbc490u1p", PaymentHash: hash, VerifyURL: "https://breez.tips/lnurlp/verify/" + hash,
			OnchainAddress: address, Sats: 49_000, ExpiresAt: at + 900},
			jsonv.O("ref", "ORD-1042", "bolt11", "lnbc490u1p", "paymentHash", hash, "verifyUrl", "https://breez.tips/lnurlp/verify/"+hash,
				"onchainAddress", address, "sats", "49000", "expiresAt", at+900)},
	} {
		for form, v := range map[string]any{
			"as itself":      c.value,
			"as a pointer":   pointerTo(c.value),
			"in a struct":    struct{ Value any }{c.value},
			"in a slice":     []any{c.value},
			"in a map":       map[string]any{"value": c.value},
			"as an any held": &struct{ V any }{c.value},
		} {
			data, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("%s, %s: %v", c.name, form, err)
			}
			got, err := jsonv.Parse(data)
			if err != nil {
				t.Fatalf("%s, %s: %v", c.name, form, err)
			}
			want := c.want
			switch form {
			case "in a struct":
				want = jsonv.O("Value", c.want)
			case "in a slice":
				want = []any{c.want}
			case "in a map":
				want = jsonv.O("value", c.want)
			case "as an any held":
				want = jsonv.O("V", c.want)
			}
			same(t, c.name+", "+form, got, want)
		}
	}
}

// pointerTo is a pointer to a copy of v, of v's own type.
func pointerTo(v any) any {
	switch v := v.(type) {
	case checkout.Fiat:
		return &v
	case checkout.RateInfo:
		return &v
	case checkout.Rest:
		return &v
	case checkout.PaymentStatus:
		return &v
	case checkout.Merchant:
		return &v
	case checkout.Invoice:
		return &v
	case checkout.DeclaredInvoice:
		return &v
	case checkout.InvoiceToCheck:
		return &v
	}
	panic("not a type of the data model")
}
