package checkout_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/checkoutlink"
	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/onchain"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/testkit"
)

// On-chain payments (SPEC.md §31), end to end against local relays and fakes of breez.tips, of
// the explorers (REST and WebSocket) and of BlockCypher: the methods of an order by its amount, a
// lot read and an address leased, two servers leasing one address, a paid address skipped, the
// witnesses written and read, the statuses of both parts, an order paid on-chain found again,
// waitForPayment woken by the explorer's WebSocket.

func TestAnAddressIsLeasedFrom1000SatsBeforeTheInvoiceIsDeclared(t *testing.T) {
	m := onchainMerchant(t)
	invoice, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "ORD-1", Sats: 49_000, Note: "Order 1"})
	must(t, err)
	address := invoice.OnchainAddress
	if !contains(lot, address) || !strings.HasPrefix(invoice.Bolt11, "lnbc") {
		t.Fatalf("%+v", invoice)
	}
	if invoice.PaymentURI != "bitcoin:"+address+"?amount=0.00049&lightning="+invoice.Bolt11 {
		t.Errorf("uri %s", invoice.PaymentURI)
	}
	// The explorer was asked whether the address was paid already.
	if !contains(m.Chain.Asked(), proto.MempoolURL+"/address/"+address+"/txs") {
		t.Error("the explorer not asked")
	}
	// The lease first, then its mark once confirmed, then the declaration: an address is never
	// shown before its lease is confirmed.
	sale := m.Sale("ORD-1")
	testkit.Eventually(t, func() bool { return len(received(m)) == 3 }, "3 records on relay-a")
	events := received(m)
	if d, _ := events[0].DTag(); d != derive.LeaseAddress(m.RegistryKey, address) {
		t.Error("not the lease first")
	}
	for _, e := range events {
		if e.PubKey != sale.Author {
			t.Error("not the sale's")
		}
	}
	if events[1].CreatedAt <= events[0].CreatedAt {
		t.Error("the mark not after the lease")
	}
	if fieldOf(openAsMerchant(t, m, events[0]), "confirmed") != false {
		t.Error("the lease published confirmed")
	}
	same(t, "mark", openAsMerchant(t, m, events[1]), jsonv.O(
		"end", invoice.ExpiresAt+120,
		"version", 1,
		"address", address,
		"sats", 49000,
		"invoice", invoice.Bolt11,
		"verifyUrl", invoice.VerifyURL,
		"createdAt", invoice.CreatedAt,
		"expiresAt", invoice.ExpiresAt,
		"sale", jsonv.O("key", hex.EncodeToString(sale.Key), "fiat", jsonv.O("minor", 49000, "digits", 0, "currency", "SAT"), "note", "Order 1"),
		"taker", sale.Taker,
		"confirmed", true,
	))
}

func TestBelow1000SatsLightningOnlyTheLotNotEvenRead(t *testing.T) {
	m := onchainMerchant(t)
	invoice, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "ORD-2", Sats: 999})
	must(t, err)
	if invoice.OnchainAddress != "" || invoice.PaymentURI != "lightning:"+invoice.Bolt11 {
		t.Errorf("%+v", invoice)
	}
	if asked := m.Chain.Asked(); len(asked) != 0 {
		t.Errorf("asked %v", asked)
	}
}

func TestAboveTheLightningAddressMaximumOnchainOnly(t *testing.T) {
	m := onchainMerchant(t)
	m.SetPay(pay("maxSendable", int64(4_000_000_000)))
	c := clientOf(t, m)
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "ORD-3", Sats: 150_000_000, DurationSeconds: 600})
	must(t, err)
	if invoice.Bolt11 != "" || invoice.PaymentHash != "" || invoice.VerifyURL != "" || invoice.Sats != 150_000_000 ||
		invoice.CreatedAt != m.Now() || invoice.ExpiresAt != m.Now()+600 {
		t.Errorf("%+v", invoice)
	}
	if invoice.PaymentURI != "bitcoin:"+invoice.OnchainAddress+"?amount=1.5" {
		t.Errorf("uri %s", invoice.PaymentURI)
	}
	// No invoice asked, nothing declared: the lease alone, then its mark.
	if n := len(m.Invoices()); n != 0 {
		t.Errorf("%d invoices", n)
	}
	testkit.Eventually(t, func() bool { return len(received(m)) == 2 }, "2 records on relay-a")
	mark := openAsMerchant(t, m, received(m)[1])
	same(t, "mark", jsonv.O("confirmed", fieldOf(mark, "confirmed"), "invoice", fieldOf(mark, "invoice"), "verifyUrl", fieldOf(mark, "verifyUrl"),
		"createdAt", fieldOf(mark, "createdAt"), "expiresAt", fieldOf(mark, "expiresAt"), "end", fieldOf(mark, "end")),
		jsonv.O("confirmed", true, "invoice", nil, "verifyUrl", nil, "createdAt", m.Now(), "expiresAt", m.Now()+600, "end", m.Now()+720))
	status, err := c.GetStatus(ctx, invoice)
	must(t, err)
	same(t, "status", jsonOf(t, status), jsonv.O("state", "open", "expiresAt", invoice.ExpiresAt, "received", "0", "rest", nil))
}

func TestWithoutALotLightningOnlyAndOnchainOnlyFails(t *testing.T) {
	m := onchainMerchant(t)
	for _, url := range m.KeyRelays {
		m.Net.Get(url).SetEvents(nil)
	}
	m.PublishChannel(m.ChannelValue(100, jsonv.Member{Name: "relays", Value: m.KeyRelays}), 100)
	c := clientOf(t, m)
	merchant, err := c.Merchant(ctx)
	must(t, err)
	if merchant.OnchainAddresses != 0 {
		t.Errorf("%d addresses", merchant.OnchainAddresses)
	}
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "ORD-4", Sats: 49_000})
	must(t, err)
	if invoice.OnchainAddress != "" {
		t.Error("an address without a lot")
	}
	m.SetPay(pay("maxSendable", 1_000_000))
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "ORD-5", Sats: 49_000})
	wantCode(t, err, checkout.CodeOnchainUnavailable)
	// Nothing published for that order.
	if n := len(m.Invoices()); n != 1 {
		t.Errorf("%d invoices", n)
	}
}

func TestWhenTheExplorerDoesNotAnswerLightningOnlyAndNetworkOnchainOnly(t *testing.T) {
	m := onchainMerchant(t)
	m.Chain.SetDown(testkit.Explorer, true)
	logger := quiet()
	c := clientOf(t, m, checkout.WithLogger(logger))
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "ORD-6", Sats: 49_000})
	must(t, err)
	if invoice.OnchainAddress != "" {
		t.Error("an address the explorer did not check")
	}
	if !logger.Has("no deposit address leased, the explorer did not answer") {
		t.Errorf("warnings %v", logger.Messages())
	}
	m.SetPay(pay("maxSendable", 1_000_000))
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "ORD-7", Sats: 49_000})
	wantCode(t, err, checkout.CodeNetwork)
}

