package checkout_test

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/onchain"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/testkit"
)

// The rest of an on-chain underpayment (SPEC.md §31, "The rest of an underpayment"), end to end
// against local relays and fakes of breez.tips and of the explorers: the invoice of the rest asked
// by a payment check, declared with rest and published in a rest record that a checkout link's
// page reads; asked once, never without time left nor outside the Lightning address's bounds; a
// failure only logged; what the rest invoices paid counted with the on-chain payment (mixed).

// restAddress is the one address of the lot.
var restAddress = lot[0]

// restSetup is a merchant whose channel names its relays and a web app for checkout links, its
// lot of one address, a client whose warnings go to log, and the invoice of the order R-1: 49 000
// sats for 600 s, with a checkout link, leased the address.
type restSetup struct {
	m       *testkit.Merchant
	c       *checkout.Client
	log     *testkit.Logger
	invoice *checkout.Invoice
}

func newRestSetup(t *testing.T) *restSetup {
	t.Helper()
	m := testkit.NewMerchant()
	m.Start(nil, nil)
	t.Cleanup(m.Stop)
	m.PublishChannel(m.ChannelValue(100, jsonv.Member{Name: "relays", Value: m.KeyRelays}, jsonv.Member{Name: "checkout", Value: jsonv.O("origin", origin)}), 100)
	m.PublishLot([]string{restAddress}, 90)
	s := &restSetup{m: m, log: &testkit.Logger{}}
	s.c = s.client(t)
	invoice, err := s.c.CreateInvoice(ctx, checkout.Order{Ref: "R-1", Sats: 49_000, DurationSeconds: 600, Note: "Order 1", Checkout: true})
	must(t, err)
	if invoice.OnchainAddress != restAddress {
		t.Fatalf("leased %q", invoice.OnchainAddress)
	}
	s.invoice = invoice
	return s
}

func (s *restSetup) client(t *testing.T, opts ...checkout.Option) *checkout.Client {
	t.Helper()
	return clientOf(t, s.m, append([]checkout.Option{checkout.WithLogger(s.log)}, opts...)...)
}

func (s *restSetup) status(t *testing.T) any {
	t.Helper()
	return statusJSON(t, s.c, s.invoice)
}

func (s *restSetup) pay(sats int64, seed string) *testkit.EsploraTx {
	tx := testkit.Payment(testkit.Txid(seed), restAddress, sats, nil, 0)
	s.m.Chain.Add(restAddress, tx)
	return tx
}

// keys are the rest keys of the invoice.
func (s *restSetup) keys() derive.Rest {
	return derive.RestKeys(s.m.IntegrationKey, s.invoice.Ref, restAddress, s.invoice.ExpiresAt)
}

// madeOf are the invoices breez.tips made of sats, in order.
func madeOf(m *testkit.Merchant, sats int64) []*testkit.MadeInvoice {
	var out []*testkit.MadeInvoice
	for _, made := range m.Invoices() {
		if made.AmountMsat == sats*1000 {
			out = append(out, made)
		}
	}
	return out
}

// declaredOf are the declarations of the sale of ref a relay holds, opened as the sale.
func declaredOf(m *testkit.Merchant, relay, ref string) []*declaration.Opened {
	sale := m.Sale(ref)
	var out []*declaration.Opened
	for _, v := range m.Net.Get(relay).Events() {
		if opened, newer, ok := declaration.Open(v, sale, nil); ok && !newer {
			out = append(out, opened)
		}
	}
	return out
}

// restsDeclared are the payment hashes declared with rest for the sale of R-1 on the first relay.
func (s *restSetup) restsDeclared() []string {
	var out []string
	for _, d := range declaredOf(s.m, s.m.KeyRelays[0], "R-1") {
		if d.Rest != nil {
			out = append(out, d.Decoded.PaymentHash)
		}
	}
	return out
}

// restRecords are the rest records of the invoice a relay holds, as a checkout link's page opens
// them.
func (s *restSetup) restRecords(relay string) []onchain.RestInvoice {
	terms := onchain.RestLinkTerms{Sats: s.invoice.Sats, CreatedAt: s.invoice.CreatedAt, ExpiresAt: s.invoice.ExpiresAt}
	var out []onchain.RestInvoice
	for _, v := range s.m.Net.Get(relay).Events() {
		if invoice, _, newer, ok := onchain.OpenRestRecord(v, onchain.ReaderOf(s.keys()), terms, nil); ok && !newer {
			out = append(out, invoice)
		}
	}
	return out
}

// recordedOn is how many relays hold an event of the rest author.
func (s *restSetup) recordedOn() int {
	author := s.keys().Author
	n := 0
	for _, url := range s.m.KeyRelays {
		for _, v := range s.m.Net.Get(url).Events() {
			if pubkey, _ := jsonv.Field(v, "pubkey"); pubkey == author {
				n++
				break
			}
		}
	}
	return n
}

// refuseAuthor has relays refuse the events of author with reason.
func refuseAuthor(m *testkit.Merchant, relays []string, author, reason string) {
	for _, url := range relays {
		r := m.Net.Get(url)
		b := r.Behaviour()
		b.RefuseEvent = func(e *nostr.Event) string {
			if e.PubKey == author {
				return reason
			}
			return ""
		}
		r.SetBehaviour(b)
	}
}

