package checkout_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/testkit"
)

var ctx = context.Background()

// pay is a member of the pay request.
func pay(name string, value any) jsonv.Member { return jsonv.Member{Name: name, Value: value} }

// answerAt answers the requests whose URL holds part with a.
func answerAt(part string, a testkit.Answer) func(*http.Request) (testkit.Answer, bool) {
	return func(req *http.Request) (testkit.Answer, bool) {
		if strings.Contains(req.URL.String(), part) {
			return a, true
		}
		return testkit.Answer{}, false
	}
}

// callbackCall is the request of the callback, with its query.
func callbackCall(t *testing.T, m *testkit.Merchant) testkit.Call {
	t.Helper()
	for _, c := range m.HTTP.Calls() {
		if strings.HasSuffix(c.URL.Path, "/invoice") {
			return c
		}
	}
	t.Fatal("no callback asked")
	return testkit.Call{}
}

func queryOf(c testkit.Call) map[string]string {
	out := map[string]string{}
	for name, values := range c.URL.Query() {
		out[name] = values[0]
	}
	return out
}

func TestCreateInvoiceKeepsTheCallbacksOwnQueryAndDropsItsFragment(t *testing.T) {
	m := newMerchant(t)
	m.SetPay(pay("callback", testkit.Callback+"?k=1#tag"))
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "ORD-1", Sats: 1000})
	must(t, err)
	call := callbackCall(t, m)
	if call.URL.Fragment != "" {
		t.Errorf("fragment %q", call.URL.Fragment)
	}
	same(t, "query", queryOf(call), map[string]string{"k": "1", "amount": "1000000", "expiry": "900", "comment": "ORD-1"})
}

func TestCreateInvoiceRefusesACallbackThatIsNotAnHTTPSURL(t *testing.T) {
	m := newMerchant(t)
	for _, callback := range []string{"https://10.0.0.1/cb", "https://pay.localhost/cb", "https://user@breez.tips/cb"} {
		m.SetPay(pay("callback", callback))
		_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "ORD-1", Sats: 1000})
		wantCode(t, err, checkout.CodeInvalidInvoice)
	}
	for _, c := range m.HTTP.Calls() {
		if strings.HasSuffix(c.URL.Path, "/cb") {
			t.Errorf("asked %s", c.URL.Host)
		}
	}
}

// SPEC.md §6, §16: a pay request whose metadata holds a lone surrogate (an escape in its JSON) is
// valid JSON but no text: it has no UTF-8 to hash. invalid_invoice before the callback is asked,
// whatever an invoice's description hash says (here the SHA-256 of its WTF-8 bytes).
func TestCreateInvoiceRefusesAPayRequestWhoseMetadataHoldsALoneSurrogate(t *testing.T) {
	m := newMerchant(t)
	// U+D800 as Go holds a lone surrogate read from JSON: its WTF-8 bytes.
	metadata := "[[\"text/plain\",\"Pay \xed\xa0\x80\"]]"
	m.SetPay(pay("metadata", metadata))
	m.SetTamper(func(made *testkit.MadeInvoice) {
		made.PR = testkit.MakeInvoice(testkit.Invoice{AmountMsat: made.AmountMsat, HasAmount: true, PaymentHash: made.PaymentHash,
			DescriptionHash: testkit.MetadataHash(metadata), Timestamp: m.Now(), ExpirySeconds: made.Expiry, HasExpiry: true})
	})
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "ORD-1", Sats: 1000})
	wantCode(t, err, checkout.CodeInvalidInvoice)
	for _, c := range m.HTTP.Calls() {
		if strings.HasSuffix(c.URL.Path, "/invoice") {
			t.Error("the callback was asked")
		}
	}
	if n := len(declarations(m)); n != 0 {
		t.Errorf("%d declarations", n)
	}
}

func TestCreateInvoiceFailsWithNetworkOnACallbackItCannotReach(t *testing.T) {
	m := newMerchant(t)
	// An https URL by its rule (SPEC.md §15), on a host nothing answers.
	m.SetPay(pay("callback", "https://xn--abc.example/cb"))
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "ORD-1", Sats: 1000})
	wantCode(t, err, checkout.CodeNetwork)
}

func TestCreateInvoiceAsksChecksAndDeclaresTheInvoiceOfAnOrderInSats(t *testing.T) {
	m := newMerchant(t)
	invoice, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "ORD-1042", Sats: 49_000, Note: "Order 1042"})
	must(t, err)
	made := m.Invoices()[0]
	same(t, "invoice", jsonOf(t, invoice), jsonv.O(
		"ref", "ORD-1042",
		"bolt11", made.PR,
		"paymentHash", made.PaymentHash,
		"verifyUrl", made.Verify,
		// Within the Lightning address's bounds, and no lot of addresses: Lightning only.
		"onchainAddress", nil,
		"paymentUri", "lightning:"+made.PR,
		"sats", "49000",
		"fiat", nil,
		"rate", nil,
		"note", "Order 1042",
		"createdAt", m.Now(),
		"expiresAt", m.Now()+900,
		// No checkout link was asked.
		"checkoutUrl", nil,
		"redirectUrl", nil,
	))
	same(t, "query", queryOf(callbackCall(t, m)), map[string]string{"amount": "49000000", "expiry": "900", "comment": "Order 1042"})
	// Every request, with the User-Agent every implementation sends (SPEC.md §15).
	_, err = clientOf(t, m).GetStatus(ctx, invoice)
	must(t, err)
	calls := m.HTTP.Calls()
	if len(calls) < 3 {
		t.Fatalf("%d requests", len(calls))
	}
	for _, c := range calls {
		same(t, "headers", c.Headers, http.Header{"Accept": {"application/json"}, "User-Agent": {"sattleme-Checkout/1"}})
	}
	// Declared on every relay of the account (a majority before the invoice is given, the
	// others a moment later), signed by the order's sale.
	testkit.Eventually(t, func() bool { return len(declarations(m)) == 3 }, "3 declarations")
	events := declarations(m)
	key, _, author := derive.SaleKeys(m.IntegrationKey, "ORD-1042")
	if _, valid := nostr.Valid(testkit.EventValue(events[0])); !valid || events[0].PubKey != author {
		t.Error("not a valid event of the sale")
	}
	inside, err := testkit.OpenAsOwner(events[0], m.RegistryKey, m.ReaderSecret)
	must(t, err)
	same(t, "declaration", inside, jsonv.O(
		"version", 1,
		"invoice", made.PR,
		"verifyUrl", made.Verify,
		"sale", jsonv.O("key", hex.EncodeToString(key), "fiat", jsonv.O("minor", 49000, "digits", 0, "currency", "SAT"), "note", "Order 1042"),
		"taker", derive.TakerTag(derive.TakerKey(m.IntegrationKey), author),
	))
}

