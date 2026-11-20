package checkout_test

import (
	"testing"
	"time"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/testkit"
)

// Reading the integration's channel over relays: internal/channel's tests; here, the merchant as
// the client gives it.
func TestTheMerchantIsReadAtEveryCallAsJSONAndNothingIsKeptOpen(t *testing.T) {
	m := testkit.NewMerchant()
	m.Start(nil, nil)
	t.Cleanup(m.Stop)
	m.PublishChannel(m.ChannelValue(100), 100)
	c, err := checkout.New(m.Key(), checkout.WithHTTPTransport(m.HTTP),
		checkout.WithWebSocketDialer(checkout.NewWebSocketDialer(m.Net.Transport())), checkout.WithLogger(quiet()))
	must(t, err)
	found, err := c.Merchant(ctx)
	must(t, err)
	same(t, "merchant", jsonOf(t, found), jsonv.O(
		"businessName", "Sunrise Bakery",
		"minSats", "1",
		"maxSats", "100000000",
		// No lot of addresses yet: no on-chain payment.
		"onchainAddresses", 0,
	))
	var back checkout.Merchant
	must(t, back.UnmarshalJSON([]byte(`{"businessName":"Sunrise Bakery","minSats":"1","maxSats":"100000000","onchainAddresses":0}`)))
	if back != *found {
		t.Errorf("read back %+v", back)
	}
	// Nothing kept: a removal stops the very next call.
	m.PublishChannel(jsonv.O("version", 1, "status", "removed", "updatedAt", 200), 200)
	_, err = c.Merchant(ctx)
	wantCode(t, err, checkout.CodeRevoked)
	// Each call its own connections, one per relay at most (a relay the read went on without may
	// never have been reached), all closed once it is done.
	total := 0
	for _, url := range m.KeyRelays {
		r := m.Net.Get(url)
		testkit.Eventually(t, func() bool { return r.Open() == 0 }, url+" closed")
		n := m.Net.Connections(url)
		if n > 2 {
			t.Errorf("%s: %d connections", url, n)
		}
		total += n
	}
	if total < 4 {
		t.Errorf("%d connections: not a majority per call", total)
	}
}

// SPEC.md §14, step 3: once the merchant removed the integration, every call that reads its channel
// is revoked: making an invoice, finding an order's invoices, reading the merchant, and the on-chain
// part of a check. The check of an invoice with an address, and the wait for it, is revoked unless
// its Lightning invoice is paid (paid on-chain, it is revoked all the same: the merchant settles it
// by hand). The check of an invoice without an address reads no relay and goes on: its verify URL
// needs nothing of the key.
func TestARemovedIntegrationStopsTheChecksThatReadItsChannelOnly(t *testing.T) {
	m := onchainMerchant(t)
	c := clientOf(t, m)
	wait := func(invoice *checkout.Invoice) (*checkout.PaymentStatus, error) {
		return c.WaitForPayment(ctx, invoice, checkout.WithInterval(250*time.Millisecond))
	}
	// Under 1 000 sats, no address: Lightning only.
	lightningOnly, err := c.CreateInvoice(ctx, checkout.Order{Ref: "RM-1", Sats: 500})
	must(t, err)
	withAddress, err := c.CreateInvoice(ctx, checkout.Order{Ref: "RM-2", Sats: 49_000})
	must(t, err)
	paidOnchain, err := c.CreateInvoice(ctx, checkout.Order{Ref: "RM-3", Sats: 49_000})
	must(t, err)
	if lightningOnly.OnchainAddress != "" || withAddress.OnchainAddress == "" || withAddress.Bolt11 == "" || paidOnchain.OnchainAddress == "" {
		t.Fatalf("invoices %+v, %+v, %+v", lightningOnly, withAddress, paidOnchain)
	}
	m.Chain.Add(paidOnchain.OnchainAddress, testkit.Payment(testkit.Txid("paid in time"), paidOnchain.OnchainAddress, 49_000, nil, 0))
	m.PublishChannel(jsonv.O("version", 1, "status", "removed", "updatedAt", m.Now()), m.Now())
	connections := func() int {
		n := 0
		for _, url := range m.KeyRelays {
			n += m.Net.Connections(url)
		}
		return n
	}
	before := connections()
	// Without an address: its status, open, then paid; no relay asked.
	status, err := c.GetStatus(ctx, lightningOnly)
	must(t, err)
	if status.State != checkout.StateOpen {
		t.Errorf("without an address: %s", status.State)
	}
	m.PayInvoice(lightningOnly.PaymentHash)
	for name, check := range map[string]func(*checkout.Invoice) (*checkout.PaymentStatus, error){
		"checked": func(invoice *checkout.Invoice) (*checkout.PaymentStatus, error) { return c.GetStatus(ctx, invoice) },
		"waited":  wait,
	} {
		status, err := check(lightningOnly)
		must(t, err)
		if status.State != checkout.StatePaid || status.Method != checkout.MethodLightning {
			t.Errorf("without an address, %s: %s %s", name, status.State, status.Method)
		}
	}
	if n := connections() - before; n != 0 {
		t.Errorf("%d connections to the relays for an invoice without an address", n)
	}
	// With an address: revoked, checked or waited for, until its Lightning invoice is paid.
	for _, invoice := range []*checkout.Invoice{withAddress, paidOnchain} {
		_, err = c.GetStatus(ctx, invoice)
		wantCode(t, err, checkout.CodeRevoked)
		_, err = wait(invoice)
		wantCode(t, err, checkout.CodeRevoked)
	}
	m.PayInvoice(withAddress.PaymentHash)
	for name, check := range map[string]func(*checkout.Invoice) (*checkout.PaymentStatus, error){
		"checked": func(invoice *checkout.Invoice) (*checkout.PaymentStatus, error) { return c.GetStatus(ctx, invoice) },
		"waited":  wait,
	} {
		status, err := check(withAddress)
		must(t, err)
		if status.State != checkout.StatePaid || status.Method != checkout.MethodLightning {
			t.Errorf("with an address, paid by its Lightning invoice, %s: %s %s", name, status.State, status.Method)
		}
	}
	// The calls that read the channel.
	_, err = c.FindInvoices(ctx, "RM-2")
	wantCode(t, err, checkout.CodeRevoked)
	_, err = c.Merchant(ctx)
	wantCode(t, err, checkout.CodeRevoked)
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "RM-4", Sats: 500})
	wantCode(t, err, checkout.CodeRevoked)
}
