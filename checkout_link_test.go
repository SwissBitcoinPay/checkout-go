package checkout_test

import (
	"bytes"
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/checkoutlink"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/testkit"
)

// Checkout links (SPEC.md §30): asked with the order, published on a majority of the merchant's
// relays before the invoice is declared, read by the merchant's web app with what its URL holds
// alone.

const origin = "https://pay.example"

// checkoutMerchant is a merchant whose channel names a web app for checkout links.
func checkoutMerchant(t *testing.T) *testkit.Merchant {
	t.Helper()
	m := testkit.NewMerchant()
	m.Start(nil, nil)
	t.Cleanup(m.Stop)
	m.PublishChannel(m.ChannelValue(100, jsonv.Member{Name: "relays", Value: m.KeyRelays}, jsonv.Member{Name: "checkout", Value: jsonv.O("origin", origin)}), 100)
	return m
}

func linkClient(t *testing.T, m *testkit.Merchant, opts ...checkout.Option) *checkout.Client {
	t.Helper()
	return clientOf(t, m, append([]checkout.Option{checkout.WithRateSource(price("99000"))}, opts...)...)
}

// received is every event the first relay received, in order.
func received(m *testkit.Merchant) []*nostr.Event { return m.Net.Get(m.KeyRelays[0]).Received() }

func isCheckout(e *nostr.Event) bool {
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == "expiration" {
			return true
		}
	}
	return false
}

// link is a checkout link, read as the page reads it, with its record from the relays.
type link struct {
	fragment checkoutlink.Fragment
	event    *nostr.Event
	copies   []*nostr.Event
	value    any
	record   *checkoutlink.Record
}

func openLink(t *testing.T, m *testkit.Merchant, url string) link {
	t.Helper()
	return openLinkOn(t, m, url, 0)
}

// openLinkOn opens a link once needed relays hold it (0: every relay).
func openLinkOn(t *testing.T, m *testkit.Merchant, url string, needed int) link {
	t.Helper()
	if !strings.HasPrefix(url, origin+"/pay#c.") {
		t.Fatalf("not a link of the web app: %s", url)
	}
	fragment := checkoutlink.ParseFragment(url[strings.IndexByte(url, '#'):])
	if fragment.Status != checkoutlink.FragmentLink {
		t.Fatalf("not a link: %s", fragment.Status)
	}
	address, encryptionKey := derive.CheckoutKeys(fragment.LinkKey)
	var l link
	l.fragment = fragment
	copyOn := func(relay string) *nostr.Event {
		for _, v := range m.Net.Get(relay).Events() {
			e, ok := nostr.WithValidID(v)
			if ok && e.PubKey == fragment.Author {
				if d, _ := e.DTag(); d == address {
					return e
				}
			}
		}
		return nil
	}
	if needed == 0 {
		needed = len(fragment.Relays)
	}
	// Published on a majority before the URL is given, on the others a moment later.
	testkit.Eventually(t, func() bool {
		held := 0
		for _, relay := range fragment.Relays {
			if copyOn(relay) != nil {
				held++
			}
		}
		return held >= needed
	}, "the link on the relays")
	for _, relay := range fragment.Relays {
		if e := copyOn(relay); e != nil {
			l.copies = append(l.copies, e)
		}
	}
	l.event = l.copies[0]
	if _, valid := nostr.Valid(testkit.EventValue(l.event)); !valid {
		t.Fatal("not a valid event")
	}
	plaintext, err := nip44.Decrypt(l.event.Content, encryptionKey)
	must(t, err)
	if len(plaintext)%256 != 0 {
		t.Errorf("a plaintext of %d bytes", len(plaintext))
	}
	record, err := jsonv.ParseString(plaintext)
	must(t, err)
	if typ := fieldOf(record, "t"); typ != "checkout" {
		t.Errorf("type %v", typ)
	}
	l.value = fieldOf(record, "v")
	l.record, _ = checkoutlink.ParseValue(l.value)
	return l
}