func TestMoreThan21MillionBitcoinsIsRefused(t *testing.T) {
	m := onchainMerchant(t)
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "ORD-8", Sats: 2_100_000_000_000_001})
	wantCode(t, err, checkout.CodeInvalidOrder, "sats")
}

func TestTheLotsAddressesAreCountedInMerchant(t *testing.T) {
	m := onchainMerchant(t)
	merchant, err := clientOf(t, m).Merchant(ctx)
	must(t, err)
	if merchant.OnchainAddresses != 3 {
		t.Errorf("%d addresses", merchant.OnchainAddresses)
	}
}

func TestAnAddressAnotherOrderHoldsIsNeverLeasedUntilItsLeaseEnded(t *testing.T) {
	m := onchainMerchant(t)
	m.PublishLot(lot[:1], 91)
	c := clientOf(t, m)
	first, err := c.CreateInvoice(ctx, checkout.Order{Ref: "L-1", Sats: 49_000, DurationSeconds: 60})
	must(t, err)
	if first.OnchainAddress != lot[0] {
		t.Fatalf("leased %s", first.OnchainAddress)
	}
	second, err := c.CreateInvoice(ctx, checkout.Order{Ref: "L-2", Sats: 49_000})
	must(t, err)
	if second.OnchainAddress != "" {
		t.Error("an address another order holds")
	}
	// Its lease ends 120 s after the invoice's end; another server's clock may be 300 s behind.
	m.SetNow(first.ExpiresAt + 120 + 300)
	third, err := c.CreateInvoice(ctx, checkout.Order{Ref: "L-3", Sats: 49_000})
	must(t, err)
	if third.OnchainAddress != "" {
		t.Error("leased within the clock tolerance")
	}
	m.SetNow(m.Now() + 1)
	fourth, err := c.CreateInvoice(ctx, checkout.Order{Ref: "L-4", Sats: 49_000})
	must(t, err)
	if fourth.OnchainAddress != lot[0] {
		t.Errorf("leased %q", fourth.OnchainAddress)
	}
}

func TestAnAddressPaidAlreadyIsSkippedAndItsWitnessNonePublished(t *testing.T) {
	m := onchainMerchant(t)
	paid := testkit.Payment(testkit.Txid("old"), lot[0], 20_000, nil, 960_000)
	m.Chain.Add(lot[0], paid)
	m.PublishLot(lot[:2], 91)
	invoice, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "L-5", Sats: 49_000})
	must(t, err)
	if invoice.OnchainAddress != lot[1] {
		t.Errorf("leased %s", invoice.OnchainAddress)
	}
	// Whichever was tried first: the paid one is never leased.
	if records := leasesOf(m, lot[0]); len(records) != 0 {
		t.Errorf("%d leases of the paid address", len(records))
	}
	m.PublishLot(lot[:1], 92)
	m.SetPay(pay("maxSendable", 1_000_000))
	_, err = clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "L-6", Sats: 49_000})
	wantCode(t, err, checkout.CodeOnchainUnavailable)
	testkit.Eventually(t, func() bool { return len(witnessesOf(m)) == 1 }, "the witness none")
	w := witnessesOf(m)[0]
	same(t, "witness", w, witnessSeen{Witness: onchain.Witness{Address: lot[0], Txid: paid.Txid, Sats: 20_000, Inputs: []string{coinOf(paid.Txid)}}, SeenAt: m.Now()})
}

func competingLease(t *testing.T, m *testkit.Merchant, sale *derive.Sale, address string) *nostr.Event {
	t.Helper()
	e, err := onchain.LeaseEventOf(sale, onchain.LeaseTerms{
		Address:   address,
		Sats:      1000,
		CreatedAt: m.Now(),
		ExpiresAt: m.Now() + 900,
		Price:     declaration.Price{Minor: 1000, Digits: 0, Currency: "SAT"},
		Note:      "Another order",
	}, m.Now(), testkit.Random("competing lease"))
	must(t, err)
	return e
}

func TestALeaseIsWithdrawnUnconfirmedWhenAnotherServerLeasedTheAddressAtOnce(t *testing.T) {
	m := onchainMerchant(t)
	m.PublishLot(lot[:1], 91)
	d := derive.LeaseAddress(m.RegistryKey, lot[0])
	other := m.Sale("ANOTHER-SERVER")
	// Another server's lease of the same address reaches every relay right after this one's.
	race := testkit.OnFirst(func(e *nostr.Event) bool { dTag, _ := e.DTag(); return dTag == d }, func() {
		competing := competingLease(t, m, other, lot[0])
		for _, relay := range m.KeyRelays {
			m.Net.Get(relay).Take(competing)
		}
	})
	for _, url := range m.KeyRelays {
		m.Net.Get(url).SetBehaviour(testkit.Behaviour{OnEvent: race})
	}
	invoice, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "L-7", Sats: 49_000})
	must(t, err)
	// It saw the other lease after its own was on a majority: not confirmed, Lightning only. Its
	// lease, unmarked, is withdrawn: it gives the invoice no address.
	if invoice.OnchainAddress != "" {
		t.Error("an address another server leased")
	}
	own := func() []leaseSeen {
		var out []leaseSeen
		for _, r := range leasesOf(m, lot[0]) {
			if r.event.PubKey != other.Author {
				out = append(out, r)
			}
		}
		return out
	}
	testkit.Eventually(t, func() bool { return len(own()) == 2 }, "the lease and its withdrawal")
	records := own()
	same(t, "kinds", kindsOf(records), []string{"lease", "withdrawn"})
	if fieldOf(openAsMerchant(t, m, records[0].event), "confirmed") != false {
		t.Error("published confirmed")
	}
	if records[1].event.CreatedAt <= records[0].event.CreatedAt {
		t.Error("the withdrawal not after the lease")
	}
	found, err := clientOf(t, m).FindInvoices(ctx, "L-7")
	must(t, err)
	if len(found) != 1 || found[0].Bolt11 != invoice.Bolt11 || found[0].OnchainAddress != "" {
		t.Errorf("%+v", found)
	}
}