// accept has relays take every event again, and answer at once.
func accept(m *testkit.Merchant, relays ...string) {
	for _, url := range relays {
		m.Net.Get(url).SetBehaviour(testkit.Behaviour{})
	}
}

func restOf(t *testing.T, status any) any {
	t.Helper()
	return fieldOf(status, "rest")
}

func TestAnUnderpaymentAsksAnInvoiceOfTheRestDeclaresItAndPublishesItsRecord(t *testing.T) {
	s := newRestSetup(t)
	s.m.SetNow(s.m.Now() + 60)
	s.pay(20_000, "part")
	status := s.status(t)
	made := madeOf(s.m, 29_000)
	if len(made) != 1 {
		t.Fatalf("%d invoices of 29 000 sats", len(made))
	}
	same(t, "status", status, jsonv.O(
		"state", "open",
		"expiresAt", s.invoice.ExpiresAt,
		"received", "20000",
		"rest", jsonv.O("sats", "29000", "bolt11", made[0].PR, "paymentUri", "bitcoin:"+restAddress+"?amount=0.00029&lightning="+made[0].PR),
	))
	// Until the invoice's end, with the order's note.
	if made[0].Expiry != s.invoice.ExpiresAt-s.m.Now() || made[0].Comment != "Order 1" {
		t.Errorf("expiry %d, comment %q", made[0].Expiry, made[0].Comment)
	}
	// Declared for the order's sale, with the order's price and note, marked as a rest.
	testkit.Eventually(t, func() bool { return len(s.restsDeclared()) == 1 }, "the declaration on relay-a")
	for _, d := range declaredOf(s.m, s.m.KeyRelays[0], "R-1") {
		if d.Rest == nil {
			continue
		}
		if d.Invoice != made[0].PR || d.Note != "Order 1" || *d.Rest != (declaration.RestOf{Address: restAddress, ExpiresAt: s.invoice.ExpiresAt}) ||
			d.Price != (declaration.Price{Minor: 49_000, Digits: 0, Currency: "SAT"}) {
			t.Errorf("declared %+v", d)
		}
	}
	// Its record, which the checkout link's page reads with the keys its link's record carries.
	testkit.Eventually(t, func() bool { return len(s.restRecords(s.m.KeyRelays[0])) == 1 }, "the record on relay-a")
	if r := s.restRecords(s.m.KeyRelays[0])[0]; r.Bolt11 != made[0].PR || r.Sats != 29_000 || r.VerifyURL != made[0].Verify {
		t.Errorf("record %+v", r)
	}
	l := openLink(t, s.m, s.invoice.CheckoutURL)
	if rest := l.record.Rest; rest == nil || rest.Author != s.keys().Author || string(rest.Key) != string(s.keys().Key) {
		t.Errorf("the link's rest %+v", l.record.Rest)
	}
	// The rest invoice is no invoice of the order.
	found, err := s.c.FindInvoices(ctx, "R-1")
	must(t, err)
	if len(found) != 1 || found[0].Bolt11 != s.invoice.Bolt11 || found[0].OnchainAddress != restAddress {
		t.Errorf("found %+v", found)
	}
	if msgs := s.log.Messages(); len(msgs) != 0 {
		t.Errorf("warnings %q", msgs)
	}
}

func TestCreateInvoicesCheckOfThePendingInvoiceAsksTheRestBeforeInvoicePending(t *testing.T) {
	s := newRestSetup(t)
	s.m.SetNow(s.m.Now() + 60)
	s.pay(20_000, "part")
	// Its payment checked, alone (SPEC.md §21 step 1): as any payment check, it asks for the rest.
	_, err := s.c.CreateInvoice(ctx, checkout.Order{Ref: "R-1", Sats: 49_000, Checkout: true})
	if pending := wantCode(t, err, checkout.CodeInvoicePending); pending.Invoice.OnchainAddress != restAddress {
		t.Errorf("pending on %q", pending.Invoice.OnchainAddress)
	}
	made := madeOf(s.m, 29_000)
	if len(made) != 1 {
		t.Fatalf("%d invoices of 29 000 sats", len(made))
	}
	testkit.Eventually(t, func() bool { return len(s.restsDeclared()) == 1 }, "the declaration on relay-a")
	// Its record, for the pages of the order's checkout links (the new one included).
	testkit.Eventually(t, func() bool { return len(s.restRecords(s.m.KeyRelays[0])) == 1 }, "the record on relay-a")
	if r := s.restRecords(s.m.KeyRelays[0])[0]; r.Bolt11 != made[0].PR {
		t.Errorf("record %+v", r)
	}
	// Shown by the next check, never asked again.
	if bolt11 := fieldOf(restOf(t, s.status(t)), "bolt11"); bolt11 != made[0].PR {
		t.Errorf("rest bolt11 %v", bolt11)
	}
	if n := len(madeOf(s.m, 29_000)); n != 1 {
		t.Errorf("%d invoices of 29 000 sats", n)
	}
	if msgs := s.log.Messages(); len(msgs) != 0 {
		t.Errorf("warnings %q", msgs)
	}
}