func TestTheLinkIsPublishedBeforeTheDeclarationAndItsURLGiven(t *testing.T) {
	m := checkoutMerchant(t)
	invoice, err := linkClient(t, m).CreateInvoice(ctx, checkout.Order{
		Ref: "ORD-1042", Fiat: &checkout.Price{Amount: "24.50", Currency: "CHF"}, Note: "Order 1042",
		Checkout: true, RedirectURL: "https://shop.example/orders/1042/thanks",
	})
	must(t, err)
	made := m.Invoices()[0]
	if invoice.RedirectURL != "https://shop.example/orders/1042/thanks" {
		t.Errorf("redirectUrl %q", invoice.RedirectURL)
	}
	// JSON as is, with exactly the fields of SPEC.md §3: no secret of the link.
	names := jsonOf(t, invoice).(*jsonv.Object).Keys()
	sort.Strings(names)
	same(t, "fields", names, sorted("ref", "bolt11", "paymentHash", "verifyUrl", "onchainAddress", "paymentUri", "sats", "fiat", "rate", "note",
		"createdAt", "expiresAt", "checkoutUrl", "redirectUrl"))

	l := openLink(t, m, invoice.CheckoutURL)
	// On every relay of the merchant, which the URL names.
	same(t, "relays", l.fragment.Relays, m.KeyRelays)
	for _, c := range l.copies {
		if c == nil || c.ID != l.event.ID {
			t.Error("not the same link on every relay")
		}
	}
	// The link first, then the declaration: a link never names an invoice its URL was not given
	// for.
	testkit.Eventually(t, func() bool { return len(received(m)) == 2 }, "the link and the declaration on relay-a")
	var order []string
	for _, e := range received(m) {
		if isCheckout(e) {
			order = append(order, "link")
		} else {
			order = append(order, "declaration")
		}
	}
	same(t, "order", order, []string{"link", "declaration"})
	// Its own author, not the sale's; dropped by the relays a week after the invoice's end.
	if _, _, sale := derive.SaleKeys(m.IntegrationKey, "ORD-1042"); l.event.PubKey == sale {
		t.Error("signed by the sale")
	}
	address, _ := derive.CheckoutKeys(l.fragment.LinkKey)
	same(t, "tags", l.event.Tags, [][]string{{"d", address}, {"expiration", strconv.FormatInt(invoice.ExpiresAt+604_800, 10)}})
	if l.event.CreatedAt != m.Now() {
		t.Errorf("created_at %d", l.event.CreatedAt)
	}
	same(t, "value", l.value, jsonv.O(
		"version", 1,
		"businessName", "Sunrise Bakery",
		"invoice", made.PR,
		"verifyUrl", made.Verify,
		"fiat", jsonv.O("minor", 2450, "digits", 2, "currency", "CHF"),
		"note", "Order 1042",
		"redirectUrl", "https://shop.example/orders/1042/thanks",
	))
	if l.record == nil || l.record.Sats != invoice.Sats || l.record.RedirectShown != "shop.example" {
		t.Errorf("record %+v", l.record)
	}
}

func sorted(names ...string) []string {
	sort.Strings(names)
	return names
}

func TestAnOrderInSatsIsWrittenWithoutAPriceAndWithoutARedirectURL(t *testing.T) {
	m := checkoutMerchant(t)
	invoice, err := linkClient(t, m).CreateInvoice(ctx, checkout.Order{Ref: "A-1", Sats: 49_000, Checkout: true})
	must(t, err)
	if invoice.RedirectURL != "" {
		t.Errorf("redirectUrl %q", invoice.RedirectURL)
	}
	l := openLink(t, m, invoice.CheckoutURL)
	if fieldOf(l.value, "fiat") != nil || fieldOf(l.value, "note") != "A-1" || fieldOf(l.value, "redirectUrl") != nil {
		t.Errorf("value %s", jsonv.Marshal(l.value))
	}
}

func TestTheLinksPublicationIsAuthenticatedAsItsAuthor(t *testing.T) {
	m := checkoutMerchant(t)
	for _, url := range m.KeyRelays {
		m.Net.Get(url).SetBehaviour(testkit.Behaviour{RequireAuth: true})
	}
	invoice, err := linkClient(t, m).CreateInvoice(ctx, checkout.Order{Ref: "A-1", Sats: 100, Checkout: true})
	must(t, err)
	// On every relay, the relays past the majority once the call returned.
	l := openLink(t, m, invoice.CheckoutURL)
	for _, url := range m.KeyRelays {
		if !contains(m.Net.Get(url).Authenticated(), l.fragment.Author) {
			t.Errorf("%s: not authenticated as the link's author", url)
		}
	}
}

func TestEachInvoiceGetsANewLinkWithNewKeys(t *testing.T) {
	m := checkoutMerchant(t)
	first, err := linkClient(t, m).CreateInvoice(ctx, checkout.Order{Ref: "A-1", Sats: 100, Checkout: true})
	must(t, err)
	second, err := linkClient(t, m).CreateInvoice(ctx, checkout.Order{Ref: "A-2", Sats: 100, Checkout: true})
	must(t, err)
	a, b := openLink(t, m, first.CheckoutURL).fragment, openLink(t, m, second.CheckoutURL).fragment
	if a.Author == b.Author || bytes.Equal(a.LinkKey, b.LinkKey) {
		t.Error("the same keys")
	}
}

