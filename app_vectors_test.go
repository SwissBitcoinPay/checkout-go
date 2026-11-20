package checkout_test

import (
	"encoding/hex"
	"strconv"
	"testing"
	"time"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/bolt11"
	"github.com/sattleme/checkout-go/internal/checkoutlink"
	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/onchain"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/testkit"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// The package's half of the cross-checks with the merchant's apps (spec/vectors/app): what it
// makes end to end, for the orders of the vectors, is what the apps read. Its randomness is its
// own: the events differ from the vectors' byte for byte, and hold exactly what they hold
// (PORTING.md, definition of done, 2 and 3).

// strings is a vector's array of strings.
func vectorStrings(t *testing.T, o *jsonv.Object, name string) []string {
	t.Helper()
	var out []string
	for _, v := range tv.Arr(t, o, name) {
		out = append(out, v.(string))
	}
	return out
}

// answering has the merchant's Lightning address answer, for the invoice of each amount (msat),
// the invoice and verify URL given.
func answering(t *testing.T, m *testkit.Merchant, invoices map[int64][2]string) {
	t.Helper()
	m.SetTamper(func(made *testkit.MadeInvoice) {
		answer, ok := invoices[made.AmountMsat]
		if !ok {
			return
		}
		decoded, valid := bolt11.Decode(answer[0])
		if !valid {
			t.Errorf("not an invoice: %s", answer[0])
			return
		}
		made.PR, made.PaymentHash, made.Verify = answer[0], decoded.PaymentHash, answer[1]
	})
}

// opened is a record a relay holds, opened with its key: its type and value, and its plaintext's
// length.
func opened(t *testing.T, e *nostr.Event, key []byte) (string, any, int) {
	t.Helper()
	plaintext, err := nip44.Decrypt(e.Content, key)
	must(t, err)
	record, err := jsonv.ParseString(plaintext)
	must(t, err)
	typ, _ := fieldOf(record, "t").(string)
	return typ, fieldOf(record, "v"), len(plaintext)
}

// eventOf is a vector's event.
func eventOf(t *testing.T, o *jsonv.Object, name string) *nostr.Event {
	t.Helper()
	v, _ := o.Get(name)
	e, ok := nostr.Valid(v)
	if !ok {
		t.Fatalf("%s: not a valid event", name)
	}
	return e
}

// PORTING.md, definition of done, 3: a checkout link the package makes for the order of
// vectors/app/checkout-link.json (same channel origin, relays, business's name, pay request,
// price, invoice and verify URL) holds exactly the vector's value, in a valid event of kind 30078
// signed by its own author, with the tags of SPEC.md §30, whose content opens with the encryption
// key its URL's link key derives (a plaintext of a multiple of 256 bytes, type checkout).
func TestTheCheckoutLinkOfTheAppsVectorHoldsItsValue(t *testing.T) {
	v := tv.Load(t, "app/checkout-link.json")
	expiresAt := tv.Int(t, v, "expiresAt")
	m := testkit.NewMerchant()
	m.KeyRelays = vectorStrings(t, v, "relays")
	m.SetNow(expiresAt - 600)
	m.Start(nil, nil)
	t.Cleanup(m.Stop)
	if tv.Str(t, v, "origin") != origin || tv.Str(t, v, "businessName") != "Sunrise Bakery" {
		t.Fatal("another channel")
	}
	m.PublishChannel(m.ChannelValue(m.Now()-60, jsonv.Member{Name: "checkout", Value: jsonv.O("origin", origin)}), m.Now()-60)
	payRequest := tv.Obj(t, v, "payRequest")
	for _, name := range payRequest.Keys() {
		m.SetPay(pay(name, fieldOf(payRequest, name)))
	}
	sats, _ := strconv.ParseInt(tv.Str(t, v, "sats"), 10, 64)
	answering(t, m, map[int64][2]string{sats * 1000: {tv.Str(t, v, "invoice"), tv.Str(t, v, "verifyUrl")}})
	o := tv.Obj(t, v, "order")
	fiat := tv.Obj(t, o, "fiat")
	c := clientOf(t, m, checkout.WithRateSource(price(tv.Str(t, v, "price"))))
	invoice, err := c.CreateInvoice(ctx, checkout.Order{
		Ref:             tv.Str(t, o, "ref"),
		Fiat:            &checkout.Price{Amount: tv.Str(t, fiat, "amount"), Currency: tv.Str(t, fiat, "currency")},
		Note:            tv.Str(t, o, "note"),
		DurationSeconds: int(tv.Int(t, o, "durationSeconds")),
		Checkout:        fieldOf(o, "checkout") == true,
		RedirectURL:     tv.Str(t, o, "redirectUrl"),
	})
	must(t, err)
	if invoice.Sats != sats || invoice.Bolt11 != tv.Str(t, v, "invoice") || invoice.ExpiresAt != expiresAt {
		t.Fatalf("%+v", invoice)
	}
	// Valid, kind 30078, its own author's; opened with the key its URL's link key derives: a
	// plaintext of a multiple of 256 bytes, type checkout (openLink checks them).
	l := openLink(t, m, invoice.CheckoutURL)
	if l.event.Kind != proto.RecordKind || l.event.PubKey != l.fragment.Author {
		t.Errorf("kind %v, by %s", l.event.Kind, l.event.PubKey)
	}
	address, encryptionKey := derive.CheckoutKeys(l.fragment.LinkKey)
	same(t, "tags", l.event.Tags, [][]string{{"d", address}, {"expiration", itoa(expiresAt + 604_800)}})
	same(t, "value", l.value, fieldOf(v, "value"))
	if typ, _, n := opened(t, l.event, encryptionKey); typ != proto.CheckoutRecordType || n%256 != 0 {
		t.Errorf("type %s, %d bytes", typ, n)
	}
	// And the vector itself: its URL gives its keys, its event opens to its value.
	fragment := checkoutlink.ParseFragment(tv.Str(t, v, "url")[len(origin+"/pay"):])
	vectorAddress, vectorKey := derive.CheckoutKeys(fragment.LinkKey)
	if fragment.Status != checkoutlink.FragmentLink || hex.EncodeToString(fragment.LinkKey) != tv.Str(t, v, "linkKey") ||
		fragment.Author != tv.Str(t, v, "author") || vectorAddress != tv.Str(t, v, "address") || hex.EncodeToString(vectorKey) != tv.Str(t, v, "encryptionKey") {
		t.Errorf("the vector's URL: %+v", fragment)
	}
	typ, value, n := opened(t, eventOf(t, v, "event"), vectorKey)
	if typ != proto.CheckoutRecordType || n%256 != 0 {
		t.Errorf("the vector's event: type %s, %d bytes", typ, n)
	}
	same(t, "the vector's value", value, fieldOf(v, "value"))
	if record, err := checkoutlink.ParseValue(value); err != nil || record.Sats != sats || record.RedirectShown != "shop.example" {
		t.Errorf("the vector's record %+v, %v", record, err)
	}
}

// vectors/app/checkout-rest.json: a checkout link to an invoice with an address, then a payment
// check that sees 20 000 of its 49 000 sats on-chain and asks an invoice of the rest. Every value
// the package derives (the invoice, the link's value with its rest author and key, the rest keys,
// the rest record's address and value, the status) is the vector's; its events, of its own
// randomness, are valid and open as the vector's do.
func TestTheRestOfTheAppsVectorIsWhatThePackageMakes(t *testing.T) {
	v := tv.Load(t, "app/checkout-rest.json")
	link, rest := tv.Obj(t, v, "link"), tv.Obj(t, v, "rest")
	address := tv.Str(t, v, "onchainAddress")
	createdAt, expiresAt := tv.Int(t, v, "createdAt"), tv.Int(t, v, "expiresAt")
	m := testkit.NewMerchant()
	if hex.EncodeToString(m.IntegrationKey) != tv.Str(t, v, "integrationKey") || tv.Str(t, v, "origin") != origin {
		t.Fatal("another merchant")
	}
	m.KeyRelays = vectorStrings(t, v, "relays")
	m.SetNow(createdAt)
	m.Start(nil, nil)
	t.Cleanup(m.Stop)
	m.PublishChannel(m.ChannelValue(m.Now()-60, jsonv.Member{Name: "checkout", Value: jsonv.O("origin", origin)}), m.Now()-60)
	m.PublishLot([]string{address}, m.Now()-30)
	payRequest := tv.Obj(t, v, "payRequest")
	for _, name := range payRequest.Keys() {
		m.SetPay(pay(name, fieldOf(payRequest, name)))
	}
	linkValue := tv.Obj(t, link, "value")
	answering(t, m, map[int64][2]string{
		49_000_000: {tv.Str(t, linkValue, "invoice"), tv.Str(t, linkValue, "verifyUrl")},
		29_000_000: {tv.Str(t, rest, "invoice"), tv.Str(t, rest, "verifyUrl")},
	})
	o := tv.Obj(t, v, "order")
	c := clientOf(t, m)
	invoice, err := c.CreateInvoice(ctx, checkout.Order{
		Ref: tv.Str(t, o, "ref"), Sats: 49_000, Note: tv.Str(t, o, "note"), DurationSeconds: int(tv.Int(t, o, "durationSeconds")),
		Checkout: true, RedirectURL: tv.Str(t, o, "redirectUrl"),
	})
	must(t, err)
	if invoice.OnchainAddress != address || itoa(invoice.Sats) != tv.Str(t, v, "sats") || invoice.CreatedAt != createdAt || invoice.ExpiresAt != expiresAt {
		t.Fatalf("%+v", invoice)
	}
	l := openLink(t, m, invoice.CheckoutURL)
	same(t, "the link's value", l.value, linkValue)
	// 20 000 sats in the mempool, a minute later: the check asks an invoice of the 29 000 left.
	m.SetNow(m.Now() + 60)
	transaction := tv.Obj(t, v, "transaction")
	m.Chain.Add(address, testkit.Payment(tv.Str(t, transaction, "txid"), address, 20_000, nil, 0))
	same(t, "status", statusJSON(t, c, invoice), fieldOf(v, "status"))
	keys := derive.RestKeys(m.IntegrationKey, invoice.Ref, address, expiresAt)
	if hex.EncodeToString(keys.Key) != tv.Str(t, rest, "restKey") || keys.Author != tv.Str(t, rest, "restAuthor") ||
		hex.EncodeToString(keys.EncryptionKey) != tv.Str(t, rest, "encryptionKey") ||
		derive.RestRecordAddress(keys.Key, tv.Str(t, rest, "paymentHash")) != tv.Str(t, rest, "address") {
		t.Error("the rest keys")
	}
	relay := m.Net.Get(m.KeyRelays[0])
	var record *nostr.Event
	for _, e := range relay.Received() {
		if e.PubKey == keys.Author {
			record = e
		}
	}
	if record == nil {
		t.Fatal("no rest record")
	}
	same(t, "the rest record's tags", record.Tags, [][]string{{"d", tv.Str(t, rest, "address")}, {"expiration", itoa(expiresAt + 604_800)}})
	typ, value, n := opened(t, record, keys.EncryptionKey)
	if typ != proto.RestRecordType || n%256 != 0 {
		t.Errorf("type %s, %d bytes", typ, n)
	}
	same(t, "the rest record's value", value, fieldOf(rest, "value"))
	// The vector's events open as the web app opens them.
	vectorLink := eventOf(t, link, "event")
	linkKey, err := hex.DecodeString(tv.Str(t, link, "encryptionKey"))
	must(t, err)
	_, vectorValue, _ := opened(t, vectorLink, linkKey)
	same(t, "the vector's link", vectorValue, linkValue)
	parsed, err := checkoutlink.ParseValue(vectorValue)
	if err != nil || parsed.Rest == nil || parsed.Rest.Author != keys.Author || hex.EncodeToString(parsed.Rest.Key) != tv.Str(t, rest, "restKey") {
		t.Errorf("the vector's link record %+v, %v", parsed, err)
	}
	restInvoice, _, newer, ok := onchain.OpenRestRecord(testkit.EventValue(eventOf(t, rest, "event")), onchain.ReaderOf(keys),
		onchain.RestLinkTerms{Sats: invoice.Sats, CreatedAt: invoice.CreatedAt, ExpiresAt: invoice.ExpiresAt}, nil)
	if !ok || newer || restInvoice.Bolt11 != tv.Str(t, rest, "invoice") || restInvoice.Sats != 29_000 {
		t.Errorf("the vector's rest record %+v", restInvoice)
	}
}

// vectors/app/implementation-declaration.json: the declaration the merchant's app reads as the
// owner of the sale ORD-2001 (and as a second sale claiming the payment of app.json's
// declaration). The vector's event opens as the app opens it; one the package makes end to end
// for the same order (the app's integration key and registry, the same invoice, price, note and
// date) opens the same way to exactly the same inside.
func TestTheImplementationDeclarationOfTheAppsVectorIsWhatThePackageMakes(t *testing.T) {
	app, v := tv.Load(t, "app/app.json"), tv.Load(t, "app/implementation-declaration.json")
	parsed, channel := tv.Obj(t, app, "parsed"), tv.Obj(t, app, "channel")
	channelValue := tv.Obj(t, tv.Obj(t, channel, "value"), "v")
	integrationKey, err := hex.DecodeString(tv.Str(t, parsed, "integrationKey"))
	must(t, err)
	registry := declaration.RegistryOf(tv.Str(t, channelValue, "registryKey"), tv.Str(t, channelValue, "registryReader"))
	ref := tv.Str(t, v, "ref")
	sale, err := derive.SaleOf(integrationKey, registry, ref)
	must(t, err)
	if sale.Author != tv.Str(t, v, "saleAuthor") || sale.Taker != tv.Str(t, v, "taker") {
		t.Fatal("another sale")
	}
	theirs := eventOf(t, v, "event")
	m := testkit.NewMerchant()
	m.KeyRelays = vectorStrings(t, parsed, "relays")
	m.SetNow(theirs.CreatedAt)
	m.Start(nil, nil)
	t.Cleanup(m.Stop)
	channelEvent, _ := channel.Get("event")
	m.Net.Get(m.KeyRelays[0]).Store(channelEvent)
	sats, _ := strconv.ParseInt(tv.Str(t, v, "sats"), 10, 64)
	answering(t, m, map[int64][2]string{sats * 1000: {tv.Str(t, v, "invoice"), tv.Str(t, v, "verifyUrl")}})
	fiat := tv.Obj(t, v, "fiat")
	c, err := checkout.New(tv.Str(t, app, "secretKey"), checkout.WithHTTPTransport(m.HTTP),
		checkout.WithWebSocketDialer(checkout.NewWebSocketDialer(m.Net.Transport())), checkout.WithClock(m.Clock()),
		checkout.WithRelayTimeout(time.Second), checkout.WithLogger(quiet()), checkout.WithRateSource(price("99000")))
	must(t, err)
	// 0.02 CHF at 99 000 CHF/BTC: 21 sats.
	if tv.Int(t, fiat, "minor") != 2 || tv.Int(t, fiat, "digits") != 2 || tv.Str(t, fiat, "currency") != "CHF" {
		t.Fatal("another price")
	}
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: ref, Fiat: &checkout.Price{Amount: "0.02", Currency: "CHF"},
		Note: tv.Str(t, v, "note"), DurationSeconds: 600})
	must(t, err)
	if invoice.Sats != sats || invoice.Bolt11 != tv.Str(t, v, "invoice") || invoice.PaymentHash != tv.Str(t, v, "paymentHash") {
		t.Fatalf("%+v", invoice)
	}
	var ours *nostr.Event
	for _, e := range m.Net.Get(m.KeyRelays[0]).Received() {
		if e.PubKey == sale.Author {
			ours = e
		}
	}
	if ours == nil {
		t.Fatal("not declared")
	}
	d := derive.RegistryAddress(registry.Key, invoice.PaymentHash)
	same(t, "tags", ours.Tags, [][]string{{"d", d}})
	same(t, "the vector's tags", theirs.Tags, [][]string{{"d", d}})
	inside := func(e *nostr.Event) any {
		typ, outside, n := opened(t, e, derive.OuterKey(registry.Key, d, sale.Author))
		if typ != proto.DeclarationRecordType || n%256 != 0 || e.Kind != proto.RecordKind || e.CreatedAt != theirs.CreatedAt {
			t.Errorf("type %s, %d bytes, kind %v, at %d", typ, n, e.Kind, e.CreatedAt)
		}
		sealed, _ := fieldOf(outside, "sealed").(string)
		plaintext, err := nip44.Decrypt(sealed, sale.InnerKey)
		must(t, err)
		v, err := jsonv.ParseString(plaintext)
		must(t, err)
		return v
	}
	same(t, "inside", inside(ours), inside(theirs))
	// Both open as the sale's declaration of the invoice.
	for _, e := range []*nostr.Event{ours, theirs} {
		if got, newer, ok := declaration.Open(testkit.EventValue(e), sale, nil); !ok || newer || got.Invoice != invoice.Bolt11 || got.Note != tv.Str(t, v, "note") {
			t.Errorf("opened %+v", got)
		}
	}
}