// SPEC.md §17 step 6: the lease of an address holds the invoice too, sealed twice as the
// declaration, with some 150 bytes more. An invoice whose declaration fits a NIP-44 plaintext and
// whose lease does not (some 40 500 characters) is invalid_invoice before anything is asked or
// published (no lot read, no explorer asked; no lease, no link, no declaration), never NIP-44's own
// error; one a character or so shorter, whose lease fits, is leased its address. The lease is
// measured as first published, "confirmed": false, the longer of its two forms (CheckLeaseSize
// takes no random source: the client's random bytes are left alone): the fewest characters whose
// lease does not fit so still fit it marked "confirmed": true, which is never published first.
func TestCreateInvoiceRefusesAnInvoiceItsLeaseCannotHold(t *testing.T) {
	m := onchainMerchant(t)
	m.PublishLot(lot[:1], 91)
	// An invoice of 49 000 sats with words more, in unknown tagged fields (of 1023 words at most,
	// and 3 of their own).
	invoiceOf := func(paymentHash string, words int) string {
		var extra []testkit.Tagged
		for words > 0 {
			n := min(words, 1023)
			extra = append(extra, testkit.Tagged{Tag: 31, Data: make([]int, n)})
			words -= n + 3
		}
		return testkit.MakeInvoice(testkit.Invoice{AmountMsat: 49_000_000, HasAmount: true, PaymentHash: paymentHash,
			DescriptionHash: testkit.MetadataHash(testkit.Metadata), Timestamp: m.Now(), ExpirySeconds: 900, HasExpiry: true, Extra: extra})
	}
	// The fewest words whose lease does not fit, by bisection: the size alone counts, whatever the
	// payment hash and the sale.
	_, hash := testkit.PreimagePair(77)
	sale := m.Sale("L-14")
	terms := func(words int) onchain.LeaseTerms {
		return onchain.LeaseTerms{Sats: 49_000, Invoice: invoiceOf(hash, words), VerifyURL: testkit.VerifyPrefix + hash,
			CreatedAt: m.Now(), ExpiresAt: m.Now() + 900, Price: declaration.Price{Minor: 49_000, Digits: 0, Currency: proto.SatsCurrency}, Note: "L-14"}
	}
	low, high := 0, 60_000
	if onchain.CheckLeaseSize(sale, terms(high)) == nil {
		t.Fatal("a lease holds every invoice")
	}
	for low+1 < high {
		middle := (low + high) / 2
		if onchain.CheckLeaseSize(sale, terms(middle)) == nil {
			low = middle
		} else {
			high = middle
		}
	}
	tooLarge := terms(high)
	marked := tooLarge
	marked.Address, marked.Confirmed, marked.HasConfirmed = lot[0], true, true
	if _, err := onchain.LeaseEventOf(sale, marked, m.Now(), rand.Reader); err != nil {
		t.Fatalf("an invoice of %d characters, whose lease does not fit, does not fit it marked confirmed either: %v", len(tooLarge.Invoice), err)
	}
	if _, err := declaration.Event(declaration.Input{Sale: sale, Invoice: tooLarge.Invoice, VerifyURL: tooLarge.VerifyURL, PaymentHash: hash,
		Price: tooLarge.Price, Note: "L-14", CreatedAt: m.Now(), Random: rand.Reader}); err != nil {
		t.Fatalf("the declaration of an invoice of %d characters does not fit either: %v", len(tooLarge.Invoice), err)
	}
	words := high
	m.SetTamper(func(made *testkit.MadeInvoice) { made.PR = invoiceOf(made.PaymentHash, words) })
	sent := map[string]int{}
	for _, url := range m.KeyRelays {
		sent[url] = len(m.Net.Get(url).Received())
	}
	lotAddress := derive.LotAddress(m.IntegrationKey)
	lotReads := func() int {
		n := 0
		for _, url := range m.KeyRelays {
			for _, f := range m.Net.Get(url).Queries() {
				ds, _ := f.Get("#d")
				list, _ := ds.([]any)
				for _, d := range list {
					if d == lotAddress {
						n++
					}
				}
			}
		}
		return n
	}
	readsBefore, askedBefore := lotReads(), len(m.Chain.Asked())
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "L-14", Sats: 49_000, Checkout: true})
	e := wantCode(t, err, checkout.CodeInvalidInvoice)
	if !strings.Contains(e.Message, "lease of an address") {
		t.Errorf("message %q", e.Message)
	}
	for _, url := range m.KeyRelays {
		if n := len(m.Net.Get(url).Received()) - sent[url]; n != 0 {
			t.Errorf("%s: %d events published", url, n)
		}
	}
	if n := lotReads() - readsBefore; n != 0 {
		t.Errorf("the lot read %d times", n)
	}
	if asked := m.Chain.Asked()[askedBefore:]; len(asked) != 0 {
		t.Errorf("the explorer asked %v", asked)
	}
	// A few characters less, its lease fits: leased.
	words = low
	invoice, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "L-15", Sats: 49_000, Checkout: true})
	must(t, err)
	if invoice.OnchainAddress != lot[0] || len(invoice.Bolt11) != len(terms(low).Invoice) {
		t.Errorf("address %q, an invoice of %d characters", invoice.OnchainAddress, len(invoice.Bolt11))
	}
}

func TestAConfirmedLeaseStaysWhenTheDeclarationFails(t *testing.T) {
	m := onchainMerchant(t)
	m.PublishLot(lot[:1], 91)
	d := derive.LeaseAddress(m.RegistryKey, lot[0])
	for _, url := range m.KeyRelays[:2] {
		// Leases and withdrawals are taken, declarations refused.
		m.Net.Get(url).SetBehaviour(testkit.Behaviour{RefuseEvent: func(e *nostr.Event) string {
			if dTag, _ := e.DTag(); dTag == d {
				return ""
			}
			return "blocked: no"
		}})
	}
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "L-8", Sats: 49_000})
	wantCode(t, err, checkout.CodeNetwork)
	// The lease and its mark: never withdrawn.
	testkit.Eventually(t, func() bool { return len(leasesOf(m, lot[0])) >= 2 }, "the lease and its mark")
	time.Sleep(100 * time.Millisecond)
	same(t, "kinds", kindsOf(leasesOf(m, lot[0])), []string{"lease", "lease"})
	for _, url := range m.KeyRelays {
		m.Net.Get(url).SetBehaviour(testkit.Behaviour{})
	}
	// It holds the address until its end: another order gets none.
	invoice, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "L-9", Sats: 49_000})
	must(t, err)
	if invoice.OnchainAddress != "" {
		t.Error("an address another order holds")
	}
}