func TestNothingMoreIsPublishedWithoutCheckout(t *testing.T) {
	m := checkoutMerchant(t)
	invoice, err := linkClient(t, m).CreateInvoice(ctx, checkout.Order{Ref: "A-1", Sats: 100})
	must(t, err)
	if invoice.CheckoutURL != "" || invoice.RedirectURL != "" {
		t.Errorf("%+v", invoice)
	}
	for _, e := range received(m) {
		if isCheckout(e) {
			t.Error("a link published")
		}
	}
}

func TestCheckoutIsRefusedWhenTheMerchantsAppGivesNoWebApp(t *testing.T) {
	m := checkoutMerchant(t)
	invalid := []any{testkit.Absent, nil, "https://pay.example", jsonv.O("origin", "http://pay.example")}
	for i, value := range invalid {
		at := int64(200 + i)
		m.PublishChannel(m.ChannelValue(at, jsonv.Member{Name: "relays", Value: m.KeyRelays}, jsonv.Member{Name: "checkout", Value: value}), at)
		_, err := linkClient(t, m).CreateInvoice(ctx, checkout.Order{Ref: "A-" + strconv.Itoa(i), Sats: 100, Checkout: true})
		wantCode(t, err, checkout.CodeCheckoutUnavailable)
		// The channel stays valid: an invoice without checkout is made.
		invoice, err := linkClient(t, m).CreateInvoice(ctx, checkout.Order{Ref: "B-" + strconv.Itoa(i), Sats: 100})
		must(t, err)
		if invoice.CheckoutURL != "" {
			t.Error("a link without checkout")
		}
	}
	_, err := linkClient(t, m).CreateInvoice(ctx, checkout.Order{Ref: "C-1", Sats: 100, Checkout: true})
	if e := wantCode(t, err, checkout.CodeCheckoutUnavailable); e.Retryable() {
		t.Error("checkout_unavailable is not retryable")
	}
	// Only the invoices made without checkout.
	if n := len(m.Invoices()); n != len(invalid) {
		t.Errorf("%d invoices", n)
	}
}

func TestAnInvalidRedirectURLIsRefusedBeforeAnyRequest(t *testing.T) {
	m := checkoutMerchant(t)
	for _, o := range []checkout.Order{
		{Ref: "A-1", Sats: 100, RedirectURL: "https://shop.example/thanks"},
		{Ref: "A-1", Sats: 100, Checkout: true, RedirectURL: "http://shop.example/thanks"},
		{Ref: "A-1", Sats: 100, Checkout: true, RedirectURL: "javascript://alert(1)"},
		{Ref: "A-1", Sats: 100, Checkout: true, RedirectURL: "https://user:pass@shop.example/"},
		{Ref: "A-1", Sats: 100, Checkout: true, RedirectURL: "https://shop.example/a b"},
	} {
		_, err := linkClient(t, m).CreateInvoice(ctx, o)
		wantCode(t, err, checkout.CodeInvalidOrder, "redirectUrl")
	}
	if n := len(m.HTTP.Calls()); n != 0 {
		t.Errorf("%d requests", n)
	}
}

func TestNothingIsDeclaredWhenFewerThanAMajorityTakeTheLink(t *testing.T) {
	m := checkoutMerchant(t)
	refuseLinks := testkit.Behaviour{RefuseEvent: func(e *nostr.Event) string {
		if isCheckout(e) {
			return "blocked: no"
		}
		return ""
	}}
	m.Net.Get("wss://relay-a.test").SetBehaviour(refuseLinks)
	m.Net.Get("wss://relay-b.test").SetBehaviour(refuseLinks)
	_, err := linkClient(t, m).CreateInvoice(ctx, checkout.Order{Ref: "A-1", Sats: 100, Checkout: true})
	wantCode(t, err, checkout.CodeNetwork)
	for _, e := range received(m) {
		if !isCheckout(e) {
			t.Error("declared")
		}
	}
	// The order gets a new invoice at once: nothing was declared.
	m.Net.Get("wss://relay-a.test").SetBehaviour(testkit.Behaviour{})
	m.Net.Get("wss://relay-b.test").SetBehaviour(testkit.Behaviour{})
	invoice, err := linkClient(t, m).CreateInvoice(ctx, checkout.Order{Ref: "A-1", Sats: 100, Checkout: true})
	must(t, err)
	if invoice.Ref != "A-1" || len(m.Invoices()) != 2 {
		t.Errorf("%d invoices", len(m.Invoices()))
	}
}