func TestCreateInvoiceNamesTheOrderByItsReferenceAndCommentsOnlyAsAllowed(t *testing.T) {
	m := newMerchant(t)
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "A-1", Sats: 100})
	must(t, err)
	m.SetPay(pay("commentAllowed", 2))
	_, err = clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "A-2", Sats: 100, DurationSeconds: 60})
	must(t, err)
	m.SetPay(pay("commentAllowed", testkit.Absent))
	_, err = clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "A-3", Sats: 100, DurationSeconds: 3600})
	must(t, err)
	var comments []any
	for _, made := range m.Invoices() {
		var comment any
		if made.HasComment {
			comment = made.Comment
		}
		comments = append(comments, []any{comment, made.Expiry})
	}
	same(t, "comments", comments, []any{[]any{"A-1", 900}, []any{nil, 60}, []any{nil, 3600}})
	// A-1's declaration (the relays past the majority may take it after A-2's).
	var first *nostr.Event
	for _, e := range declarations(m) {
		if e.PubKey == m.Sale("A-1").Author {
			first = e
		}
	}
	if first == nil {
		t.Fatal("A-1 not declared")
	}
	inside, err := testkit.OpenAsOwner(first, m.RegistryKey, m.ReaderSecret)
	must(t, err)
	if note, _ := jsonv.Field(fieldOf(inside, "sale"), "note"); note != "A-1" {
		t.Errorf("note %v", note)
	}
}

func TestCreateInvoiceRefusesAnInvalidOrderBeforeAnyRequest(t *testing.T) {
	fiat := &checkout.Price{Amount: "1", Currency: "CHF"}
	custom := checkout.CustomRateSource(func(context.Context, string) (checkout.RateQuote, error) {
		return checkout.RateQuote{Price: "1"}, nil
	})
	cases := []struct {
		param string
		order checkout.Order
	}{
		{"ref", checkout.Order{Sats: 1}},
		{"ref", checkout.Order{Ref: strings.Repeat("x", 65), Sats: 1}},
		{"ref", checkout.Order{Ref: "CMD 1", Sats: 1}},
		{"ref", checkout.Order{Ref: "CMD/1", Sats: 1}},
		{"ref", checkout.Order{Ref: "Bestellung-\xc3\xa4", Sats: 1}},
		{"ref", checkout.Order{Ref: "CMD-1\n", Sats: 1}},
		{"sats", checkout.Order{Ref: "A"}},
		{"sats", checkout.Order{Ref: "A", Sats: 1, Fiat: fiat}},
		{"sats", checkout.Order{Ref: "A", Sats: -1}},
		{"sats", checkout.Order{Ref: "A", Sats: 2_100_000_000_000_001}},
		{"sats", checkout.Order{Ref: "A", Sats: math.MaxInt64}},
		{"durationSeconds", checkout.Order{Ref: "A", Sats: 1, DurationSeconds: 59}},
		{"durationSeconds", checkout.Order{Ref: "A", Sats: 1, DurationSeconds: 3601}},
		{"durationSeconds", checkout.Order{Ref: "A", Sats: 1, DurationSeconds: -900}},
		{"note", checkout.Order{Ref: "A", Sats: 1, Note: strings.Repeat("n", 101)}},
		{"note", checkout.Order{Ref: "A", Sats: 1, Note: "line\nbreak"}},
		{"note", checkout.Order{Ref: "A", Sats: 1, Note: "\xff"}},
		{"fiat", checkout.Order{Ref: "A", Fiat: &checkout.Price{Amount: "24.501", Currency: "CHF"}}},
		{"fiat", checkout.Order{Ref: "A", Fiat: &checkout.Price{Amount: "24.50", Currency: "SAT"}}},
		{"fiat", checkout.Order{Ref: "A", Fiat: &checkout.Price{Amount: "24.50", Currency: "chf"}}},
		{"rateSource", checkout.Order{Ref: "A", Sats: 1, RateSources: []checkout.RateSource{custom}}},
		{"rateSource", checkout.Order{Ref: "A", Sats: 1, RateSources: []checkout.RateSource{checkout.Kraken}}},
		{"rateSource", checkout.Order{Ref: "A", Fiat: fiat, RateSources: []checkout.RateSource{{}}}},
		{"rateSource", checkout.Order{Ref: "A", Fiat: fiat, RateSources: []checkout.RateSource{}}},
		{"rateSource", checkout.Order{Ref: "A", Fiat: fiat, RateSources: []checkout.RateSource{checkout.CustomRateSource(nil)}}},
		{"rateSource", checkout.Order{Ref: "A", Fiat: fiat, RateSources: []checkout.RateSource{checkout.Kraken, checkout.Kraken}}},
		{"rateSource", checkout.Order{Ref: "A", Sats: 1, RateSources: []checkout.RateSource{checkout.Kraken, checkout.CoinGecko}}},
		{"redirectUrl", checkout.Order{Ref: "A", Sats: 1, RedirectURL: "https://shop.example/thanks"}},
	}
	m := newMerchant(t)
	for _, c := range cases {
		_, err := clientOf(t, m).CreateInvoice(ctx, c.order)
		wantCode(t, err, checkout.CodeInvalidOrder, c.param)
	}
	if len(m.Invoices()) != 0 || len(declarations(m)) != 0 || len(m.HTTP.Calls()) != 0 {
		t.Error("asked before the order was checked")
	}
	for _, url := range m.KeyRelays {
		if n := m.Net.Connections(url); n != 0 {
			t.Errorf("%s: %d connections", url, n)
		}
	}
}