func TestOnlyTheWithdrawalIsPublishedWhenTheLeaseDoesNotReachAMajority(t *testing.T) {
	m := onchainMerchant(t)
	m.PublishLot(lot[:1], 91)
	for _, url := range m.KeyRelays[:2] {
		m.Net.Get(url).SetBehaviour(testkit.Behaviour{Refuse: "blocked: no"})
	}
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "L-10", Sats: 49_000})
	wantCode(t, err, checkout.CodeNetwork)
	// The lease, then its withdrawal: no mark, no declaration, no link.
	third := m.KeyRelays[2]
	testkit.Eventually(t, func() bool { return len(m.Net.Get(third).Received()) >= 2 }, "two records on relay-c")
	time.Sleep(100 * time.Millisecond)
	if n := len(m.Net.Get(third).Received()); n != 2 {
		t.Errorf("%d records", n)
	}
	// Sent at once on the connection to relay-c, in either order: the withdrawal is the newer, the
	// one the relay keeps.
	records := leasesOn(m, third, lot[0])
	kinds := kindsOf(records)
	sort.Strings(kinds)
	same(t, "kinds", kinds, []string{"lease", "withdrawn"})
	lease, withdrawal := records[0], records[1]
	if lease.record.Outside.Kind != "lease" {
		lease, withdrawal = withdrawal, lease
	}
	if withdrawal.event.CreatedAt <= lease.event.CreatedAt {
		t.Error("the withdrawal not after the lease")
	}
	d := derive.LeaseAddress(m.RegistryKey, lot[0])
	var at []any
	for _, v := range m.Net.Get(third).Events() {
		if nostr.DTagOf(v) == d {
			at = append(at, v)
		}
	}
	if len(at) != 1 || fieldOf(at[0], "id") != withdrawal.event.ID {
		t.Errorf("relay-c holds %d records at the address, not the withdrawal", len(at))
	}
}

// paidSetup is the invoice of the order P-1, 49 000 sats lasting 600 s, with the address of a lot
// of one.
func paidSetup(t *testing.T) (*testkit.Merchant, *checkout.Client, *checkout.Invoice) {
	t.Helper()
	m := onchainMerchant(t)
	m.PublishLot(lot[:1], 91)
	c := clientOf(t, m)
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "P-1", Sats: 49_000, DurationSeconds: 600})
	must(t, err)
	if invoice.OnchainAddress == "" {
		t.Fatal("no address")
	}
	return m, c, invoice
}

func statusJSON(t *testing.T, c *checkout.Client, invoice checkout.Checkable) any {
	t.Helper()
	status, err := c.GetStatus(ctx, invoice)
	must(t, err)
	return jsonOf(t, status)
}

func TestUnconfirmedOnceEnoughIsSentThenPaidAtThreeConfirmations(t *testing.T) {
	m, c, invoice := paidSetup(t)
	address := invoice.OnchainAddress
	sale := m.Sale("P-1").Author
	same(t, "open", statusJSON(t, c, invoice), jsonv.O("state", "open", "expiresAt", invoice.ExpiresAt, "received", "0", "rest", nil))
	tx := testkit.Payment(testkit.Txid("pay"), address, 49_000, nil, 0)
	m.Chain.Add(address, tx)
	same(t, "unconfirmed", statusJSON(t, c, invoice), jsonv.O("state", "unconfirmed", "received", "49000", "confirmations", 0, "txids", []string{tx.Txid}))
	// Seen while the lease runs: the witness of the order's sale, which the merchant's app reads.
	testkit.Eventually(t, func() bool { return len(witnessesOf(m)) == 1 }, "the witness")
	w := witnessesOf(m)[0]
	if w.Address != address || w.Txid != tx.Txid || w.Sale != sale || w.Sats != 49_000 || w.Replacement {
		t.Errorf("%+v", w)
	}
	m.Chain.Confirm(address, tx.Txid, m.Chain.Height()-1)
	status := statusJSON(t, c, invoice)
	if fieldOf(status, "state") != "unconfirmed" || fieldOf(status, "confirmations") != 2.0 {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	// 3 confirmations on the explorer read: paid, on what it alone shows.
	m.Chain.Confirm(address, tx.Txid, m.Chain.Height()-2)
	same(t, "paid", statusJSON(t, c, invoice), jsonv.O("state", "paid", "method", "onchain", "received", "49000", "txids", []string{tx.Txid}))
	// Long after the invoice's end: the witness says it came in time.
	m.SetNow(invoice.ExpiresAt + 3600)
	same(t, "paid", statusJSON(t, c, invoice), jsonv.O("state", "paid", "method", "onchain", "received", "49000", "txids", []string{tx.Txid}))
	// One witness: a check that finds it adds nothing.
	time.Sleep(50 * time.Millisecond)
	if n := len(witnessesOf(m)); n != 1 {
		t.Errorf("%d witnesses", n)
	}
	if asked := m.Chain.BlockCypherAsked(); len(asked) != 0 {
		t.Errorf("BlockCypher asked %v", asked)
	}
}

func TestOpenWhileUnderpaidThenExpiredWithWhatWasReceived(t *testing.T) {
	m, c, invoice := paidSetup(t)
	m.Chain.Add(invoice.OnchainAddress, testkit.Payment(testkit.Txid("part"), invoice.OnchainAddress, 20_000, nil, 969_990))
	// Its rest, asked in Lightning too (rest_test.go).
	status, err := c.GetStatus(ctx, invoice)
	must(t, err)
	if status.State != checkout.StateOpen || status.ExpiresAt != invoice.ExpiresAt || status.Received != 20_000 || status.Rest == nil || status.Rest.Sats != 29_000 {
		t.Errorf("%+v", status)
	}
	m.SetNow(invoice.ExpiresAt + 121)
	same(t, "expired", statusJSON(t, c, invoice), jsonv.O("state", "expired", "received", "20000"))
}

func TestExpiredForAPaymentNoCheckSawReceivedLateByBlockCyphersTime(t *testing.T) {
	m, c, invoice := paidSetup(t)
	address := invoice.OnchainAddress
	m.SetNow(invoice.ExpiresAt + 3600)
	tx := testkit.Payment(testkit.Txid("late"), address, 49_000, nil, 0)
	m.Chain.Add(address, tx)
	m.Chain.SetReceived(tx.Txid, iso(invoice.ExpiresAt+121))
	same(t, "expired", statusJSON(t, c, invoice), jsonv.O("state", "expired", "received", "0"))
	same(t, "asked", m.Chain.BlockCypherAsked(), []string{proto.BlockCypherURL + "/txs/" + tx.Txid + "?limit=1&includeHex=false"})
	// Of no order: a witness none, so that the merchant's app takes the address out of the lot.
	testkit.Eventually(t, func() bool { return len(witnessesOf(m)) == 1 }, "the witness none")
	if w := witnessesOf(m)[0]; w.Txid != tx.Txid || w.Sale != "" {
		t.Errorf("%+v", w)
	}
}

// SPEC.md §31, "The on-chain payment", step 3.4: "listed now, it was broadcast by now" needs the
// clock once the explorer answered. The check starts 5 s before the lease's end, the explorer
// answers 10 s later: listed then, the transaction was not seen in time, and BlockCypher received
// it 3 s after the end. An order paid on-chain only (its status is its on-chain part's).
func TestATransactionIsJudgedByTheClockOnceTheExplorerAnswered(t *testing.T) {
	m := onchainMerchant(t)
	m.PublishLot(lot[:1], 91)
	m.SetPay(pay("maxSendable", 1_000_000))
	c := clientOf(t, m)
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "T-1", Sats: 49_000, DurationSeconds: 600})
	must(t, err)
	address := invoice.OnchainAddress
	if address == "" || invoice.Bolt11 != "" {
		t.Fatalf("%+v", invoice)
	}
	m.SetNow(invoice.ExpiresAt + 115)
	tx := testkit.Payment(testkit.Txid("listed after the end"), address, 49_000, nil, 0)
	m.Chain.Add(address, tx)
	m.Chain.SetReceived(tx.Txid, iso(invoice.ExpiresAt+123))
	m.SetOverride(func(req *http.Request) (testkit.Answer, bool) {
		if strings.HasSuffix(req.URL.Path, "/address/"+address+"/txs") {
			m.SetNow(m.Now() + 10)
		}
		return testkit.Answer{}, false
	})
	same(t, "expired", statusJSON(t, c, invoice), jsonv.O("state", "expired", "received", "0"))
	// Of no order: a witness none.
	testkit.Eventually(t, func() bool { return len(witnessesOf(m)) == 1 }, "the witness none")
	if w := witnessesOf(m)[0]; w.Txid != tx.Txid || w.Sale != "" {
		t.Errorf("%+v", w)
	}
}

