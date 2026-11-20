package onchain

import (
	"encoding/hex"
	"strconv"
	"testing"

	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// The package against vectors/onchain-rests.json (SPEC.md §31, "The rest of an underpayment",
// "Acceptance records"): the rest keys and addresses, rest records and acceptance records as a
// checkout link's page reads them, declarations of rest invoices as the sale reads them, and what
// a check shows and asks of the rest.

func restsVectors(t *testing.T) (*jsonv.Object, []byte) {
	t.Helper()
	v := tv.Load(t, "onchain-rests.json")
	key, err := hex.DecodeString(tv.Str(t, v, "integrationKey"))
	if err != nil {
		t.Fatal(err)
	}
	return v, key
}

func digitsOf(t *testing.T, o *jsonv.Object, name string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(tv.Str(t, o, name), 10, 64)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return n
}

func TestTheRestKeysAndAddressesOfTheVectors(t *testing.T) {
	v, integrationKey := restsVectors(t)
	cases := tv.Objs(t, v, "keys")
	for _, c := range cases {
		keys := derive.RestKeys(integrationKey, tv.Str(t, c, "ref"), tv.Str(t, c, "address"), tv.Int(t, c, "expiresAt"))
		var records []any
		for _, r := range tv.Objs(t, c, "records") {
			hash := tv.Str(t, r, "paymentHash")
			records = append(records, jsonv.O("paymentHash", hash, "address", derive.RestRecordAddress(keys.Key, hash)))
		}
		got := jsonv.O(
			"restKey", hex.EncodeToString(keys.Key),
			"restSecret", hex.EncodeToString(keys.SecretKey),
			"restAuthor", keys.Author,
			"restEncryptionKey", hex.EncodeToString(keys.EncryptionKey),
			"records", records,
			"acceptanceAddress", derive.AcceptanceRecordAddress(keys.Key),
		)
		want := jsonv.O(
			"restKey", tv.Get(c, "restKey"),
			"restSecret", tv.Get(c, "restSecret"),
			"restAuthor", tv.Get(c, "restAuthor"),
			"restEncryptionKey", tv.Get(c, "restEncryptionKey"),
			"records", tv.Get(c, "records"),
			"acceptanceAddress", tv.Get(c, "acceptanceAddress"),
		)
		tv.Equal(t, tv.Str(t, c, "ref")+" "+tv.Str(t, c, "address"), got, want)
	}
	t.Logf("%d cases", len(cases))
}

// restsKeys are the rest keys of the first case of keys: the invoice link of the order ORD-1042.
func restsKeys(t *testing.T, v *jsonv.Object, integrationKey []byte, link *jsonv.Object) derive.Rest {
	t.Helper()
	first := tv.Objs(t, v, "keys")[0]
	return derive.RestKeys(integrationKey, "ORD-1042", tv.Str(t, first, "address"), tv.Int(t, link, "expiresAt"))
}

func TestTheRestRecordsOfTheVectorsAsAPageReadsThem(t *testing.T) {
	v, integrationKey := restsVectors(t)
	records := tv.Obj(t, v, "records")
	link := tv.Obj(t, records, "link")
	keys := restsKeys(t, v, integrationKey, link)
	if hex.EncodeToString(keys.Key) != tv.Str(t, records, "restKey") || keys.Author != tv.Str(t, records, "restAuthor") {
		t.Fatal("the rest keys")
	}
	// As a page reads them: the rest key and author its link's record carries, the encryption key
	// the rest key derives.
	reader := RestReader{Key: keys.Key, Author: tv.Str(t, records, "restAuthor"), EncryptionKey: derive.RestEncryptionKey(keys.Key)}
	terms := RestLinkTerms{Sats: digitsOf(t, link, "sats"), CreatedAt: tv.Int(t, link, "createdAt"), ExpiresAt: tv.Int(t, link, "expiresAt")}
	cases := tv.Objs(t, records, "cases")
	for _, c := range cases {
		invoice, _, newer, ok := OpenRestRecord(tv.Get(c, "event"), reader, terms, nil)
		var got any
		switch {
		case ok && newer:
			got = "newer"
		case ok:
			got = jsonv.O("invoice", invoice.Bolt11, "paymentHash", invoice.PaymentHash, "verifyUrl", invoice.VerifyURL, "sats", strconv.FormatInt(invoice.Sats, 10))
		}
		tv.Equal(t, tv.Str(t, c, "name"), got, tv.Get(c, "expected"))
	}
	t.Logf("%d cases", len(cases))
}

func TestTheAcceptanceRecordsOfTheVectorsAsAPageReadsThem(t *testing.T) {
	v, integrationKey := restsVectors(t)
	acceptances := tv.Obj(t, v, "acceptances")
	keys := restsKeys(t, v, integrationKey, tv.Obj(t, tv.Obj(t, v, "records"), "link"))
	if hex.EncodeToString(keys.Key) != tv.Str(t, acceptances, "restKey") || keys.Author != tv.Str(t, acceptances, "restAuthor") {
		t.Fatal("the rest keys")
	}
	sats := digitsOf(t, tv.Obj(t, acceptances, "link"), "sats")
	cases := tv.Objs(t, acceptances, "cases")
	for _, c := range cases {
		received, _, newer, ok := OpenAcceptanceRecord(tv.Get(c, "event"), ReaderOf(keys), sats)
		var got any
		switch {
		case ok && newer:
			got = "newer"
		case ok:
			got = jsonv.O("received", strconv.FormatInt(received, 10))
		}
		tv.Equal(t, tv.Str(t, c, "name"), got, tv.Get(c, "expected"))
	}
	t.Logf("%d cases", len(cases))
}

func TestTheDeclarationsOfRestInvoicesOfTheVectorsAsTheSaleReadsThem(t *testing.T) {
	v, integrationKey := restsVectors(t)
	declarations := tv.Obj(t, v, "declarations")
	sale, err := derive.SaleOf(integrationKey, declaration.RegistryOf(tv.Str(t, declarations, "registryKey"), tv.Str(t, declarations, "registryReader")),
		tv.Str(t, declarations, "ref"))
	if err != nil {
		t.Fatal(err)
	}
	cases := tv.Objs(t, declarations, "cases")
	for _, c := range cases {
		opened, newer, ok := declaration.Open(tv.Get(c, "event"), sale, nil)
		var got any
		if ok && !newer {
			var rest any
			if opened.Rest != nil {
				rest = jsonv.O("address", opened.Rest.Address, "expiresAt", opened.Rest.ExpiresAt)
			}
			got = jsonv.O("invoice", opened.Invoice, "note", opened.Note, "rest", rest)
		}
		tv.Equal(t, tv.Str(t, c, "name"), got, tv.Get(c, "expected"))
	}
	t.Logf("%d cases", len(cases))
}

func TestWhatACheckShowsAndAsksOfTheRestInTheVectors(t *testing.T) {
	v, _ := restsVectors(t)
	address := tv.Str(t, tv.Objs(t, v, "keys")[0], "address")
	cases := tv.Objs(t, v, "asking")
	for _, c := range cases {
		var rests []RestInvoice
		paid := map[string]bool{}
		for _, r := range tv.Objs(t, c, "rests") {
			rests = append(rests, RestInvoice{
				Bolt11: tv.Str(t, r, "id"), PaymentHash: tv.Str(t, r, "paymentHash"), VerifyURL: "https://breez.tips/verify",
				Sats: digitsOf(t, r, "sats"), CreatedAt: tv.Int(t, r, "createdAt"), ExpiresAt: tv.Int(t, r, "expiresAt"),
			})
			if tv.Get(r, "paid") == true {
				paid[tv.Str(t, r, "paymentHash")] = true
			}
		}
		sats, expiresAt, now := digitsOf(t, c, "sats"), tv.Int(t, c, "expiresAt"), tv.Int(t, c, "now")
		rest := RestOf(sats, expiresAt, address, digitsOf(t, c, "received"), now, rests, paid)
		var lease *LeaseOfInvoice
		if l, ok := jsonv.AsObject(tv.Get(c, "lease")); ok {
			lease = &LeaseOfInvoice{Sats: digitsOf(t, l, "sats"), ExpiresAt: tv.Int(t, l, "expiresAt")}
		}
		ask, asking := RestToAsk(rest, sats, expiresAt, now, lease)
		sendable := tv.Obj(t, c, "sendable")
		if asking && !Sendable(ask.Sats, digitsOf(t, sendable, "min"), digitsOf(t, sendable, "max")) {
			asking = false
		}
		var gotRest, gotAsk any
		if rest != nil {
			var invoice any
			if rest.Bolt11 != "" {
				invoice = rest.Bolt11
			}
			gotRest = jsonv.O("sats", strconv.FormatInt(rest.Sats, 10), "invoice", invoice)
		}
		if asking {
			gotAsk = jsonv.O("sats", strconv.FormatInt(ask.Sats, 10), "expirySeconds", ask.ExpirySeconds)
		}
		tv.Equal(t, tv.Str(t, c, "name"), jsonv.O("rest", gotRest, "ask", gotAsk), tv.Get(c, "expected"))
	}
	t.Logf("%d cases", len(cases))
}