func TestCreateInvoiceAcceptsTheEdges(t *testing.T) {
	m := newMerchant(t)
	ref := strings.Repeat("A", 60) + ".:_-"
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: ref, Sats: 1, Note: strings.Repeat("\xc3\xa9", 100), DurationSeconds: 60})
	must(t, err)
	testkit.Eventually(t, func() bool { return len(declarations(m)) == 3 }, "3 declarations")
	// Spaces around a note are not part of it.
	_, err = clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "A-2", Sats: 1, Note: "  " + strings.Repeat("n", 100) + "  "})
	must(t, err)
}

func TestCreateInvoiceKeepsTheAmountWithinTheLightningAddressBoundsRoundedInwards(t *testing.T) {
	m := newMerchant(t)
	m.SetPay(pay("minSendable", 10_001), pay("maxSendable", 1_000_999))
	c := clientOf(t, m)
	merchant, err := c.Merchant(ctx)
	must(t, err)
	if merchant.MinSats != 11 || merchant.MaxSats != 1000 {
		t.Errorf("bounds %d to %d", merchant.MinSats, merchant.MaxSats)
	}
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "B-10", Sats: 10})
	wantCode(t, err, checkout.CodeInvalidOrder, "sats")
	// From 1 000 sats on, on-chain only: no address without a lot.
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "B-1001", Sats: 1001})
	if e := wantCode(t, err, checkout.CodeOnchainUnavailable); !e.Retryable() {
		t.Error("onchain_unavailable is retryable")
	}
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "B-11", Sats: 11})
	must(t, err)
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "B-1000", Sats: 1000})
	must(t, err)
	if n := len(m.Invoices()); n != 2 {
		t.Errorf("%d invoices", n)
	}
}

func TestCreateInvoiceRefusesAnInvoiceThatFailsAChecksAndDeclaresNothing(t *testing.T) {
	m := newMerchant(t)
	metadata := testkit.MetadataHash(testkit.Metadata)
	remake := func(f func(made *testkit.MadeInvoice, in *testkit.Invoice)) func(*testkit.MadeInvoice) {
		return func(made *testkit.MadeInvoice) {
			in := testkit.Invoice{AmountMsat: made.AmountMsat, HasAmount: true, PaymentHash: made.PaymentHash, DescriptionHash: metadata,
				Timestamp: m.Now(), ExpirySeconds: 900, HasExpiry: true}
			f(made, &in)
			made.PR = testkit.MakeInvoice(in)
		}
	}
	cases := []struct {
		name   string
		tamper func(*testkit.MadeInvoice)
	}{
		{"an invoice of another network", remake(func(_ *testkit.MadeInvoice, in *testkit.Invoice) { in.Network = "tb" })},
		{"another amount", remake(func(_ *testkit.MadeInvoice, in *testkit.Invoice) { in.AmountMsat -= 1000 })},
		{"no amount", remake(func(_ *testkit.MadeInvoice, in *testkit.Invoice) { in.HasAmount = false })},
		{"another merchant's invoice", remake(func(_ *testkit.MadeInvoice, in *testkit.Invoice) { in.DescriptionHash = testkit.MetadataHash("[]") })},
		{"a longer expiry", remake(func(_ *testkit.MadeInvoice, in *testkit.Invoice) { in.ExpirySeconds = 901 })},
		{"no expiry (3600 s)", remake(func(_ *testkit.MadeInvoice, in *testkit.Invoice) { in.HasExpiry = false })},
		{"a date 5 minutes off", remake(func(_ *testkit.MadeInvoice, in *testkit.Invoice) { in.Timestamp -= 301 })},
		{"a date in the future", remake(func(_ *testkit.MadeInvoice, in *testkit.Invoice) { in.Timestamp += 301 })},
		{"a verify URL in http", func(made *testkit.MadeInvoice) { made.Verify = "http://breez.tips/lnurlp/verify/" + made.PaymentHash }},
		{"a verify URL on another host", func(made *testkit.MadeInvoice) { made.Verify = "https://evil.test/verify/" + made.PaymentHash }},
		{"not an invoice", func(made *testkit.MadeInvoice) { made.PR = "lnbc1nope" }},
	}
	for _, c := range cases {
		m.SetTamper(c.tamper)
		_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "C-1", Sats: 1000})
		if e := errorOf(t, err); e.Code != checkout.CodeInvalidInvoice {
			t.Errorf("%s: %s", c.name, e.Code)
		}
	}
	if n := len(declarations(m)); n != 0 {
		t.Errorf("%d declarations", n)
	}
}

func TestCreateInvoiceRefusesAnInvoiceDatedWithin5MinutesButAlreadyOver(t *testing.T) {
	m := newMerchant(t)
	m.SetTamper(func(made *testkit.MadeInvoice) {
		made.PR = testkit.MakeInvoice(testkit.Invoice{AmountMsat: made.AmountMsat, HasAmount: true, PaymentHash: made.PaymentHash,
			DescriptionHash: testkit.MetadataHash(testkit.Metadata), Timestamp: m.Now() - 200, ExpirySeconds: 60, HasExpiry: true})
	})
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "C-3", Sats: 1000, DurationSeconds: 60})
	wantCode(t, err, checkout.CodeInvalidInvoice)
	if n := len(declarations(m)); n != 0 {
		t.Errorf("%d declarations", n)
	}
}

func TestCreateInvoiceRefusesAValidInvoiceTooLargeToBeDeclared(t *testing.T) {
	m := newMerchant(t)
	// 70 unknown fields of 1023 words: some 72 000 characters, more than a NIP-44 plaintext holds.
	var extra []testkit.Tagged
	for i := 0; i < 70; i++ {
		extra = append(extra, testkit.Tagged{Tag: 31, Data: make([]int, 1023)})
	}
	m.SetTamper(func(made *testkit.MadeInvoice) {
		made.PR = testkit.MakeInvoice(testkit.Invoice{AmountMsat: made.AmountMsat, HasAmount: true, PaymentHash: made.PaymentHash,
			DescriptionHash: testkit.MetadataHash(testkit.Metadata), Timestamp: m.Now(), ExpirySeconds: 900, HasExpiry: true, Extra: extra})
	})
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "C-4", Sats: 1000})
	e := wantCode(t, err, checkout.CodeInvalidInvoice)
	if !strings.Contains(e.Message, "too large to be declared") {
		t.Errorf("message %q", e.Message)
	}
	if n := len(declarations(m)); n != 0 {
		t.Errorf("%d declarations", n)
	}
}