// SPEC.md §12: a record of a newer format is never written over. A newer version's witness of
// none of a late transaction, on every relay: what it holds cannot be read here, so no witness
// none is published at its address, and the package says it should be updated.
func TestAWitnessNoneIsNeverPublishedOverOneOfANewerFormat(t *testing.T) {
	m, _, invoice := paidSetup(t)
	address := invoice.OnchainAddress
	m.SetNow(invoice.ExpiresAt + 3600)
	tx := testkit.Payment(testkit.Txid("late, its witness newer"), address, 49_000, nil, 0)
	m.Chain.Add(address, tx)
	m.Chain.SetReceived(tx.Txid, iso(invoice.ExpiresAt+121))
	seen := onchain.SeenOf(m.IntegrationKey)
	newer, err := nostr.Seal(nostr.SealOptions{
		Type:          proto.WitnessRecordType,
		Value:         jsonv.O("version", 2),
		Signer:        seen.SecretKey,
		EncryptionKey: seen.EncryptionKey,
		Address:       onchain.WitnessAddress(seen.AddressKey, address, tx.Txid, ""),
		CreatedAt:     m.Now() - 60,
		Tags: [][]string{
			{"o", onchain.OrderTag(seen.AddressKey, address, "")},
			{"o", onchain.AddressTag(seen.AddressKey, address)},
		},
		Random: testkit.Random("newer witness"),
	})
	must(t, err)
	sent := map[string]int{}
	for _, url := range m.KeyRelays {
		m.Net.Get(url).Take(newer)
		sent[url] = len(m.Net.Get(url).Received())
	}
	log := &testkit.Logger{}
	same(t, "expired", statusJSON(t, clientOf(t, m, checkout.WithLogger(log)), invoice), jsonv.O("state", "expired", "received", "0"))
	time.Sleep(100 * time.Millisecond)
	for _, url := range m.KeyRelays {
		for _, e := range m.Net.Get(url).Received()[sent[url]:] {
			if e.PubKey == seen.Author {
				t.Errorf("%s: a witness published", url)
			}
		}
	}
	if !log.Has("a witness of this address has a newer format") {
		t.Errorf("warnings %q", log.Messages())
	}
}

// SPEC.md §12: a lease of a newer format holds its address, and the package says it should be
// updated.
func TestALeaseOfANewerFormatHoldsItsAddressAndIsLogged(t *testing.T) {
	m := onchainMerchant(t)
	m.PublishLot(lot[:2], 91)
	other := m.Sale("ANOTHER-SERVER")
	d := derive.LeaseAddress(m.RegistryKey, lot[0])
	newer, err := nostr.Seal(nostr.SealOptions{
		Type:          proto.LeaseRecordType,
		Value:         jsonv.O("version", 2),
		Signer:        other.SecretKey,
		EncryptionKey: derive.OuterKey(m.RegistryKey, d, other.Author),
		Address:       d,
		CreatedAt:     m.Now() - 60,
		Random:        testkit.Random("newer lease"),
	})
	must(t, err)
	for _, url := range m.KeyRelays {
		m.Net.Get(url).Take(newer)
	}
	log := &testkit.Logger{}
	invoice, err := clientOf(t, m, checkout.WithLogger(log)).CreateInvoice(ctx, checkout.Order{Ref: "N-1", Sats: 49_000})
	must(t, err)
	if invoice.OnchainAddress != lot[1] {
		t.Errorf("leased %q", invoice.OnchainAddress)
	}
	if !log.Has("a lease of the integration's addresses has a newer format") {
		t.Errorf("warnings %q", log.Messages())
	}
}

// SPEC.md §12 and §31, "The lot": a lot of a newer format is empty (no on-chain payment), and the
// package says it should be updated.
func TestALotOfANewerFormatIsEmptyAndLogged(t *testing.T) {
	m := onchainMerchant(t)
	_, encryptionKey := derive.ChannelKeys(m.IntegrationKey)
	newer, err := nostr.Seal(nostr.SealOptions{
		Type:          proto.LotRecordType,
		Value:         jsonv.O("version", 2),
		Signer:        m.TeamSecret,
		EncryptionKey: encryptionKey,
		Address:       derive.LotAddress(m.IntegrationKey),
		CreatedAt:     95,
		Random:        testkit.Random("newer lot"),
	})
	must(t, err)
	for _, url := range m.KeyRelays {
		m.Net.Get(url).Take(newer)
	}
	log := &testkit.Logger{}
	invoice, err := clientOf(t, m, checkout.WithLogger(log)).CreateInvoice(ctx, checkout.Order{Ref: "N-2", Sats: 49_000})
	must(t, err)
	if invoice.OnchainAddress != "" || invoice.Bolt11 == "" {
		t.Errorf("%+v", invoice)
	}
	if !log.Has("the integration's lot of addresses has a newer format") {
		t.Errorf("warnings %q", log.Messages())
	}
}