func TestTheRestIsAskedOnceAndAgainForANewRest(t *testing.T) {
	s := newRestSetup(t)
	s.pay(20_000, "part")
	first := s.status(t)
	same(t, "another client", statusJSON(t, s.client(t), s.invoice), first)
	if n := len(madeOf(s.m, 29_000)); n != 1 {
		t.Errorf("%d invoices of 29 000 sats", n)
	}
	// More sent on-chain: a new rest, a new invoice of it.
	s.pay(10_000, "more")
	second := s.status(t)
	if fieldOf(second, "received") != "30000" || fieldOf(restOf(t, second), "sats") != "19000" {
		t.Errorf("%s", jsonv.Marshal(second))
	}
	if n := len(madeOf(s.m, 19_000)); n != 1 {
		t.Errorf("%d invoices of 19 000 sats", n)
	}
	testkit.Eventually(t, func() bool { return len(s.restRecords(s.m.KeyRelays[0])) == 2 }, "two records on relay-a")
	var sats []int64
	for _, r := range s.restRecords(s.m.KeyRelays[0]) {
		sats = append(sats, r.Sats)
	}
	sort.Slice(sats, func(i, j int) bool { return sats[i] < sats[j] })
	same(t, "records", sats, []int64{19_000, 29_000})
}

func TestARestBelow1000SatsIsLightningOnly(t *testing.T) {
	s := newRestSetup(t)
	s.pay(48_500, "part")
	status := s.status(t)
	made := madeOf(s.m, 500)
	if len(made) != 1 {
		t.Fatalf("%d invoices of 500 sats", len(made))
	}
	if fieldOf(status, "state") != "open" || fieldOf(status, "received") != "48500" {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	same(t, "rest", restOf(t, status), jsonv.O("sats", "500", "bolt11", made[0].PR, "paymentUri", "lightning:"+made[0].PR))
}

func TestNoRestIsAskedWithLessThanAMinuteLeftNorOutsideTheLightningAddressesBounds(t *testing.T) {
	s := newRestSetup(t)
	s.pay(20_000, "part")
	s.m.SetNow(s.invoice.ExpiresAt - 59)
	same(t, "a minute", restOf(t, s.status(t)), jsonv.O("sats", "29000", "bolt11", nil, "paymentUri", "bitcoin:"+restAddress+"?amount=0.00029"))
	s.m.SetNow(s.invoice.ExpiresAt - 600)
	s.m.SetPay(pay("maxSendable", 28_000_000))
	if bolt11 := fieldOf(restOf(t, s.status(t)), "bolt11"); bolt11 != nil {
		t.Errorf("above the maximum: %v", bolt11)
	}
	s.m.SetPay(pay("maxSendable", int64(100_000_000_000)), pay("minSendable", 29_001_000))
	if bolt11 := fieldOf(restOf(t, s.status(t)), "bolt11"); bolt11 != nil {
		t.Errorf("below the minimum: %v", bolt11)
	}
	if n := len(madeOf(s.m, 29_000)); n != 0 {
		t.Errorf("%d invoices of 29 000 sats", n)
	}
	if msgs := s.log.Messages(); len(msgs) != 0 {
		t.Errorf("warnings %q", msgs)
	}
	// Past the end, no rest at all.
	s.m.SetNow(s.invoice.ExpiresAt)
	if status := s.status(t); fieldOf(status, "state") != "open" || restOf(t, status) != nil {
		t.Errorf("%s", jsonv.Marshal(status))
	}
}

func TestALightningAddressThatFailsIsLoggedAndAskedAgainAtTheNextCheck(t *testing.T) {
	s := newRestSetup(t)
	s.pay(20_000, "part")
	s.m.SetOverride(answerAt(testkit.Callback, testkit.Status(500, jsonv.O("oops", true))))
	status := s.status(t)
	if fieldOf(status, "state") != "open" || fieldOf(restOf(t, status), "bolt11") != nil {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	if msgs := s.log.Messages(); len(msgs) != 1 || !strings.Contains(msgs[0], "no Lightning invoice of the rest") {
		t.Errorf("warnings %q", msgs)
	}
	s.m.SetOverride(nil)
	made := func() string { all := madeOf(s.m, 29_000); return all[len(all)-1].PR }
	if bolt11 := fieldOf(restOf(t, s.status(t)), "bolt11"); bolt11 != made() {
		t.Errorf("rest bolt11 %v", bolt11)
	}
}

// SPEC.md §16, §23: a pay request whose metadata holds a lone surrogate is invalid_invoice; asked
// for the invoice of an underpayment's rest, it is logged, and the status still given, without an
// invoice of the rest (none asked); the next check asks again.
func TestAPayRequestWhoseMetadataHoldsALoneSurrogateAsksNoRestAndIsLogged(t *testing.T) {
	s := newRestSetup(t)
	s.pay(20_000, "part")
	s.m.SetPay(pay("metadata", "[[\"text/plain\",\"Pay \xed\xa0\x80\"]]"))
	status := s.status(t)
	if fieldOf(status, "state") != "open" || fieldOf(status, "received") != "20000" || fieldOf(restOf(t, status), "bolt11") != nil {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	if msgs := s.log.Messages(); len(msgs) != 1 || !strings.Contains(msgs[0], "no Lightning invoice of the rest") {
		t.Errorf("warnings %q", msgs)
	}
	if n := len(madeOf(s.m, 29_000)); n != 0 {
		t.Errorf("%d invoices of the rest asked", n)
	}
}

func TestAnInvoiceOfTheRestThatDoesNotPayTheMerchantIsRefusedAndLogged(t *testing.T) {
	s := newRestSetup(t)
	s.pay(20_000, "part")
	s.m.SetTamper(func(made *testkit.MadeInvoice) { made.PR = made.PR[:len(made.PR)-8] + "qqqqqqqq" })
	if bolt11 := fieldOf(restOf(t, s.status(t)), "bolt11"); bolt11 != nil {
		t.Errorf("rest bolt11 %v", bolt11)
	}
	if msgs := s.log.Messages(); len(msgs) != 1 {
		t.Errorf("warnings %q", msgs)
	}
	time.Sleep(100 * time.Millisecond)
	if declared := s.restsDeclared(); len(declared) != 0 {
		t.Errorf("declared %v", declared)
	}
}

func TestAnInvoiceOfTheRestAnotherCheckAskedMeanwhileIsChecked(t *testing.T) {
	s := newRestSetup(t)
	s.pay(20_000, "part")
	// Paid as soon as it is made: the check that waited for the other finds it paid.
	s.m.SetTamper(func(made *testkit.MadeInvoice) { s.m.PayInvoice(made.PaymentHash) })
	var wg sync.WaitGroup
	states := make([]any, 2)
	for i := range states {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			states[i] = s.status(t)
		}(i)
	}
	wg.Wait()
	made := madeOf(s.m, 29_000)
	if len(made) != 1 {
		t.Fatalf("%d invoices of 29 000 sats", len(made))
	}
	// The one that asked it shows it; the one that waited finds it paid.
	if fieldOf(states[0], "state") == "unconfirmed" {
		states[0], states[1] = states[1], states[0]
	}
	if fieldOf(states[0], "state") != "open" || fieldOf(restOf(t, states[0]), "bolt11") != made[0].PR {
		t.Errorf("the first %s", jsonv.Marshal(states[0]))
	}
	if fieldOf(states[1], "state") != "unconfirmed" || fieldOf(states[1], "received") != "49000" {
		t.Errorf("the second %s", jsonv.Marshal(states[1]))
	}
}

func TestAnInvoiceOfTheRestAMinorityHoldsIsDeclaredOnAMajorityBeforeItIsUsed(t *testing.T) {
	s := newRestSetup(t)
	s.pay(20_000, "part")
	sale := s.m.Sale("R-1")
	a, b, c := s.m.KeyRelays[0], s.m.KeyRelays[1], s.m.KeyRelays[2]
	// Its declaration reaches one relay of three: not to be shown.
	refuseAuthor(s.m, []string{b, c}, sale.Author, "blocked")
	if bolt11 := fieldOf(restOf(t, s.status(t)), "bolt11"); bolt11 != nil {
		t.Errorf("rest bolt11 %v", bolt11)
	}
	if msgs := s.log.Messages(); len(msgs) != 1 {
		t.Errorf("warnings %q", msgs)
	}
	made := madeOf(s.m, 29_000)
	if len(made) != 1 {
		t.Fatalf("%d invoices of 29 000 sats", len(made))
	}
	testkit.Eventually(t, func() bool { return len(s.restsDeclared()) == 1 }, "the declaration on relay-a")
	// Still refused: read on the relay that holds it (relay-c never answers the read of the sale's
	// records: the majority read is relay-a's and relay-b's, as node's relays answer in their
	// order), it cannot be declared on a majority, so nothing is told of the rest (and no other
	// invoice of it is asked).
	slow := s.m.Net.Get(c).Behaviour()
	slow.MuteQuery = readsOf(sale.Author)
	s.m.Net.Get(c).SetBehaviour(slow)
	_, err := s.c.GetStatus(ctx, s.invoice)
	wantCode(t, err, checkout.CodeNetwork)
	if n := len(madeOf(s.m, 29_000)); n != 1 {
		t.Errorf("%d invoices of 29 000 sats", n)
	}
	// Taken again, the third relay slow: read on the first two, declared on a majority, shown.
	accept(s.m, b)
	s.m.Net.Get(c).SetBehaviour(testkit.Behaviour{MuteQuery: readsOf(sale.Author)})
	if bolt11 := fieldOf(restOf(t, s.status(t)), "bolt11"); bolt11 != made[0].PR {
		t.Errorf("rest bolt11 %v", bolt11)
	}
	declared := func(relay string) bool {
		for _, d := range declaredOf(s.m, relay, "R-1") {
			if d.Decoded.PaymentHash == made[0].PaymentHash {
				return true
			}
		}
		return false
	}
	n := 0
	for _, relay := range []string{a, b, c} {
		if declared(relay) {
			n++
		}
	}
	if n < 2 {
		t.Errorf("declared on %d relays", n)
	}
	// Paid, then read without the first relay: still counted, never asked again.
	s.m.PayInvoice(made[0].PaymentHash)
	accept(s.m, c)
	s.m.Net.Get(a).SetBehaviour(testkit.Behaviour{MuteQuery: readsOf(sale.Author)})
	if status := s.status(t); fieldOf(status, "state") != "unconfirmed" || fieldOf(status, "received") != "49000" {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	if n := len(madeOf(s.m, 29_000)); n != 1 {
		t.Errorf("%d invoices of 29 000 sats", n)
	}
}

func TestAnInvoiceOfTheRestIsShownOnlyOnceItsRecordIsOnAMajority(t *testing.T) {
	s := newRestSetup(t)
	s.pay(20_000, "part")
	// Its record taken by one relay of three: a checkout link's page reading the other two would
	// not see it, nor its payment. Declared, never shown.
	refuseAuthor(s.m, s.m.KeyRelays[1:], s.keys().Author, "blocked: not now")
	same(t, "rest", restOf(t, s.status(t)), jsonv.O("sats", "29000", "bolt11", nil, "paymentUri", "bitcoin:"+restAddress+"?amount=0.00029"))
	testkit.Eventually(t, func() bool { return s.recordedOn() == 1 }, "the record on relay-a")
	if msgs := s.log.Messages(); len(msgs) == 0 || !strings.Contains(msgs[0], "record of an invoice of the rest was not published") {
		t.Errorf("warnings %q", msgs)
	}
	// Still refused: still not shown, never asked again.
	if bolt11 := fieldOf(restOf(t, s.status(t)), "bolt11"); bolt11 != nil {
		t.Errorf("rest bolt11 %v", bolt11)
	}
	made := madeOf(s.m, 29_000)
	if len(made) != 1 {
		t.Fatalf("%d invoices of 29 000 sats", len(made))
	}
	// Taken: published again on a majority at the next check, shown.
	accept(s.m, s.m.KeyRelays...)
	if bolt11 := fieldOf(restOf(t, s.status(t)), "bolt11"); bolt11 != made[0].PR {
		t.Errorf("rest bolt11 %v", bolt11)
	}
	if n := s.recordedOn(); n < 2 {
		t.Errorf("recorded on %d relays", n)
	}
	if n := len(madeOf(s.m, 29_000)); n != 1 {
		t.Errorf("%d invoices of 29 000 sats", n)
	}
}

func TestARecordAMajorityTookInTheCheckIsShownThoughFewerThanAMajorityAnswerTheReadOfRecords(t *testing.T) {
	s := newRestSetup(t)
	s.pay(20_000, "part")
	author := s.keys().Author
	// Two relays of three take its record, then stall on the read of the rest records.
	stall := func(f *jsonv.Object) bool {
		authors, _ := f.Get("authors")
		items, _ := authors.([]any)
		for _, item := range items {
			if item == author {
				return !f.Has("#d")
			}
		}
		return false
	}
	for _, url := range s.m.KeyRelays[1:] {
		s.m.Net.Get(url).SetBehaviour(testkit.Behaviour{MuteQuery: stall})
	}
	status := s.status(t)
	made := madeOf(s.m, 29_000)
	if len(made) != 1 {
		t.Fatalf("%d invoices of 29 000 sats", len(made))
	}
	if rest := restOf(t, status); fieldOf(rest, "sats") != "29000" || fieldOf(rest, "bolt11") != made[0].PR {
		t.Errorf("rest %s", jsonv.Marshal(rest))
	}
	testkit.Eventually(t, func() bool { return len(s.restRecords(s.m.KeyRelays[0])) == 1 }, "the record on relay-a")
	// The next check, which took nothing, cannot tell: not shown until a majority answers, and no
	// record published again (what the relays that did not answer hold is never written over).
	sent := map[string]int{}
	for _, url := range s.m.KeyRelays {
		sent[url] = len(s.m.Net.Get(url).Received())
	}
	if bolt11 := fieldOf(restOf(t, s.status(t)), "bolt11"); bolt11 != nil {
		t.Errorf("rest bolt11 %v", bolt11)
	}
	time.Sleep(100 * time.Millisecond)
	for _, url := range s.m.KeyRelays {
		for _, e := range s.m.Net.Get(url).Received()[sent[url]:] {
			if e.PubKey == author {
				t.Errorf("%s: a rest record published again after a read fewer than a majority answered", url)
			}
		}
	}
	accept(s.m, s.m.KeyRelays...)
	if bolt11 := fieldOf(restOf(t, s.status(t)), "bolt11"); bolt11 != made[0].PR {
		t.Errorf("rest bolt11 %v", bolt11)
	}
	if n := len(madeOf(s.m, 29_000)); n != 1 {
		t.Errorf("%d invoices of 29 000 sats", n)
	}
}

// SPEC.md §12: a record of a newer format is never written over. A newer version wrote the record
// of a rest invoice in a format this one cannot read: its address is held, nothing is published
// over it, and the package says it should be updated.
func TestARestRecordIsNeverPublishedOverOneOfANewerFormat(t *testing.T) {
	s := newRestSetup(t)
	s.pay(20_000, "part")
	keys := s.keys()
	relays := s.m.KeyRelays
	// Its record taken by the first relay alone: not known on a majority yet.
	refuseAuthor(s.m, relays[1:], keys.Author, "blocked: not now")
	s.status(t)
	made := madeOf(s.m, 29_000)
	if len(made) != 1 {
		t.Fatalf("%d invoices of 29 000 sats", len(made))
	}
	newer, err := nostr.Seal(nostr.SealOptions{
		Type:          proto.RestRecordType,
		Value:         jsonv.O("version", 2),
		Signer:        keys.SecretKey,
		EncryptionKey: keys.EncryptionKey,
		Address:       derive.RestRecordAddress(keys.Key, made[0].PaymentHash),
		CreatedAt:     s.m.Now() - 1,
		Tags:          [][]string{{"expiration", itoa(s.invoice.ExpiresAt + 604_800)}},
		Random:        testkit.Random("newer rest record"),
	})
	must(t, err)
	second := s.m.Net.Get(relays[1])
	second.Take(newer)
	accept(s.m, relays[:2]...)
	// The third relay never answers the read of the rest records: the read is the first two's (it
	// would not see a record the third alone holds).
	s.m.Net.Get(relays[2]).SetBehaviour(testkit.Behaviour{MuteQuery: readsOf(keys.Author)})
	sent := len(second.Received())
	before := len(s.log.Messages())
	// Its address is held: nothing published over the record of the newer format.
	if bolt11 := fieldOf(restOf(t, s.status(t)), "bolt11"); bolt11 != made[0].PR {
		t.Errorf("rest bolt11 %v", bolt11)
	}
	time.Sleep(100 * time.Millisecond)
	for _, e := range second.Received()[sent:] {
		if e.PubKey == keys.Author {
			t.Error("a rest record published over the newer one")
		}
	}
	if !contains(eventIDs(second.Events()), newer.ID) {
		t.Error("the newer record replaced")
	}
	if !hasMessage(s.log.Messages()[before:], "a record of an invoice of the rest has a newer format") {
		t.Errorf("warnings %q", s.log.Messages())
	}
}

// SPEC.md §31 (as the coordinated update states it): the decision to ask for a rest invoice, its
// expiry, its checks and its records take the clock read when deciding, inside the order's
// critical section. A clock that moves on while breez.tips makes the invoice: its declaration and
// its record are dated at the decision, the expiry asked running to the invoice's end from it.
func TestTheRestIsAskedDeclaredAndRecordedAtTheTimeOfTheDecision(t *testing.T) {
	s := newRestSetup(t)
	s.pay(20_000, "part")
	s.m.SetOverride(func(req *http.Request) (testkit.Answer, bool) {
		if req.URL.Scheme+"://"+req.URL.Host+req.URL.Path == testkit.Callback {
			s.m.SetNow(s.m.Now() + 5)
		}
		return testkit.Answer{}, false
	})
	decidedAt := s.m.Now()
	s.status(t)
	made := madeOf(s.m, 29_000)
	if len(made) != 1 {
		t.Fatalf("%d invoices of 29 000 sats", len(made))
	}
	if made[0].Expiry != s.invoice.ExpiresAt-decidedAt {
		t.Errorf("expiry %d, want %d", made[0].Expiry, s.invoice.ExpiresAt-decidedAt)
	}
	sale := s.m.Sale("R-1")
	var declared, recorded *nostr.Event
	testkit.Eventually(t, func() bool {
		declared, recorded = nil, nil
		for _, e := range s.m.Net.Get(s.m.KeyRelays[0]).Received() {
			if opened, _, ok := declaration.Open(testkit.EventValue(e), sale, nil); ok && opened.Rest != nil {
				declared = e
			}
			if e.PubKey == s.keys().Author {
				recorded = e
			}
		}
		return declared != nil && recorded != nil
	}, "the declaration and the record on relay-a")
	if declared.CreatedAt != decidedAt || recorded.CreatedAt != decidedAt {
		t.Errorf("declared at %d, recorded at %d, decided at %d", declared.CreatedAt, recorded.CreatedAt, decidedAt)
	}
}

// SPEC.md §31: the check decides with the clock read once its turn to ask came, after
// the rest invoices were read again, never earlier: a check that waited has less time left. A clock
// that moves on while the check reads them again: the expiry asked runs to the invoice's end from
// the later time, and the declaration and the record are dated then.
func TestTheRestIsDecidedWithTheClockReadAfterTheRestInvoicesWereReadAgain(t *testing.T) {
	s := newRestSetup(t)
	s.pay(20_000, "part")
	sale := s.m.Sale("R-1")
	before := s.m.Now()
	var mu sync.Mutex
	firstPages := map[string]int{}
	moved := false
	for _, url := range s.m.KeyRelays {
		url := url
		s.m.Net.Get(url).SetBehaviour(testkit.Behaviour{Raw: func(message []any, _ func(string)) bool {
			if message[0] != "REQ" || len(message) < 3 {
				return false
			}
			filter, _ := jsonv.AsObject(message[2])
			authors, _ := filter.Get("authors")
			_, paged := filter.Get("until")
			if list, ok := authors.([]any); !ok || len(list) != 1 || list[0] != sale.Author || paged {
				return false
			}
			mu.Lock()
			defer mu.Unlock()
			firstPages[url]++
			// A relay's second read of the sale's records in the check: the rest invoices read again.
			if firstPages[url] == 2 && !moved {
				moved = true
				s.m.SetNow(s.m.Now() + 5)
			}
			return false
		}})
	}
	s.status(t)
	mu.Lock()
	if !moved {
		t.Fatal("the rest invoices were not read again")
	}
	mu.Unlock()
	decidedAt := before + 5
	made := madeOf(s.m, 29_000)
	if len(made) != 1 {
		t.Fatalf("%d invoices of 29 000 sats", len(made))
	}
	if made[0].Expiry != s.invoice.ExpiresAt-decidedAt {
		t.Errorf("expiry %d, want %d", made[0].Expiry, s.invoice.ExpiresAt-decidedAt)
	}
	var declared, recorded *nostr.Event
	testkit.Eventually(t, func() bool {
		declared, recorded = nil, nil
		for _, e := range s.m.Net.Get(s.m.KeyRelays[0]).Received() {
			if opened, _, ok := declaration.Open(testkit.EventValue(e), sale, nil); ok && opened.Rest != nil {
				declared = e
			}
			if e.PubKey == s.keys().Author {
				recorded = e
			}
		}
		return declared != nil && recorded != nil
	}, "the declaration and the record on relay-a")
	if declared.CreatedAt != decidedAt || recorded.CreatedAt != decidedAt {
		t.Errorf("declared at %d, recorded at %d, decided at %d", declared.CreatedAt, recorded.CreatedAt, decidedAt)
	}
}

// A mixed payment: on-chain, and the rest in Lightning.

// underpaid pays 20 000 sats on-chain and has the check ask the rest: the transaction, and the
// invoice of the rest breez.tips made.
func (s *restSetup) underpaid(t *testing.T) (*testkit.EsploraTx, *testkit.MadeInvoice) {
	t.Helper()
	tx := s.pay(20_000, "part")
	s.status(t)
	made := madeOf(s.m, 29_000)
	if len(made) != 1 {
		t.Fatalf("%d invoices of 29 000 sats", len(made))
	}
	return tx, made[0]
}

func TestAMixedPaymentIsUnconfirmedOnceTheRestIsPaidThenPaidWithThePreimage(t *testing.T) {
	s := newRestSetup(t)
	tx, rest := s.underpaid(t)
	s.m.PayInvoice(rest.PaymentHash)
	same(t, "unconfirmed", s.status(t), jsonv.O("state", "unconfirmed", "received", "49000", "confirmations", 0, "txids", []string{tx.Txid}))
	s.m.Chain.Confirm(restAddress, tx.Txid, s.m.Chain.Height()-2)
	s.m.SetNow(s.invoice.ExpiresAt + 3600)
	paid := jsonv.O("state", "paid", "method", "mixed", "received", "49000", "txids", []string{tx.Txid}, "preimages", []string{rest.Preimage})
	same(t, "paid", s.status(t), paid)
	// The order is paid: its next invoice is refused, without the preimage of its own invoice.
	_, err := s.c.CreateInvoice(ctx, checkout.Order{Ref: "R-1", Sats: 49_000})
	e := wantCode(t, err, checkout.CodeOrderPaid)
	if e.Preimage != "" || e.Status == nil || e.Status.Method != checkout.MethodMixed {
		t.Errorf("%+v", e)
	}
}

func TestTheRestPaidAndTheOnchainPartThreeDeepIsPaidMixed(t *testing.T) {
	s := newRestSetup(t)
	tx, rest := s.underpaid(t)
	s.m.PayInvoice(rest.PaymentHash)
	s.m.Chain.Confirm(restAddress, tx.Txid, s.m.Chain.Height()-1)
	if status := s.status(t); fieldOf(status, "state") != "unconfirmed" || fieldOf(status, "confirmations") != float64(2) {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	// 3 confirmations on the explorer read: paid, mixed.
	s.m.Chain.Confirm(restAddress, tx.Txid, s.m.Chain.Height()-2)
	same(t, "paid", s.status(t), jsonv.O("state", "paid", "method", "mixed", "received", "49000", "txids", []string{tx.Txid}, "preimages", []string{rest.Preimage}))
}

func TestARestPaidCountsWhenTheOnchainPaymentIsGoneAndExpiresWithAllReceived(t *testing.T) {
	s := newRestSetup(t)
	tx, rest := s.underpaid(t)
	s.m.PayInvoice(rest.PaymentHash)
	s.m.Chain.Drop(restAddress, tx.Txid)
	if status := s.status(t); fieldOf(status, "state") != "open" || fieldOf(status, "received") != "29000" || fieldOf(restOf(t, status), "sats") != "20000" {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	s.m.SetNow(s.invoice.ExpiresAt + 3600)
	same(t, "expired", s.status(t), jsonv.O("state", "expired", "received", "29000"))
}

func TestTheAddressIsNeverLeasedAgainForTheOrderOnceAWitnessNamesIt(t *testing.T) {
	s := newRestSetup(t)
	tx, rest := s.underpaid(t)
	s.m.PayInvoice(rest.PaymentHash)
	// The on-chain payment gone, the explorer shows nothing paid the address; its witness does
	// (SPEC.md §31, "Leasing an address"): the earlier invoice's 29 000 never counts for a new one.
	s.m.Chain.Drop(restAddress, tx.Txid)
	s.m.SetNow(s.invoice.ExpiresAt + 421)
	same(t, "expired", s.status(t), jsonv.O("state", "expired", "received", "29000"))
	next, err := s.c.CreateInvoice(ctx, checkout.Order{Ref: "R-1", Sats: 49_000, DurationSeconds: 600})
	must(t, err)
	if next.OnchainAddress != "" || next.Bolt11 == s.invoice.Bolt11 {
		t.Errorf("%+v", next)
	}
}

func TestTheOrderGetsANewInvoiceOnceTheInvoiceAndItsRestEndedUnpaid(t *testing.T) {
	s := newRestSetup(t)
	s.underpaid(t)
	s.m.SetNow(s.invoice.ExpiresAt + 100)
	_, err := s.c.CreateInvoice(ctx, checkout.Order{Ref: "R-1", Sats: 49_000})
	wantCode(t, err, checkout.CodeInvoicePending)
	// The rest invoice ended with the invoice: both final 120 s later.
	s.m.SetNow(s.invoice.ExpiresAt + 200)
	next, err := s.c.CreateInvoice(ctx, checkout.Order{Ref: "R-1", Sats: 49_000})
	must(t, err)
	// The address was paid: never leased again.
	if next.Bolt11 == s.invoice.Bolt11 || next.OnchainAddress != "" {
		t.Errorf("%+v", next)
	}
}

// waitFor runs WaitForPayment on the invoice, its statuses kept: its outcome comes on the channel.
func (s *restSetup) waitFor(t *testing.T, opts ...checkout.WaitOption) (<-chan waited, func() []*checkout.PaymentStatus) {
	t.Helper()
	var mu sync.Mutex
	var statuses []*checkout.PaymentStatus
	done := make(chan waited, 1)
	go func() {
		status, err := s.c.WaitForPayment(ctx, s.invoice, append([]checkout.WaitOption{checkout.WithOnStatus(func(st *checkout.PaymentStatus) {
			mu.Lock()
			statuses = append(statuses, st)
			mu.Unlock()
		})}, opts...)...)
		done <- waited{status, err}
	}()
	return done, func() []*checkout.PaymentStatus {
		mu.Lock()
		defer mu.Unlock()
		return append([]*checkout.PaymentStatus(nil), statuses...)
	}
}

type waited struct {
	status *checkout.PaymentStatus
	err    error
}

func outcomeOf(t *testing.T, done <-chan waited) waited {
	t.Helper()
	select {
	case w := <-done:
		return w
	case <-time.After(20 * time.Second):
		t.Fatal("still waiting")
		return waited{}
	}
}

func TestWaitForPaymentEndsPaidOnceTheRestIsPaidWithoutAPushOfTheExplorer(t *testing.T) {
	s := newRestSetup(t)
	s.m.Net.AddExplorerSocket()
	tx, rest := s.underpaid(t)
	s.m.Chain.Confirm(restAddress, tx.Txid, s.m.Chain.Height()-2)
	done, statuses := s.waitFor(t, checkout.WithInterval(250*time.Millisecond))
	testkit.Eventually(t, func() bool { return len(statuses()) > 0 }, "a status")
	if last := statuses()[len(statuses())-1]; last.State != checkout.StateOpen || last.Rest == nil || last.Rest.Sats != 29_000 {
		t.Errorf("%+v", last)
	}
	s.m.PayInvoice(rest.PaymentHash)
	w := outcomeOf(t, done)
	must(t, w.err)
	same(t, "paid", jsonOf(t, w.status), jsonv.O("state", "paid", "method", "mixed", "received", "49000", "txids", []string{tx.Txid}, "preimages", []string{rest.Preimage}))
}

func TestWaitForPaymentNeverEndsOnWhatWasDueBeforeTheRestWasPaid(t *testing.T) {
	s := newRestSetup(t)
	tx, rest := s.underpaid(t)
	s.m.Chain.Confirm(restAddress, tx.Txid, s.m.Chain.Height()-2)
	// The rest paid in time, its verify URL failing once past the grace period: the full check
	// (no push of the explorer here, the next one is 60 s later) judges the on-chain part against
	// the whole amount, final, and cannot tell the rest.
	s.m.PayInvoice(rest.PaymentHash)
	s.m.SetNow(s.invoice.ExpiresAt + 121)
	var mu sync.Mutex
	failures := 1
	s.m.SetOverride(func(req *http.Request) (testkit.Answer, bool) {
		mu.Lock()
		defer mu.Unlock()
		if req.URL.String() == rest.Verify {
			failures--
			if failures >= 0 {
				return testkit.Status(500, jsonv.O("oops", true)), true
			}
		}
		return testkit.Answer{}, false
	})
	done, statuses := s.waitFor(t, checkout.WithInterval(250*time.Millisecond))
	// The rest seen paid by the next Lightning check: only a full check, with the 20 000 on-chain
	// all that is due, gives the status (never "expired" with 49 000 received).
	w := outcomeOf(t, done)
	must(t, w.err)
	same(t, "paid", jsonOf(t, w.status), jsonv.O("state", "paid", "method", "mixed", "received", "49000", "txids", []string{tx.Txid}, "preimages", []string{rest.Preimage}))
	// Failed once, then read by the Lightning check and by the full check it started.
	mu.Lock()
	if failures != -2 {
		t.Errorf("verify asked %d times", 1-failures)
	}
	mu.Unlock()
	for _, st := range statuses() {
		if st.State == checkout.StateExpired {
			t.Errorf("%+v", st)
		}
	}
}

// eventIDs are the ids of the events a relay holds.
func eventIDs(events []any) []string {
	var out []string
	for _, v := range events {
		id, _ := jsonv.Field(v, "id")
		s, _ := id.(string)
		out = append(out, s)
	}
	return out
}

// hasMessage tells whether one of messages contains part.
func hasMessage(messages []string, part string) bool {
	for _, m := range messages {
		if strings.Contains(m, part) {
			return true
		}
	}
	return false
}