func TestCreateInvoiceAcceptsAnInvoiceDatedWithin5Minutes(t *testing.T) {
	m := newMerchant(t)
	m.SetTamper(func(made *testkit.MadeInvoice) {
		made.PR = testkit.MakeInvoice(testkit.Invoice{AmountMsat: made.AmountMsat, HasAmount: true, PaymentHash: made.PaymentHash,
			DescriptionHash: testkit.MetadataHash(testkit.Metadata), Timestamp: m.Now() - 300, ExpirySeconds: 899, HasExpiry: true})
	})
	invoice, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "C-2", Sats: 1000})
	must(t, err)
	if invoice.ExpiresAt != m.Now()-300+899 {
		t.Errorf("expiresAt %d", invoice.ExpiresAt)
	}
}

func TestCreateInvoiceRefusesAnAnswerWithoutAnInvoiceOrTheServicesRefusal(t *testing.T) {
	m := newMerchant(t)
	m.SetOverride(answerAt("/invoice", testkit.JSON(jsonv.O("verify", "https://breez.tips/v"))))
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "D-1", Sats: 1})
	wantCode(t, err, checkout.CodeInvalidInvoice)
	m.SetOverride(answerAt("/invoice", testkit.Status(400, jsonv.O("status", "ERROR", "reason", "Amount out of range"))))
	_, err = clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "D-2", Sats: 1})
	wantCode(t, err, checkout.CodeNetwork)
	m.SetOverride(answerAt("/invoice", testkit.Status(502, jsonv.O())))
	_, err = clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "D-3", Sats: 1})
	wantCode(t, err, checkout.CodeNetwork)
	if n := len(declarations(m)); n != 0 {
		t.Errorf("%d declarations", n)
	}
}

func TestCreateInvoiceGivesTheInvoiceOnceAMajorityTookItsDeclaration(t *testing.T) {
	m := newMerchant(t)
	m.Net.Get("wss://relay-a.test").SetBehaviour(testkit.Behaviour{Refuse: "blocked: no"})
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "E-1", Sats: 1})
	must(t, err)
}

func TestCreateInvoiceGivesNoInvoiceWithoutAMajority(t *testing.T) {
	m := newMerchant(t)
	m.Net.Get("wss://relay-a.test").SetBehaviour(testkit.Behaviour{Refuse: "blocked: no"})
	m.Net.Get("wss://relay-b.test").SetBehaviour(testkit.Behaviour{Refuse: "error: internal error"})
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "E-2", Sats: 1})
	wantCode(t, err, checkout.CodeNetwork)
	m.Net.Get("wss://relay-b.test").SetBehaviour(testkit.Behaviour{})
	m.Net.Down("wss://relay-c.test")
	_, err = clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "E-3", Sats: 1})
	wantCode(t, err, checkout.CodeNetwork)
}

func TestCreateInvoiceAuthenticatesToARelayAsTheOrdersSale(t *testing.T) {
	m := newMerchant(t)
	m.Net.Get("wss://relay-a.test").SetBehaviour(testkit.Behaviour{RequireAuth: true})
	m.Net.Get("wss://relay-b.test").SetBehaviour(testkit.Behaviour{RequireAuth: true})
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "E-4", Sats: 1})
	must(t, err)
	_, _, sale := derive.SaleKeys(m.IntegrationKey, "E-4")
	relayB := m.Net.Get("wss://relay-b.test")
	testkit.Eventually(t, func() bool { return len(eventsOf(relayB, sale)) == 1 }, "the declaration on relay-b")
	if !contains(relayB.Authenticated(), sale) {
		t.Error("not authenticated as the sale")
	}
}

// eventsOf are the events relay holds by author.
func eventsOf(r *testkit.Relay, author string) []any {
	var out []any
	for _, e := range r.Events() {
		if pubkey, _ := jsonv.Field(e, "pubkey"); pubkey == author {
			out = append(out, e)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func TestCreateInvoiceDeclaresOnTheAccountsRelaysTheChannelNames(t *testing.T) {
	m := testkit.NewMerchant()
	account := []string{"wss://account-a.test", "wss://account-b.test"}
	m.Start(account, nil)
	t.Cleanup(m.Stop)
	m.PublishChannel(m.ChannelValue(100, jsonv.Member{Name: "relays", Value: account}), 100, append(append([]string{}, m.KeyRelays...), account...)...)
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "E-5", Sats: 1})
	must(t, err)
	if n := len(m.Net.Get("wss://account-b.test").Events()); n != 2 {
		t.Errorf("%d events on the account's relay", n)
	}
	if n := len(m.Net.Get("wss://relay-a.test").Received()); n != 0 {
		t.Errorf("%d events sent to the key's relay", n)
	}
}

func TestCreateInvoiceRefusesOnceTheMerchantRemovedTheIntegration(t *testing.T) {
	m := newMerchant(t)
	m.PublishChannel(jsonv.O("version", 1, "status", "removed", "updatedAt", 200), 200)
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "F-1", Sats: 1})
	wantCode(t, err, checkout.CodeRevoked)
	if n := len(m.HTTP.Calls()); n != 0 {
		t.Errorf("%d requests", n)
	}
}

func price(p string) checkout.RateSource {
	return checkout.CustomRateSource(func(context.Context, string) (checkout.RateQuote, error) {
		return checkout.RateQuote{Price: p}, nil
	})
}