// SPEC.md §21: a declaration of the order's sale of a newer format is logged.
func TestADeclarationOfANewerFormatIsLogged(t *testing.T) {
	m := onchainMerchant(t)
	sale := m.Sale("N-3")
	d := derive.RegistryAddress(m.RegistryKey, strings.Repeat("ab", 32))
	// Its inside, which the sale reads, of a version above this one's.
	sealed, err := nip44.Encrypt(`{"version":2}`, sale.InnerKey, testkit.Random("newer inside"))
	must(t, err)
	newer, err := nostr.Seal(nostr.SealOptions{
		Type:          proto.DeclarationRecordType,
		Value:         jsonv.O("sealed", sealed),
		Signer:        sale.SecretKey,
		EncryptionKey: derive.OuterKey(m.RegistryKey, d, sale.Author),
		Address:       d,
		CreatedAt:     m.Now() - 60,
		Random:        testkit.Random("newer declaration"),
	})
	must(t, err)
	for _, url := range m.KeyRelays {
		m.Net.Get(url).Take(newer)
	}
	log := &testkit.Logger{}
	found, err := clientOf(t, m, checkout.WithLogger(log)).FindInvoices(ctx, "N-3")
	must(t, err)
	if len(found) != 0 {
		t.Errorf("found %+v", found)
	}
	if !log.Has("a declaration or a lease of this order has a newer format") {
		t.Errorf("warnings %q", log.Messages())
	}
}

func TestAPaymentNoCheckSawThatBlockCypherReceivedInTimeCounts(t *testing.T) {
	m, c, invoice := paidSetup(t)
	address := invoice.OnchainAddress
	m.SetNow(invoice.ExpiresAt + 3600)
	tx := testkit.Payment(testkit.Txid("in time"), address, 49_000, nil, 0)
	m.Chain.Add(address, tx)
	m.Chain.SetReceived(tx.Txid, iso(invoice.ExpiresAt+100))
	status := statusJSON(t, c, invoice)
	if fieldOf(status, "state") != "unconfirmed" || fieldOf(status, "received") != "49000" {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	testkit.Eventually(t, func() bool { return len(witnessesOf(m)) == 1 }, "the witness")
	if w := witnessesOf(m)[0]; w.Txid != tx.Txid || w.Sale != m.Sale("P-1").Author || w.Replacement {
		t.Errorf("%+v", w)
	}
	// Its witness spares the next check the question.
	_, err := c.GetStatus(ctx, invoice)
	must(t, err)
	if n := len(m.Chain.BlockCypherAsked()); n != 1 {
		t.Errorf("BlockCypher asked %d times", n)
	}
}

func TestAFeeBumpAfterTheLeasesEndOfAPaymentSeenInTimeCounts(t *testing.T) {
	m, c, invoice := paidSetup(t)
	address := invoice.OnchainAddress
	first := testkit.Payment(testkit.Txid("first"), address, 49_000, nil, 0)
	m.Chain.Add(address, first)
	_, err := c.GetStatus(ctx, invoice)
	must(t, err)
	testkit.Eventually(t, func() bool { return len(witnessesOf(m)) == 1 }, "the first witness")
	m.SetNow(invoice.ExpiresAt + 3600)
	m.Chain.Drop(address, first.Txid)
	bump := testkit.Payment(testkit.Txid("bump"), address, 49_000, append(coinsOf(first), testkit.Txid("fee")+":1"), 0)
	m.Chain.Add(address, bump)
	status := statusJSON(t, c, invoice)
	same(t, "unconfirmed", jsonv.O("state", fieldOf(status, "state"), "received", fieldOf(status, "received"), "txids", fieldOf(status, "txids")),
		jsonv.O("state", "unconfirmed", "received", "49000", "txids", []string{bump.Txid}))
	testkit.Eventually(t, func() bool {
		for _, w := range witnessesOf(m) {
			if w.Txid == bump.Txid && w.Sale == m.Sale("P-1").Author && w.Replacement {
				return true
			}
		}
		return false
	}, "the witness of the replacement")
	// Nothing to ask BlockCypher: all of it stands for the payment it replaces.
	if asked := m.Chain.BlockCypherAsked(); len(asked) != 0 {
		t.Errorf("BlockCypher asked %v", asked)
	}
}

func TestNetworkWhenTheExplorerDoesNotAnswerUnlessLightningWasPaid(t *testing.T) {
	m, c, invoice := paidSetup(t)
	m.Chain.SetDown(testkit.Explorer, true)
	_, err := c.GetStatus(ctx, invoice)
	wantCode(t, err, checkout.CodeNetwork)
	m.PayInvoice(invoice.PaymentHash)
	status, err := c.GetStatus(ctx, invoice)
	must(t, err)
	if status.State != checkout.StatePaid || status.Method != checkout.MethodLightning {
		t.Errorf("%+v", status)
	}
}

// SPEC.md §14: a paid Lightning invoice wins over every failure of the on-chain part of an invoice
// with an address, as over revoked (TestARemovedIntegrationStopsTheChecksThatReadItsChannelOnly)
// and an explorer that does not answer (TestNetworkWhenTheExplorerDoesNotAnswerUnlessLightningWasPaid):
// a channel no longer found (channel_not_found), and the key's relays down, the channel unread
// (network). Checked, the failure; once its Lightning invoice is paid, checked and waited for, paid.
func TestAPaidLightningInvoiceWinsOverEveryFailureOfTheOnchainPart(t *testing.T) {
	for _, c := range []struct {
		name string
		fail func(m *testkit.Merchant)
		code checkout.Code
	}{
		{"channel not found", func(m *testkit.Merchant) {
			for _, url := range m.KeyRelays {
				m.Net.Get(url).SetEvents(nil)
			}
		}, checkout.CodeChannelNotFound},
		{"the key's relays down", func(m *testkit.Merchant) {
			for _, url := range m.KeyRelays {
				m.Net.Down(url)
			}
		}, checkout.CodeNetwork},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, client, invoice := paidSetup(t)
			c.fail(m)
			_, err := client.GetStatus(ctx, invoice)
			wantCode(t, err, c.code)
			m.PayInvoice(invoice.PaymentHash)
			for name, check := range map[string]func() (*checkout.PaymentStatus, error){
				"checked": func() (*checkout.PaymentStatus, error) { return client.GetStatus(ctx, invoice) },
				"waited": func() (*checkout.PaymentStatus, error) {
					return client.WaitForPayment(ctx, invoice, checkout.WithInterval(250*time.Millisecond))
				},
			} {
				status, err := check()
				must(t, err)
				if status.State != checkout.StatePaid || status.Method != checkout.MethodLightning {
					t.Errorf("%s: %s %s", name, status.State, status.Method)
				}
			}
		})
	}
}

func TestAnOrderPaidOnchainIsFoundByItsNextInvoicePendingWhileConfirmedThenPaid(t *testing.T) {
	m, c, invoice := paidSetup(t)
	address := invoice.OnchainAddress
	tx := testkit.Payment(testkit.Txid("order"), address, 49_000, nil, 0)
	m.Chain.Add(address, tx)
	_, err := c.GetStatus(ctx, invoice)
	must(t, err)
	m.SetNow(invoice.ExpiresAt + 600)
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "P-1", Sats: 49_000})
	pending := wantCode(t, err, checkout.CodeInvoicePending)
	if pending.Invoice.Bolt11 != invoice.Bolt11 || pending.Invoice.OnchainAddress != address {
		t.Errorf("%+v", pending.Invoice)
	}
	m.Chain.Confirm(address, tx.Txid, m.Chain.Height()-5)
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "P-1", Sats: 49_000})
	paid := wantCode(t, err, checkout.CodeOrderPaid)
	if paid.Preimage != "" {
		t.Errorf("preimage %q", paid.Preimage)
	}
	same(t, "status", jsonOf(t, paid.Status), jsonv.O("state", "paid", "method", "onchain", "received", "49000", "txids", []string{tx.Txid}))
}