func TestNoURLIsGivenWhenTheDeclarationFailsAfterTheLink(t *testing.T) {
	m := checkoutMerchant(t)
	refuseDeclarations := testkit.Behaviour{RefuseEvent: func(e *nostr.Event) string {
		if isCheckout(e) {
			return ""
		}
		return "blocked: no"
	}}
	m.Net.Get("wss://relay-a.test").SetBehaviour(refuseDeclarations)
	m.Net.Get("wss://relay-b.test").SetBehaviour(refuseDeclarations)
	invoice, err := linkClient(t, m).CreateInvoice(ctx, checkout.Order{Ref: "A-1", Sats: 100, Checkout: true})
	wantCode(t, err, checkout.CodeNetwork)
	if invoice != nil {
		t.Error("an invoice given")
	}
}

func TestAPendingInvoiceGetsANewLinkWithThisCallsRedirectURL(t *testing.T) {
	m := checkoutMerchant(t)
	first, err := linkClient(t, m).CreateInvoice(ctx, checkout.Order{Ref: "ORD-7", Fiat: &checkout.Price{Amount: "0.02", Currency: "CHF"}, Note: "Order 7",
		Checkout: true, RedirectURL: "https://shop.example/7"})
	must(t, err)
	_, err = linkClient(t, m).CreateInvoice(ctx, checkout.Order{Ref: "ORD-7", Fiat: &checkout.Price{Amount: "0.02", Currency: "CHF"},
		Checkout: true, RedirectURL: "myshop://orders/7"})
	pending := wantCode(t, err, checkout.CodeInvoicePending)
	if pending.Invoice.Bolt11 != first.Bolt11 || pending.CheckoutURL == "" || pending.CheckoutURL == first.CheckoutURL {
		t.Fatalf("%+v", pending)
	}
	l := openLink(t, m, pending.CheckoutURL)
	same(t, "value", l.value, jsonv.O(
		"version", 1,
		"businessName", "Sunrise Bakery",
		"invoice", first.Bolt11,
		"verifyUrl", first.VerifyURL,
		"fiat", jsonv.O("minor", 2, "digits", 2, "currency", "CHF"),
		"note", "Order 7",
		"redirectUrl", "myshop://orders/7",
	))
	if l.record == nil || l.record.RedirectShown != "myshop://" {
		t.Errorf("record %+v", l.record)
	}
	// Without checkout, no link.
	_, err = linkClient(t, m).CreateInvoice(ctx, checkout.Order{Ref: "ORD-7", Fiat: &checkout.Price{Amount: "0.02", Currency: "CHF"}})
	if plain := wantCode(t, err, checkout.CodeInvoicePending); plain.CheckoutURL != "" {
		t.Errorf("a link: %s", plain.CheckoutURL)
	}
}

func TestTheDeclarationStaysAsItWasWithoutTheRedirectURL(t *testing.T) {
	m := checkoutMerchant(t)
	_, err := linkClient(t, m).CreateInvoice(ctx, checkout.Order{Ref: "A-1", Sats: 100, Checkout: true, RedirectURL: "https://shop.example/thanks"})
	must(t, err)
	testkit.Eventually(t, func() bool { return len(received(m)) == 2 }, "the link and the declaration on relay-a")
	for _, e := range received(m) {
		if isCheckout(e) {
			continue
		}
		inside, err := testkit.OpenAsOwner(e, m.RegistryKey, m.ReaderSecret)
		must(t, err)
		names := inside.Keys()
		sort.Strings(names)
		same(t, "fields", names, []string{"invoice", "sale", "taker", "verifyUrl", "version"})
	}
}

// A link is published within the call's context: a cancelled call publishes none.
func TestACancelledCallGivesNoLink(t *testing.T) {
	m := checkoutMerchant(t)
	call, cancel := context.WithCancel(ctx)
	cancel()
	_, err := linkClient(t, m).CreateInvoice(call, checkout.Order{Ref: "A-1", Sats: 100, Checkout: true})
	if err != context.Canceled {
		t.Fatalf("got %v", err)
	}
	for _, e := range received(m) {
		if isCheckout(e) {
			t.Error("a link published")
		}
	}
}