func TestCreateInvoiceConvertsAPriceInACurrencyAtItsRateAndDeclaresThePrice(t *testing.T) {
	m := newMerchant(t)
	c := clientOf(t, m, checkout.WithRateSource(price("69012")))
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "G-1", Fiat: &checkout.Price{Amount: "24.50", Currency: "CHF"}, Note: "Order 1042"})
	must(t, err)
	got := jsonOf(t, invoice)
	same(t, "sats", jsonv.O("sats", fieldOf(got, "sats"), "fiat", fieldOf(got, "fiat"), "rate", fieldOf(got, "rate")), jsonv.O(
		"sats", "35502",
		"fiat", jsonv.O("amount", "24.50", "currency", "CHF", "minor", "2450", "digits", 2),
		"rate", jsonv.O("source", "custom", "currency", "CHF", "price", "69012", "fetchedAt", m.Now(), "time", nil),
	))
	if made := m.Invoices()[0]; made.AmountMsat != 35_502_000 {
		t.Errorf("asked %d msat", made.AmountMsat)
	}
	inside, err := testkit.OpenAsOwner(declarations(m)[0], m.RegistryKey, m.ReaderSecret)
	must(t, err)
	fiat, _ := jsonv.Field(fieldOf(inside, "sale"), "fiat")
	same(t, "declared price", fiat, jsonv.O("minor", 2450, "digits", 2, "currency", "CHF"))
	// Another source for one invoice.
	other, err := c.CreateInvoice(ctx, checkout.Order{Ref: "G-2", Fiat: &checkout.Price{Amount: "1500", Currency: "JPY"},
		RateSources: []checkout.RateSource{price("15000000")}})
	must(t, err)
	if other.Sats != 10_000 || other.Rate.Source != "custom" || other.Rate.Price != "15000000" {
		t.Errorf("%+v", other)
	}
}

func fieldOf(v any, name string) any {
	f, _ := jsonv.Field(v, name)
	return f
}

func TestCreateInvoiceRefusesAConvertedAmountOutsideTheBounds(t *testing.T) {
	m := newMerchant(t)
	m.SetPay(pay("minSendable", 100_000))
	_, err := clientOf(t, m, checkout.WithRateSource(price("100000"))).CreateInvoice(ctx, checkout.Order{Ref: "G-3", Fiat: &checkout.Price{Amount: "0.05", Currency: "CHF"}})
	wantCode(t, err, checkout.CodeInvalidOrder, "fiat")
}

func TestOneOrderOneInvoiceAtATime(t *testing.T) {
	m := newMerchant(t)
	c := clientOf(t, m)
	first, err := c.CreateInvoice(ctx, checkout.Order{Ref: "H-1", Sats: 1, DurationSeconds: 60})
	must(t, err)
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "H-1", Sats: 1})
	pending := wantCode(t, err, checkout.CodeInvoicePending)
	if pending.Invoice.Bolt11 != first.Bolt11 || pending.Invoice.ExpiresAt != first.ExpiresAt || pending.Invoice.Sats != 1 ||
		pending.Invoice.Fiat != nil || pending.Invoice.Note != "H-1" {
		t.Errorf("pending %+v", pending.Invoice)
	}
	// Kept as the order's Invoice: the one CreateInvoice gave (it had no rate nor link), stored and
	// checked as it is.
	kept := pending.Invoice.AsInvoice()
	same(t, "kept", jsonOf(t, kept), jsonOf(t, first))
	var stored checkout.Invoice
	must(t, json.Unmarshal(jsonv.Marshal(jsonOf(t, kept)), &stored))
	if status, err := c.GetStatus(ctx, &stored); err != nil || status.State != checkout.StateOpen {
		t.Errorf("kept: %+v, %v", status, err)
	}
	// Its end, then the 120 s a late payment may still be reported in.
	m.SetNow(first.ExpiresAt + 119)
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "H-1", Sats: 1})
	wantCode(t, err, checkout.CodeInvoicePending)
	m.SetNow(first.ExpiresAt + 120)
	second, err := c.CreateInvoice(ctx, checkout.Order{Ref: "H-1", Sats: 1})
	must(t, err)
	if second.Bolt11 == first.Bolt11 {
		t.Error("the same invoice")
	}
	// Another order is another sale.
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "H-2", Sats: 1})
	must(t, err)
}

func TestANewInvoiceForAnOrderAlreadyPaidIsRefused(t *testing.T) {
	m := newMerchant(t)
	c := clientOf(t, m)
	first, err := c.CreateInvoice(ctx, checkout.Order{Ref: "H-3", Sats: 1, DurationSeconds: 60})
	must(t, err)
	m.PayInvoice(first.PaymentHash)
	m.SetNow(first.ExpiresAt + 600)
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "H-3", Sats: 1})
	paid := wantCode(t, err, checkout.CodeOrderPaid)
	if paid.Preimage != m.Invoice(first.PaymentHash).Preimage {
		t.Errorf("preimage %q", paid.Preimage)
	}
}

func TestAnOrderPaidWhileItsInvoiceMayStillBePaidIsNeverHandedOutAgain(t *testing.T) {
	m := newMerchant(t)
	c := clientOf(t, m)
	first, err := c.CreateInvoice(ctx, checkout.Order{Ref: "H-4", Sats: 1})
	must(t, err)
	m.PayInvoice(first.PaymentHash)
	// Within its time: its payment checked even alone (SPEC.md §21, step 1).
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "H-4", Sats: 1})
	paid := wantCode(t, err, checkout.CodeOrderPaid)
	if paid.Invoice.Bolt11 != first.Bolt11 {
		t.Error("another invoice")
	}
	same(t, "status", jsonOf(t, paid.Status), jsonv.O("state", "paid", "method", "lightning", "preimage", m.Invoice(first.PaymentHash).Preimage))
}

func TestTheErrorOfThePaymentCheckOfTheOnlyInvoiceThatMayStillBePaid(t *testing.T) {
	m := newMerchant(t)
	c := clientOf(t, m)
	first, err := c.CreateInvoice(ctx, checkout.Order{Ref: "H-5", Sats: 1})
	must(t, err)
	// Its Lightning address does not answer: it may have been paid.
	m.SetOverride(answerAt(testkit.VerifyPrefix, testkit.Text(503, "")))
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "H-5", Sats: 1})
	wantCode(t, err, checkout.CodeNetwork)
	m.SetOverride(nil)
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "H-5", Sats: 1})
	if pending := wantCode(t, err, checkout.CodeInvoicePending); pending.Invoice.Bolt11 != first.Bolt11 {
		t.Error("another invoice")
	}
}