func TestFindInvoicesGivesItsAddressAndAnOrderPaidOnchainOnlyToo(t *testing.T) {
	m, c, invoice := paidSetup(t)
	m.SetPay(pay("maxSendable", 1_000_000))
	m.PublishLot(lot[1:2], 92)
	onchainOnly, err := c.CreateInvoice(ctx, checkout.Order{Ref: "P-2", Sats: 2_000_000, Note: "Big"})
	must(t, err)
	found, err := c.FindInvoices(ctx, "P-1")
	must(t, err)
	same(t, "P-1", jsonOf(t, found[0]), jsonv.O(
		"ref", "P-1",
		"bolt11", invoice.Bolt11,
		"paymentHash", invoice.PaymentHash,
		"verifyUrl", invoice.VerifyURL,
		"onchainAddress", invoice.OnchainAddress,
		"paymentUri", invoice.PaymentURI,
		"sats", "49000",
		"fiat", nil,
		"note", "P-1",
		"createdAt", invoice.CreatedAt,
		"expiresAt", invoice.ExpiresAt,
		"declaredAt", m.Now(),
	))
	found, err = c.FindInvoices(ctx, "P-2")
	must(t, err)
	same(t, "P-2", jsonOf(t, found), []any{jsonv.O(
		"ref", "P-2",
		"bolt11", nil,
		"paymentHash", nil,
		"verifyUrl", nil,
		"onchainAddress", onchainOnly.OnchainAddress,
		"paymentUri", onchainOnly.PaymentURI,
		"sats", "2000000",
		"fiat", nil,
		"note", "Big",
		"createdAt", onchainOnly.CreatedAt,
		"expiresAt", onchainOnly.ExpiresAt,
		// Its confirmed lease: marked a second after the lease.
		"declaredAt", onchainOnly.CreatedAt+1,
	)})
	// Pending: its declaration and its lease published again, nothing else made.
	before := len(m.Invoices())
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "P-2", Sats: 2_000_000})
	wantCode(t, err, checkout.CodeInvoicePending)
	if n := len(m.Invoices()); n != before {
		t.Errorf("%d invoices", n-before)
	}
}

func TestAStoredInvoiceWithAnAddressButNoReferenceIsRefused(t *testing.T) {
	_, c, invoice := paidSetup(t)
	stored := *invoice
	stored.Ref = ""
	_, err := c.GetStatus(ctx, stored)
	wantCode(t, err, checkout.CodeInvalidOrder, "invoice")
	other := *invoice
	other.OnchainAddress = "bc1qnotanaddress"
	_, err = c.GetStatus(ctx, other)
	wantCode(t, err, checkout.CodeInvalidOrder, "invoice")
	// As JSON: without its ref, and with a ref that is no string.
	data, err := json.Marshal(invoice)
	must(t, err)
	for _, change := range []func(o *jsonv.Object){
		func(o *jsonv.Object) { o.Set("ref", nil) },
		func(o *jsonv.Object) { o.Set("ref", 1042.0) },
	} {
		v, err := jsonv.Parse(data)
		must(t, err)
		change(v.(*jsonv.Object))
		var read checkout.InvoiceToCheck
		must(t, json.Unmarshal(jsonv.Marshal(v), &read))
		_, err = c.GetStatus(ctx, read)
		wantCode(t, err, checkout.CodeInvalidOrder, "invoice")
	}
}

func TestAnInvoiceWithoutAnAddressFieldIsReadAsLightningOnly(t *testing.T) {
	m, c, invoice := paidSetup(t)
	data, err := json.Marshal(invoice)
	must(t, err)
	v, err := jsonv.Parse(data)
	must(t, err)
	object := v.(*jsonv.Object)
	without := jsonv.NewObject()
	for _, name := range object.Keys() {
		if name != "onchainAddress" && name != "paymentUri" {
			without.Set(name, fieldOf(object, name))
		}
	}
	var stored checkout.Invoice
	must(t, json.Unmarshal(jsonv.Marshal(without), &stored))
	m.Chain.SetDown(testkit.Explorer, true)
	status, err := c.GetStatus(ctx, stored)
	must(t, err)
	if status.State != checkout.StateOpen || status.Received != 0 {
		t.Errorf("%+v", status)
	}
}

func TestWaitForPaymentChecksTheOnchainPartWhenTheWebSocketSaysSomethingChanged(t *testing.T) {
	m := onchainMerchant(t)
	socket := m.Net.AddExplorerSocket()
	m.PublishLot(lot[:1], 91)
	c := clientOf(t, m)
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "W-1", Sats: 49_000})
	must(t, err)
	address := invoice.OnchainAddress
	var mu sync.Mutex
	var statuses []*checkout.PaymentStatus
	last := func() *checkout.PaymentStatus {
		mu.Lock()
		defer mu.Unlock()
		if len(statuses) == 0 {
			return nil
		}
		return statuses[len(statuses)-1]
	}
	type outcome struct {
		status *checkout.PaymentStatus
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		// Lightning checked once a minute: what moves here is the socket.
		status, err := c.WaitForPayment(ctx, invoice, checkout.WithInterval(60*time.Second), checkout.WithOnStatus(func(s *checkout.PaymentStatus) {
			mu.Lock()
			statuses = append(statuses, s)
			mu.Unlock()
		}))
		done <- outcome{status, err}
	}()
	testkit.Eventually(t, func() bool { return len(socket.Messages()) == 2 && last() != nil }, "the subscription and a status")
	same(t, "messages", socket.Messages(), []any{jsonv.O("action", "want", "data", []string{"blocks"}), jsonv.O("track-address", address)})
	if got := socket.Handshakes()[0].Get("User-Agent"); got != "sattleme-Checkout/1" {
		t.Errorf("User-Agent %q", got)
	}
	if last().State != checkout.StateOpen {
		t.Errorf("state %s", last().State)
	}
	tx := testkit.Payment(testkit.Txid("watched"), address, 49_000, nil, 0)
	m.Chain.Add(address, tx)
	socket.Push(jsonv.O("address-transactions", []any{tx.JSON()}))
	testkit.Eventually(t, func() bool { s := last(); return s != nil && s.State == checkout.StateUnconfirmed }, "unconfirmed")
	m.Chain.Confirm(address, tx.Txid, m.Chain.Height()-2)
	socket.Push(jsonv.O("block", jsonv.O("height", m.Chain.Height())))
	var result outcome
	select {
	case result = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("still waiting")
	}
	must(t, result.err)
	same(t, "paid", jsonOf(t, result.status), jsonv.O("state", "paid", "method", "onchain", "received", "49000", "txids", []string{tx.Txid}))
	// Closed once done.
	testkit.Eventually(t, func() bool { return socket.Open() == 0 }, "the socket closed")
}