func TestAPendingInvoiceIsDeclaredOnAMajorityAgainBeforeItIsHandedOut(t *testing.T) {
	m := newMerchant(t)
	c := clientOf(t, m)
	a, b := m.Net.Get("wss://relay-a.test"), m.Net.Get("wss://relay-b.test")
	a.SetBehaviour(testkit.Behaviour{Refuse: "blocked: no"})
	b.SetBehaviour(testkit.Behaviour{Refuse: "blocked: no"})
	_, err := c.CreateInvoice(ctx, checkout.Order{Ref: "H-7", Sats: 1})
	wantCode(t, err, checkout.CodeNetwork)
	made := m.Invoices()[0]
	_, _, sale := derive.SaleKeys(m.IntegrationKey, "H-7")
	// Only relay-c holds its declaration (once the publication's tail reached it). relay-a never
	// answers the read of the sale's records (late, however busy the machine): relay-b and relay-c
	// are the majority read, and the declaration found is not handed out while the others refuse
	// it.
	testkit.Eventually(t, func() bool { return len(eventsOf(m.Net.Get("wss://relay-c.test"), sale)) == 1 }, "the declaration on relay-c")
	a.SetBehaviour(testkit.Behaviour{Refuse: "blocked: no", MuteQuery: readsOf(sale)})
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "H-7", Sats: 1})
	wantCode(t, err, checkout.CodeNetwork)
	// Now a majority takes it again: then, and only then, it is handed out.
	a.SetBehaviour(testkit.Behaviour{MuteQuery: readsOf(sale)})
	b.SetBehaviour(testkit.Behaviour{})
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "H-7", Sats: 1})
	if pending := wantCode(t, err, checkout.CodeInvoicePending); pending.Invoice.Bolt11 != made.PR {
		t.Error("another invoice")
	}
	for _, url := range m.KeyRelays {
		testkit.Eventually(t, func() bool { return len(eventsOf(m.Net.Get(url), sale)) == 1 }, url+" holds the declaration")
	}
	if n := len(m.Invoices()); n != 1 {
		t.Errorf("%d invoices", n)
	}
}

func TestAnotherInvoiceWhenTheMajorityReadLacksOneNeverHandedOut(t *testing.T) {
	m := newMerchant(t)
	c := clientOf(t, m)
	a, b, cc := m.Net.Get("wss://relay-a.test"), m.Net.Get("wss://relay-b.test"), m.Net.Get("wss://relay-c.test")
	a.SetBehaviour(testkit.Behaviour{Refuse: "blocked: no"})
	b.SetBehaviour(testkit.Behaviour{Refuse: "blocked: no"})
	// Declared on relay-c alone: "do not show this invoice".
	_, err := c.CreateInvoice(ctx, checkout.Order{Ref: "H-10", Sats: 1})
	wantCode(t, err, checkout.CodeNetwork)
	// relay-a and relay-b answer first, without it (relay-c never answers the read of the sale's
	// records): a new invoice, declared on a majority.
	_, _, sale := derive.SaleKeys(m.IntegrationKey, "H-10")
	a.SetBehaviour(testkit.Behaviour{})
	b.SetBehaviour(testkit.Behaviour{})
	cc.SetBehaviour(testkit.Behaviour{MuteQuery: readsOf(sale)})
	second, err := c.CreateInvoice(ctx, checkout.Order{Ref: "H-10", Sats: 1})
	must(t, err)
	invoices := m.Invoices()
	if len(invoices) != 2 || second.Bolt11 != invoices[1].PR {
		t.Errorf("%d invoices", len(invoices))
	}
}

func TestAPaidOrderIsRefusedEvenWhenAnotherCheckGetsNoAnswer(t *testing.T) {
	m := newMerchant(t)
	c := clientOf(t, m)
	first, err := c.CreateInvoice(ctx, checkout.Order{Ref: "H-8", Sats: 1, DurationSeconds: 60})
	must(t, err)
	m.SetNow(first.ExpiresAt + 120)
	second, err := c.CreateInvoice(ctx, checkout.Order{Ref: "H-8", Sats: 1, DurationSeconds: 60})
	must(t, err)
	m.PayInvoice(second.PaymentHash)
	m.SetNow(second.ExpiresAt + 600)
	m.SetOverride(answerAt(first.PaymentHash, testkit.Status(502, jsonv.O())))
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "H-8", Sats: 1})
	if paid := wantCode(t, err, checkout.CodeOrderPaid); paid.Invoice.Bolt11 != second.Bolt11 {
		t.Error("another invoice")
	}
	// Unpaid, one check without an answer: retry later.
	other, err := c.CreateInvoice(ctx, checkout.Order{Ref: "H-9", Sats: 1, DurationSeconds: 60})
	must(t, err)
	m.SetNow(other.ExpiresAt + 120)
	m.SetOverride(answerAt(other.PaymentHash, testkit.Status(502, jsonv.O())))
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "H-9", Sats: 1})
	wantCode(t, err, checkout.CodeNetwork)
}

func TestTwoCallsForOneOrderMakeOneInvoice(t *testing.T) {
	m := newMerchant(t)
	c := clientOf(t, m)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.CreateInvoice(ctx, checkout.Order{Ref: "H-4", Sats: 1})
		}(i)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Errorf("%v, %v", errs[0], errs[1])
	}
	if n := len(m.Invoices()); n != 1 {
		t.Errorf("%d invoices", n)
	}
}

func TestFindInvoicesFindsTheInvoicesDeclaredForAnOrderAndNeedsAMajority(t *testing.T) {
	m := newMerchant(t)
	c := clientOf(t, m, checkout.WithRateSource(price("69012")))
	first, err := c.CreateInvoice(ctx, checkout.Order{Ref: "H-5", Fiat: &checkout.Price{Amount: "24.50", Currency: "CHF"}, DurationSeconds: 60})
	must(t, err)
	m.SetNow(first.ExpiresAt + 120)
	second, err := c.CreateInvoice(ctx, checkout.Order{Ref: "H-5", Sats: 7})
	must(t, err)
	found, err := c.FindInvoices(ctx, "H-5")
	must(t, err)
	if len(found) != 2 || found[0].Bolt11 != first.Bolt11 || found[1].Bolt11 != second.Bolt11 {
		t.Fatalf("found %d", len(found))
	}
	same(t, "first", jsonOf(t, found[0]), jsonv.O(
		"ref", "H-5",
		"bolt11", first.Bolt11,
		"paymentHash", first.PaymentHash,
		"verifyUrl", first.VerifyURL,
		"onchainAddress", nil,
		"paymentUri", "lightning:"+first.Bolt11,
		"sats", "35502",
		"fiat", jsonv.O("amount", "24.50", "currency", "CHF", "minor", "2450", "digits", 2),
		"note", "H-5",
		"createdAt", first.CreatedAt,
		"expiresAt", first.ExpiresAt,
		"declaredAt", first.CreatedAt,
	))
	if found[1].Sats != 7 || found[1].Fiat != nil {
		t.Errorf("second %+v", found[1])
	}
	none, err := c.FindInvoices(ctx, "H-6")
	must(t, err)
	if len(none) != 0 {
		t.Errorf("%d invoices of another order", len(none))
	}
	_, err = c.FindInvoices(ctx, "H 6")
	wantCode(t, err, checkout.CodeInvalidOrder, "ref")
	m.Net.Down("wss://relay-a.test")
	m.Net.Down("wss://relay-b.test")
	_, err = c.FindInvoices(ctx, "H-5")
	wantCode(t, err, checkout.CodeNetwork)
}

func TestACallMakesAnInvoiceOverOneConnectionPerRelay(t *testing.T) {
	m := newMerchant(t)
	c := clientOf(t, m)
	first, err := c.CreateInvoice(ctx, checkout.Order{Ref: "R-1", Sats: 1, DurationSeconds: 60})
	must(t, err)
	// The declaration reaches every relay, the relays past the majority once the call returned.
	connections := func(n int) {
		for _, url := range m.KeyRelays {
			r := m.Net.Get(url)
			// Every subscription closed once read, and every connection once the call is done.
			testkit.Eventually(t, func() bool { return m.Net.Connections(url) == n && r.AllClosed() && r.Open() == 0 }, url+" closed")
		}
	}
	connections(1)
	m.SetNow(first.ExpiresAt + 120)
	// The channel, the order's declarations page after page, the new declaration: one connection.
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "R-1", Sats: 1, DurationSeconds: 60})
	must(t, err)
	connections(2)
}

func TestACallIsNotSlowedByARelayThatNeverAnswers(t *testing.T) {
	for _, kind := range []string{"mute", "blackhole"} {
		t.Run(kind, func(t *testing.T) {
			m := newMerchant(t)
			if kind == "mute" {
				m.Net.Get("wss://relay-c.test").SetBehaviour(testkit.Behaviour{Mute: true})
			} else {
				m.Net.Down("wss://relay-c.test")
				m.Net.AddBlackhole("wss://relay-c.test")
			}
			c := clientOf(t, m, checkout.WithRelayTimeout(30*time.Second))
			started := time.Now()
			_, err := c.CreateInvoice(ctx, checkout.Order{Ref: "R-2", Sats: 1})
			must(t, err)
			found, err := c.FindInvoices(ctx, "R-2")
			must(t, err)
			if len(found) != 1 {
				t.Errorf("found %d", len(found))
			}
			// A majority answered: never the relay's timeout.
			if elapsed := time.Since(started); elapsed > 15*time.Second {
				t.Errorf("took %v", elapsed)
			}
		})
	}
}

func TestACallStopsWhenItsContextEndsAndClosesItsConnections(t *testing.T) {
	m := newMerchant(t)
	for _, url := range m.KeyRelays {
		m.Net.Get(url).SetBehaviour(testkit.Behaviour{QueryDelay: 30 * time.Second})
	}
	call, cancel := context.WithCancel(ctx)
	time.AfterFunc(100*time.Millisecond, cancel)
	started := time.Now()
	_, err := clientOf(t, m, checkout.WithRelayTimeout(50*time.Second)).CreateInvoice(call, checkout.Order{Ref: "R-3", Sats: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	var e *checkout.Error
	if errors.As(err, &e) {
		t.Error("a cancellation is no error of the package")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("took %v", elapsed)
	}
	// The relays see each connection close a moment later.
	for _, url := range m.KeyRelays {
		r := m.Net.Get(url)
		testkit.Eventually(t, func() bool { return r.Open() == 0 }, url+" closed")
	}
	if n := len(m.Invoices()); n != 0 {
		t.Errorf("%d invoices", n)
	}
	// A deadline is the context's error too.
	deadline, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	_, err = clientOf(t, m, checkout.WithRelayTimeout(50*time.Second)).CreateInvoice(deadline, checkout.Order{Ref: "R-3", Sats: 1})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestRatesByName(t *testing.T) {
	m := newMerchant(t)
	m.SetOverride(func(req *http.Request) (testkit.Answer, bool) {
		switch req.URL.Host {
		case "api.coingecko.com":
			return testkit.JSON(jsonv.O("bitcoin", jsonv.O("chf", 69012, "last_updated_at", m.Now()-30))), true
		case "api.kraken.com":
			return testkit.JSON(jsonv.O("error", []any{}, "result", jsonv.O("XBTCHF", jsonv.O("c", []any{"70000.0", "0.1"})))), true
		}
		return testkit.Answer{}, false
	})
	c := clientOf(t, m, checkout.WithRateSource(checkout.CoinGecko), checkout.WithCoinGeckoAPIKey("demo-key"))
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "S-1", Fiat: &checkout.Price{Amount: "24.50", Currency: "CHF"}})
	must(t, err)
	if invoice.Sats != 35502 || invoice.Rate.Source != "coingecko" || invoice.Rate.Price != "69012" {
		t.Errorf("%+v %+v", invoice, invoice.Rate)
	}
	for _, call := range m.HTTP.Calls() {
		if call.URL.Host == "api.coingecko.com" && call.Headers.Get("x-cg-demo-api-key") != "demo-key" {
			t.Error("CoinGecko asked without the Demo key")
		}
	}
	other, err := c.CreateInvoice(ctx, checkout.Order{Ref: "S-2", Fiat: &checkout.Price{Amount: "7", Currency: "CHF"}, RateSources: []checkout.RateSource{checkout.Kraken}})
	must(t, err)
	if other.Rate.Source != "kraken" || other.Rate.Price != "70000" {
		t.Errorf("%+v", other.Rate)
	}
}

func TestAnUnknownSourceIsRefusedWhenTheClientIsMade(t *testing.T) {
	m := newMerchant(t)
	for _, sources := range [][]checkout.RateSource{{{}}, {}, {checkout.Kraken, checkout.Kraken}, {checkout.Kraken, {}}, nil, {checkout.CustomRateSource(nil)}} {
		_, err := checkout.New(m.Key(), checkout.WithRateSource(sources...))
		wantCode(t, err, checkout.CodeInvalidOption, "rateSource")
	}
	for _, apiKey := range []string{"", "demo key", "d\xc3\xa9mo", "demo\n"} {
		_, err := checkout.New(m.Key(), checkout.WithCoinGeckoAPIKey(apiKey))
		wantCode(t, err, checkout.CodeInvalidOption, "coingeckoApiKey")
	}
}

func TestWithoutRateSourceTheDefaultListIsAskedInOrder(t *testing.T) {
	m := newMerchant(t)
	m.SetOverride(func(req *http.Request) (testkit.Answer, bool) {
		switch req.URL.Host {
		case "api.coingecko.com":
			return testkit.Status(429, jsonv.O("status", jsonv.O("error_code", 429))), true
		case "mempool.space":
			if req.URL.Path == "/api/v1/prices" {
				return testkit.JSON(jsonv.O("time", m.Now()-10, "CHF", 69012)), true
			}
		}
		return testkit.Answer{}, false
	})
	logger := quiet()
	c := clientOf(t, m, checkout.WithLogger(logger))
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "D-1", Fiat: &checkout.Price{Amount: "24.50", Currency: "CHF"}})
	must(t, err)
	if invoice.Sats != 35502 || invoice.Rate.Source != "mempool" || invoice.Rate.Price != "69012" {
		t.Errorf("%+v", invoice.Rate)
	}
	var asked []any
	for _, call := range m.HTTP.Calls() {
		if !strings.HasSuffix(call.URL.Host, "breez.tips") {
			asked = append(asked, call.URL.Host)
			// No key given: CoinGecko is asked without one.
			if call.Headers.Get("x-cg-demo-api-key") != "" {
				t.Error("a Demo key sent")
			}
		}
	}
	same(t, "asked", asked, []any{"api.coingecko.com", "mempool.space"})
	same(t, "warnings", logger.Messages(), []string{"Rate source coingecko failed (network): coingecko answered HTTP 429; trying mempool"})
}