func TestWaitForPaymentOfAnInvoiceWithAnAddressStopsWhenItsContextEnds(t *testing.T) {
	m := onchainMerchant(t)
	m.PublishLot(lot[:1], 91)
	c := clientOf(t, m)
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "W-2", Sats: 49_000})
	must(t, err)
	call, cancel := context.WithCancel(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err = c.WaitForPayment(call, invoice)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// SPEC.md §1, §4: WaitForPayment leaves nothing running once it returned. A full check it started
// waits for the explorer's answer (from a transport that does not heed the request's context: it
// answers once released) when the Lightning invoice is paid: the wait returns paid, the check it
// started stopped. Once the explorer answers, a part of the amount listed (the check would publish
// its witness, ask an invoice of the rest and declare it), nothing is asked nor published.
func TestWaitForPaymentLeavesNothingRunningOnceItReturned(t *testing.T) {
	m := onchainMerchant(t)
	socket := m.Net.AddExplorerSocket()
	m.PublishLot(lot[:1], 91)
	c := clientOf(t, m)
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "W-3", Sats: 49_000})
	must(t, err)
	address := invoice.OnchainAddress
	var statuses atomic.Int32
	type outcome struct {
		status *checkout.PaymentStatus
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		status, err := c.WaitForPayment(ctx, invoice, checkout.WithInterval(250*time.Millisecond),
			checkout.WithOnStatus(func(*checkout.PaymentStatus) { statuses.Add(1) }))
		done <- outcome{status, err}
	}()
	testkit.Eventually(t, func() bool { return statuses.Load() > 0 && len(socket.Messages()) == 2 }, "a first status")
	// The explorer's answers to the address's transactions, held until released.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)
	var held atomic.Int32
	m.SetOverride(func(req *http.Request) (testkit.Answer, bool) {
		if req.URL.Host == "mempool.space" && strings.HasSuffix(req.URL.Path, "/txs") {
			held.Add(1)
			<-release
		}
		return testkit.Answer{}, false
	})
	// A block: a full check starts, and waits for the explorer's answer.
	socket.Push(jsonv.O("block", jsonv.O("height", m.Chain.Height())))
	testkit.Eventually(t, func() bool { return held.Load() > 0 }, "a full check waiting for the explorer")
	// Meanwhile the Lightning invoice is paid: the wait returns.
	m.PayInvoice(invoice.PaymentHash)
	var result outcome
	select {
	case result = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("WaitForPayment waits for the check it started")
	}
	must(t, result.err)
	if result.status.State != checkout.StatePaid || result.status.Method != checkout.MethodLightning {
		t.Fatalf("%+v", result.status)
	}
	calls, invoices := len(m.HTTP.Calls()), len(m.Invoices())
	queries, received := map[string]int{}, map[string]int{}
	for _, url := range m.KeyRelays {
		queries[url], received[url] = len(m.Net.Get(url).Queries()), len(m.Net.Get(url).Received())
	}
	m.Chain.Add(address, testkit.Payment(testkit.Txid("late part"), address, 10_000, nil, 0))
	releaseAll()
	time.Sleep(2 * time.Second)
	if n := len(m.HTTP.Calls()) - calls; n > 0 {
		t.Errorf("%d HTTP requests once WaitForPayment returned", n)
	}
	if n := len(m.Invoices()) - invoices; n > 0 {
		t.Errorf("%d invoices asked once WaitForPayment returned", n)
	}
	for _, url := range m.KeyRelays {
		if n := len(m.Net.Get(url).Queries()) - queries[url]; n > 0 {
			t.Errorf("%s: %d reads once WaitForPayment returned", url, n)
		}
		if n := len(m.Net.Get(url).Received()) - received[url]; n > 0 {
			t.Errorf("%s: %d events published once WaitForPayment returned", url, n)
		}
	}
}

func TestACheckoutLinkToAnInvoiceWithAnAddressIsARecordOfVersion2(t *testing.T) {
	m := onchainMerchant(t)
	m.PublishLot(lot[:1], 91)
	invoice, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "C-1", Sats: 49_000, Checkout: true, RedirectURL: "https://shop.example/thanks"})
	must(t, err)
	fragment := checkoutlink.ParseFragment(invoice.CheckoutURL[strings.IndexByte(invoice.CheckoutURL, '#'):])
	if fragment.Status != checkoutlink.FragmentLink {
		t.Fatal("not a link")
	}
	address, encryptionKey := derive.CheckoutKeys(fragment.LinkKey)
	var event *nostr.Event
	testkit.Eventually(t, func() bool {
		for _, v := range held(m) {
			if e, ok := nostr.WithValidID(v); ok {
				if d, _ := e.DTag(); d == address {
					event = e
					return true
				}
			}
		}
		return false
	}, "the link on relay-a")
	plaintext, err := nip44.Decrypt(event.Content, encryptionKey)
	must(t, err)
	record, err := jsonv.ParseString(plaintext)
	must(t, err)
	keys := derive.RestKeys(m.IntegrationKey, "C-1", invoice.OnchainAddress, invoice.ExpiresAt)
	value := fieldOf(record, "v")
	same(t, "value", value, jsonv.O(
		"version", 2,
		"businessName", "Sunrise Bakery",
		"invoice", invoice.Bolt11,
		"verifyUrl", invoice.VerifyURL,
		"onchainAddress", invoice.OnchainAddress,
		"sats", 49000,
		"createdAt", invoice.CreatedAt,
		"expiresAt", invoice.ExpiresAt,
		"fiat", nil,
		"note", "C-1",
		"redirectUrl", "https://shop.example/thanks",
		// The rest records of this invoice of the order: written by the integration key alone.
		"restAuthor", keys.Author,
		"restKey", hex.EncodeToString(keys.Key),
	))
	parsed, err := checkoutlink.ParseValue(value)
	must(t, err)
	if parsed.OnchainAddress != invoice.OnchainAddress || parsed.Sats != 49_000 || parsed.ExpiresAt != invoice.ExpiresAt ||
		parsed.Rest == nil || parsed.Rest.Author != keys.Author || hex.EncodeToString(parsed.Rest.Key) != hex.EncodeToString(keys.Key) {
		t.Errorf("%+v", parsed)
	}
}