func TestAnOrdersListIsAskedInItsOrderAndOnlyThatList(t *testing.T) {
	m := newMerchant(t)
	m.SetOverride(func(req *http.Request) (testkit.Answer, bool) {
		switch req.URL.Host {
		case "api.kraken.com":
			return testkit.JSON(jsonv.O("error", []any{"EQuery:Unknown asset pair"})), true
		case "api.coinbase.com":
			return testkit.JSON(jsonv.O("data", jsonv.O("currency", "BTC", "rates", jsonv.O("CHF", "69012.00")))), true
		}
		return testkit.Answer{}, false
	})
	c := clientOf(t, m, checkout.WithRateSource(checkout.CoinGecko))
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "D-2", Fiat: &checkout.Price{Amount: "24.50", Currency: "CHF"},
		RateSources: []checkout.RateSource{checkout.Kraken, checkout.Coinbase}})
	must(t, err)
	if invoice.Rate.Source != "coinbase" || invoice.Rate.Price != "69012" {
		t.Errorf("%+v", invoice.Rate)
	}
	var hosts []string
	for _, call := range m.HTTP.Calls() {
		hosts = append(hosts, call.URL.Host)
	}
	if !contains(hosts, "api.kraken.com") || contains(hosts, "api.coingecko.com") {
		t.Errorf("asked %v", hosts)
	}
}

func TestTheLastNetworkOrRateErrorWhenNoSourceHasAPrice(t *testing.T) {
	m := newMerchant(t)
	m.SetOverride(func(req *http.Request) (testkit.Answer, bool) {
		switch req.URL.Host {
		case "api.kraken.com":
			return testkit.JSON(jsonv.O("error", []any{"EQuery:Unknown asset pair"})), true
		case "api.coinbase.com":
			return testkit.Status(503, jsonv.O()), true
		}
		return testkit.Answer{}, false
	})
	c := clientOf(t, m)
	_, err := c.CreateInvoice(ctx, checkout.Order{Ref: "D-3", Fiat: &checkout.Price{Amount: "1", Currency: "CHF"},
		RateSources: []checkout.RateSource{checkout.Coinbase, checkout.Kraken}})
	wantCode(t, err, checkout.CodeNetwork)
	// Only sources without the currency: unsupported_currency, naming them.
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "D-4", Fiat: &checkout.Price{Amount: "1", Currency: "THB"},
		RateSources: []checkout.RateSource{checkout.Kraken, checkout.Mempool}})
	e := wantCode(t, err, checkout.CodeUnsupportedCurrency)
	if e.Message != "None of kraken, mempool has a price in THB" || e.Currency != "THB" {
		t.Errorf("%q %q", e.Message, e.Currency)
	}
	if n := len(m.Invoices()); n != 0 {
		t.Errorf("%d invoices", n)
	}
}

func TestAnOrdersPendingInvoiceComesBeforeAFailingRateSource(t *testing.T) {
	m := newMerchant(t)
	_, err := clientOf(t, m, checkout.WithRateSource(price("69012"))).CreateInvoice(ctx, checkout.Order{Ref: "S-3", Fiat: &checkout.Price{Amount: "1", Currency: "CHF"}})
	must(t, err)
	failing := clientOf(t, m, checkout.WithRateSource(checkout.CustomRateSource(func(context.Context, string) (checkout.RateQuote, error) {
		return checkout.RateQuote{}, checkout.NewNetworkError("down", nil)
	})))
	_, err = failing.CreateInvoice(ctx, checkout.Order{Ref: "S-3", Fiat: &checkout.Price{Amount: "1", Currency: "CHF"}})
	wantCode(t, err, checkout.CodeInvoicePending)
	_, err = failing.CreateInvoice(ctx, checkout.Order{Ref: "S-4", Fiat: &checkout.Price{Amount: "1", Currency: "CHF"}})
	wantCode(t, err, checkout.CodeNetwork)
	if n := len(m.Invoices()); n != 1 {
		t.Errorf("%d invoices", n)
	}
}
