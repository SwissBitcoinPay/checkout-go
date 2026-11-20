package checkout_test

import (
	"encoding/json"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/checkoutlink"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/onchain"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/testkit"
)

// The merchant's tolerance of on-chain underpayments (SPEC.md §31, "The merchant's tolerance",
// "Acceptance records"), end to end against local relays and fakes of breez.tips and of the
// explorers: read in the channel, kept in the order's lease, its payment judged with it whatever
// the channel says later; a payment short by no more than it is paid (being confirmed first), one
// short by more asks its whole rest; the pages of the order's checkout links told by an acceptance
// record, never the tolerance itself.

const (
	// tolerance is 5 %: of 49 000 sats, 2 450 may be missing, 46 550 pay the order.
	tolerance     = 500
	toleranceSats = 49_000
	leastAccepted = 46_550
)

// toleranceSetup is a merchant with its lot of one address, and a client whose warnings go to log.
type toleranceSetup struct {
	m   *testkit.Merchant
	c   *checkout.Client
	log *testkit.Logger
}

func newToleranceSetup(t *testing.T) *toleranceSetup {
	t.Helper()
	m := testkit.NewMerchant()
	m.Start(nil, nil)
	t.Cleanup(m.Stop)
	m.PublishLot([]string{restAddress}, 90)
	s := &toleranceSetup{m: m, log: &testkit.Logger{}}
	s.c = clientOf(t, m, checkout.WithLogger(s.log))
	return s
}

// publishChannel publishes the channel with value as its onchainTolerance (absent: none), dated
// createdAt.
func (s *toleranceSetup) publishChannel(value any, createdAt int64, absent bool) {
	members := []jsonv.Member{{Name: "relays", Value: s.m.KeyRelays}, {Name: "checkout", Value: jsonv.O("origin", origin)}}
	if !absent {
		members = append(members, jsonv.Member{Name: "onchainTolerance", Value: value})
	}
	s.m.PublishChannel(s.m.ChannelValue(createdAt, members...), createdAt)
}

func (s *toleranceSetup) createInvoice(t *testing.T, ref string) *checkout.Invoice {
	t.Helper()
	invoice, err := s.c.CreateInvoice(ctx, checkout.Order{Ref: ref, Sats: toleranceSats, DurationSeconds: 600, Note: "Order 1", Checkout: true})
	must(t, err)
	if invoice.OnchainAddress != restAddress {
		t.Fatalf("leased %q", invoice.OnchainAddress)
	}
	return invoice
}

// leaseInside is the inside of the sale of ref's lease the first relay holds.
func (s *toleranceSetup) leaseInside(t *testing.T, ref string) *jsonv.Object {
	t.Helper()
	sale := s.m.Sale(ref)
	var found []*jsonv.Object
	testkit.Eventually(t, func() bool {
		found = nil
		for _, v := range s.m.Net.Get(s.m.KeyRelays[0]).Events() {
			if record, ok := onchain.OpenSaleLease(v, sale, nil); ok && record.Kind == onchain.SaleLease {
				found = append(found, record.Lease.Inside)
			}
		}
		return len(found) == 1
	}, "the lease on relay-a")
	return found[0]
}

type acceptance struct {
	received int64
	event    *nostr.Event
}

// acceptances are the acceptance records of invoice a relay holds, as a checkout link's page opens
// them.
func (s *toleranceSetup) acceptances(invoice *checkout.Invoice, relay string) []acceptance {
	keys := derive.RestKeys(s.m.IntegrationKey, invoice.Ref, invoice.OnchainAddress, invoice.ExpiresAt)
	var out []acceptance
	for _, v := range s.m.Net.Get(relay).Events() {
		if received, e, newer, ok := onchain.OpenAcceptanceRecord(v, onchain.ReaderOf(keys), invoice.Sats); ok && !newer {
			out = append(out, acceptance{received, e})
		}
	}
	return out
}

func receivedOf(list []acceptance) []int64 {
	out := []int64{}
	for _, a := range list {
		out = append(out, a.received)
	}
	return out
}

func TestTheOrdersToleranceIsTheChannelsKeptInItsLeaseNeverInWhatTheShopGets(t *testing.T) {
	s := newToleranceSetup(t)
	s.publishChannel(tolerance, 100, false)
	invoice := s.createInvoice(t, "R-1")
	inside := s.leaseInside(t, "R-1")
	if fieldOf(inside, "sats") != float64(toleranceSats) || fieldOf(inside, "tolerance") != float64(tolerance) {
		t.Errorf("lease %s", jsonv.Marshal(inside))
	}
	// Neither the Invoice nor the checkout link's record (the customer reads it) holds it.
	data, err := json.Marshal(invoice)
	must(t, err)
	v, err := jsonv.Parse(data)
	must(t, err)
	names := v.(*jsonv.Object).Keys()
	sort.Strings(names)
	same(t, "fields", names, []string{"bolt11", "checkoutUrl", "createdAt", "expiresAt", "fiat", "note", "onchainAddress", "paymentHash",
		"paymentUri", "rate", "redirectUrl", "ref", "sats", "verifyUrl"})
	if strings.Contains(strings.ToLower(string(data)), "toleran") {
		t.Errorf("the invoice tells it: %s", data)
	}
	l := openLink(t, s.m, invoice.CheckoutURL)
	_, encryptionKey := derive.CheckoutKeys(l.fragment.LinkKey)
	plaintext, err := nip44.Decrypt(l.event.Content, encryptionKey)
	must(t, err)
	if strings.Contains(strings.ToLower(plaintext), "toleran") {
		t.Errorf("the link tells it: %s", plaintext)
	}
	if l.record == nil {
		t.Error("the link's record does not read")
	}
	if _, err := checkoutlink.ParseValue(l.value); err != nil {
		t.Error(err)
	}
}

func TestTheOrderHasNoToleranceWhenTheChannelsIsNoneOfOne(t *testing.T) {
	for name, value := range map[string]any{"absent": nil, "0": 0, "a fraction": 2.5, "a string": "500", "negative": -1, "above 10 %": 1001} {
		t.Run(name, func(t *testing.T) {
			s := newToleranceSetup(t)
			s.publishChannel(value, 100, name == "absent")
			s.createInvoice(t, "R-1")
			if inside := s.leaseInside(t, "R-1"); inside.Has("tolerance") {
				t.Errorf("lease %s", jsonv.Marshal(inside))
			}
		})
	}
}

func TestTheOrdersToleranceIs10PercentAtMost(t *testing.T) {
	s := newToleranceSetup(t)
	s.publishChannel(1000, 100, false)
	s.createInvoice(t, "R-1")
	if inside := s.leaseInside(t, "R-1"); fieldOf(inside, "tolerance") != float64(1000) {
		t.Errorf("lease %s", jsonv.Marshal(inside))
	}
}

// withinTolerance is the invoice of R-1, its channel's tolerance 5 %, a minute later.
func withinTolerance(t *testing.T) (*toleranceSetup, *checkout.Invoice) {
	t.Helper()
	s := newToleranceSetup(t)
	s.publishChannel(tolerance, 100, false)
	invoice := s.createInvoice(t, "R-1")
	s.m.SetNow(s.m.Now() + 60)
	return s, invoice
}

func (s *toleranceSetup) pay(sats int64, seed string, coins []string) *testkit.EsploraTx {
	tx := testkit.Payment(testkit.Txid(seed), restAddress, sats, coins, 0)
	s.m.Chain.Add(restAddress, tx)
	return tx
}

func TestAPaymentWithinTheTolerancePaysTheOrderWithoutARest(t *testing.T) {
	s, invoice := withinTolerance(t)
	tx := s.pay(leastAccepted, "least", nil)
	same(t, "unconfirmed", statusJSON(t, s.c, invoice), jsonv.O("state", "unconfirmed", "received", itoa(leastAccepted), "confirmations", 0, "txids", []string{tx.Txid}))
	if n := len(madeOf(s.m, toleranceSats-leastAccepted)); n != 0 {
		t.Errorf("%d invoices of the rest", n)
	}
	s.m.Chain.Confirm(restAddress, tx.Txid, s.m.Chain.Height()-2)
	s.m.SetNow(invoice.ExpiresAt + 3600)
	paid := jsonv.O("state", "paid", "method", "onchain", "received", itoa(leastAccepted), "txids", []string{tx.Txid})
	same(t, "paid", statusJSON(t, s.c, invoice), paid)
	// The order is paid: no new invoice for it.
	_, err := s.c.CreateInvoice(ctx, checkout.Order{Ref: "R-1", Sats: toleranceSats})
	e := wantCode(t, err, checkout.CodeOrderPaid)
	same(t, "status", jsonOf(t, e.Status), paid)
	if e.Preimage != "" {
		t.Errorf("preimage %q", e.Preimage)
	}
}

func TestAPaymentShortByMoreThanTheToleranceAsksItsWholeRest(t *testing.T) {
	s, invoice := withinTolerance(t)
	s.pay(leastAccepted-1, "short", nil)
	status := statusJSON(t, s.c, invoice)
	made := madeOf(s.m, toleranceSats-leastAccepted+1)
	if len(made) != 1 {
		t.Fatalf("%d invoices of the rest", len(made))
	}
	same(t, "status", status, jsonv.O(
		"state", "open",
		"expiresAt", invoice.ExpiresAt,
		"received", itoa(leastAccepted-1),
		"rest", jsonv.O("sats", "2451", "bolt11", made[0].PR, "paymentUri", "bitcoin:"+restAddress+"?amount=0.00002451&lightning="+made[0].PR),
	))
	time.Sleep(100 * time.Millisecond)
	if a := s.acceptances(invoice, s.m.KeyRelays[0]); len(a) != 0 {
		t.Errorf("%d acceptance records", len(a))
	}
}

func TestTheToleranceIsReachedByASecondPaymentAfterAnUnderpayment(t *testing.T) {
	s, invoice := withinTolerance(t)
	s.pay(44_100, "first", nil)
	status := statusJSON(t, s.c, invoice)
	if fieldOf(status, "state") != "open" || fieldOf(status, "received") != "44100" || fieldOf(fieldOf(status, "rest"), "sats") != "4900" {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	s.pay(2450, "second", nil)
	if status := statusJSON(t, s.c, invoice); fieldOf(status, "state") != "unconfirmed" || fieldOf(status, "received") != itoa(leastAccepted) {
		t.Errorf("%s", jsonv.Marshal(status))
	}
}

func TestWithAnInvoiceOfTheRestPaidOnlyWhatTheToleranceLeavesIsDueOnchain(t *testing.T) {
	s, invoice := withinTolerance(t)
	tx := s.pay(20_000, "part", nil)
	statusJSON(t, s.c, invoice)
	made := madeOf(s.m, 29_000)
	if len(made) != 1 {
		t.Fatalf("%d invoices of the rest", len(made))
	}
	s.m.PayInvoice(made[0].PaymentHash)
	s.m.Chain.Confirm(restAddress, tx.Txid, s.m.Chain.Height()-2)
	same(t, "paid", statusJSON(t, s.c, invoice), jsonv.O("state", "paid", "method", "mixed", "received", "49000", "txids", []string{tx.Txid}, "preimages", []string{made[0].Preimage}))
}

func TestWaitForPaymentWaitsForAPaymentWithinTheToleranceUntilPaid(t *testing.T) {
	s, invoice := withinTolerance(t)
	s.m.Net.AddExplorerSocket()
	tx := s.pay(leastAccepted, "least", nil)
	s.m.Chain.Confirm(restAddress, tx.Txid, s.m.Chain.Height()-2)
	done := make(chan waited, 1)
	go func() {
		status, err := s.c.WaitForPayment(ctx, invoice, checkout.WithInterval(250*time.Millisecond))
		done <- waited{status, err}
	}()
	w := outcomeOf(t, done)
	must(t, w.err)
	same(t, "paid", jsonOf(t, w.status), jsonv.O("state", "paid", "method", "onchain", "received", itoa(leastAccepted), "txids", []string{tx.Txid}))
}

func TestTheToleranceOfTheOrdersCreationStillJudgesItsPaymentWhenTheChannelsIsLowered(t *testing.T) {
	s := newToleranceSetup(t)
	s.publishChannel(tolerance, 100, false)
	invoice := s.createInvoice(t, "R-1")
	s.publishChannel(0, 200, false)
	s.m.SetNow(s.m.Now() + 60)
	s.pay(leastAccepted, "least", nil)
	if status := statusJSON(t, s.c, invoice); fieldOf(status, "state") != "unconfirmed" {
		t.Errorf("%s", jsonv.Marshal(status))
	}
}

func TestAToleranceSetAfterTheOrdersCreationNeverJudgesItsPayment(t *testing.T) {
	s := newToleranceSetup(t)
	s.publishChannel(nil, 100, true)
	invoice := s.createInvoice(t, "R-1")
	s.publishChannel(tolerance, 200, false)
	s.m.SetNow(s.m.Now() + 60)
	s.pay(leastAccepted, "least", nil)
	status := statusJSON(t, s.c, invoice)
	if fieldOf(status, "state") != "open" || fieldOf(status, "received") != itoa(leastAccepted) || fieldOf(fieldOf(status, "rest"), "sats") != "2450" {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	// Past its end, for good: never paid by a tolerance set afterwards.
	s.m.SetNow(invoice.ExpiresAt + 3600)
	same(t, "expired", statusJSON(t, s.c, invoice), jsonv.O("state", "expired", "received", itoa(leastAccepted)))
}

func TestTheAcceptanceRecordTellsTheLinksPageWhatWasReceivedOnEveryRelayOnce(t *testing.T) {
	s, invoice := withinTolerance(t)
	s.pay(47_000, "least", nil)
	statusJSON(t, s.c, invoice)
	for _, relay := range s.m.KeyRelays {
		testkit.Eventually(t, func() bool { return len(s.acceptances(invoice, relay)) == 1 }, relay+" holds the acceptance record")
		same(t, relay, receivedOf(s.acceptances(invoice, relay)), []int64{47_000})
	}
	// Its address, from the invoice's rest key, and its life, the links'.
	keys := derive.RestKeys(s.m.IntegrationKey, "R-1", restAddress, invoice.ExpiresAt)
	e := s.acceptances(invoice, s.m.KeyRelays[0])[0].event
	if e.PubKey != keys.Author {
		t.Error("not the rest author")
	}
	same(t, "tags", e.Tags, [][]string{{"d", derive.AcceptanceRecordAddress(keys.Key)}, {"expiration", itoa(invoice.ExpiresAt + 604_800)}})
	// What was received, never the tolerance.
	plaintext, err := nip44.Decrypt(e.Content, keys.EncryptionKey)
	must(t, err)
	record, err := jsonv.ParseString(plaintext)
	must(t, err)
	same(t, "record", record, jsonv.O("t", "checkout-accepted", "v", jsonv.O("version", 1, "received", 47_000)))
	first := s.m.Net.Get(s.m.KeyRelays[0])
	sent := len(first.Received())
	statusJSON(t, s.c, invoice)
	// Known on a majority: not published again.
	time.Sleep(100 * time.Millisecond)
	if n := len(first.Received()); n != sent {
		t.Errorf("%d events more", n-sent)
	}
}

func TestTheAcceptanceRecordIsPublishedAgainOfLessWhenAReplacementLowersWhatWasReceived(t *testing.T) {
	s, invoice := withinTolerance(t)
	first := s.pay(48_000, "first", nil)
	statusJSON(t, s.c, invoice)
	s.m.SetNow(s.m.Now() + 1)
	s.m.Chain.Drop(restAddress, first.Txid)
	// A fee bump of the same coin, paying less.
	s.pay(47_000, "bump", coinsOf(first))
	if status := statusJSON(t, s.c, invoice); fieldOf(status, "state") != "unconfirmed" || fieldOf(status, "received") != "47000" {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	testkit.Eventually(t, func() bool {
		list := s.acceptances(invoice, s.m.KeyRelays[0])
		sort.Slice(list, func(i, j int) bool { return list[i].event.CreatedAt > list[j].event.CreatedAt })
		return len(list) > 0 && list[0].received == 47_000
	}, "the acceptance record of 47 000 the newest")
}

func TestTheAcceptanceRecordIsNeverPublishedForAPaymentOfTheWholeAmount(t *testing.T) {
	s, invoice := withinTolerance(t)
	s.pay(toleranceSats, "whole", nil)
	if status := statusJSON(t, s.c, invoice); fieldOf(status, "state") != "unconfirmed" {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	time.Sleep(100 * time.Millisecond)
	if a := s.acceptances(invoice, s.m.KeyRelays[0]); len(a) != 0 {
		t.Errorf("%d acceptance records", len(a))
	}
}

func TestAFailedPublicationOfTheAcceptanceRecordIsLoggedAndPublishedAtTheNextCheck(t *testing.T) {
	s, invoice := withinTolerance(t)
	keys := derive.RestKeys(s.m.IntegrationKey, "R-1", restAddress, invoice.ExpiresAt)
	d := derive.AcceptanceRecordAddress(keys.Key)
	refuse := testkit.Behaviour{RefuseEvent: func(e *nostr.Event) string {
		if tag, _ := e.DTag(); tag == d {
			return "blocked: no"
		}
		return ""
	}}
	for _, url := range s.m.KeyRelays {
		s.m.Net.Get(url).SetBehaviour(refuse)
	}
	s.pay(leastAccepted, "least", nil)
	if status := statusJSON(t, s.c, invoice); fieldOf(status, "state") != "unconfirmed" || fieldOf(status, "received") != itoa(leastAccepted) {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	msgs := s.log.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "the acceptance record of a payment was not published") {
		t.Fatalf("warnings %q", msgs)
	}
	if strings.Contains(strings.ToLower(msgs[0]), "toleran") || strings.Contains(msgs[0], "500") {
		t.Errorf("the warning tells the tolerance: %q", msgs[0])
	}
	if a := s.acceptances(invoice, s.m.KeyRelays[0]); len(a) != 0 {
		t.Errorf("%d acceptance records", len(a))
	}
	accept(s.m, s.m.KeyRelays...)
	statusJSON(t, s.c, invoice)
	testkit.Eventually(t, func() bool { return len(s.acceptances(invoice, s.m.KeyRelays[0])) == 1 }, "the acceptance record")
	same(t, "received", receivedOf(s.acceptances(invoice, s.m.KeyRelays[0])), []int64{leastAccepted})
}

// readsAt makes a relay that never answers a read of the records at the address d (a d tag of the
// read's filter), while muting holds: it takes every write and answers every other read.
func readsAt(d string, muting *atomic.Bool) func(filter *jsonv.Object) bool {
	return func(filter *jsonv.Object) bool {
		tags, _ := filter.Get("#d")
		list, _ := tags.([]any)
		for _, tag := range list {
			if tag == d {
				return muting.Load()
			}
		}
		return false
	}
}

// SPEC.md §13, §31: a read of the acceptance records that fewer than a majority of the relays
// answered decides nothing. Two relays of three never answer it: nothing is published (what they
// hold cannot be told), a warning is logged, never the check's error; once they answer, the next
// check publishes the record.
func TestNoAcceptanceRecordIsPublishedAfterAReadFewerThanAMajorityAnswered(t *testing.T) {
	s, invoice := withinTolerance(t)
	keys := derive.RestKeys(s.m.IntegrationKey, "R-1", restAddress, invoice.ExpiresAt)
	var muting atomic.Bool
	muting.Store(true)
	for _, url := range s.m.KeyRelays[1:] {
		s.m.Net.Get(url).SetBehaviour(testkit.Behaviour{MuteQuery: readsAt(derive.AcceptanceRecordAddress(keys.Key), &muting)})
	}
	s.pay(leastAccepted, "least", nil)
	sent := map[string]int{}
	for _, url := range s.m.KeyRelays {
		sent[url] = len(s.m.Net.Get(url).Received())
	}
	if status := statusJSON(t, s.c, invoice); fieldOf(status, "state") != "unconfirmed" || fieldOf(status, "received") != itoa(leastAccepted) {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	time.Sleep(100 * time.Millisecond)
	for _, url := range s.m.KeyRelays {
		for _, e := range s.m.Net.Get(url).Received()[sent[url]:] {
			if e.PubKey == keys.Author {
				t.Errorf("%s: an acceptance record published after a read that one relay of three answered", url)
			}
		}
	}
	msgs := s.log.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "the acceptance record of a payment was not published") {
		t.Fatalf("warnings %q", msgs)
	}
	muting.Store(false)
	statusJSON(t, s.c, invoice)
	for _, url := range s.m.KeyRelays {
		testkit.Eventually(t, func() bool { return len(s.acceptances(invoice, url)) == 1 }, url+" holds the acceptance record")
		same(t, url, receivedOf(s.acceptances(invoice, url)), []int64{leastAccepted})
	}
}

// SPEC.md §12, §13: what relays that did not answer the read hold is never written over. Two
// relays of three hold an acceptance record of a newer format and never answer the read: nothing
// is published over it (the relays keep the newest record of an address); once they answer, the
// record of the newer format is seen, and still nothing is published.
func TestAnAcceptanceRecordOfANewerFormatOnRelaysThatDidNotAnswerIsNeverWrittenOver(t *testing.T) {
	s, invoice := withinTolerance(t)
	keys := derive.RestKeys(s.m.IntegrationKey, "R-1", restAddress, invoice.ExpiresAt)
	d := derive.AcceptanceRecordAddress(keys.Key)
	newer, err := nostr.Seal(nostr.SealOptions{
		Type:          proto.AcceptanceRecordType,
		Value:         jsonv.O("version", 2),
		Signer:        keys.SecretKey,
		EncryptionKey: keys.EncryptionKey,
		Address:       d,
		CreatedAt:     s.m.Now() - 1,
		Tags:          [][]string{{"expiration", itoa(invoice.ExpiresAt + 604_800)}},
		Random:        testkit.Random("newer acceptance record"),
	})
	must(t, err)
	var muting atomic.Bool
	muting.Store(true)
	for _, url := range s.m.KeyRelays[1:] {
		s.m.Net.Get(url).Take(newer)
		s.m.Net.Get(url).SetBehaviour(testkit.Behaviour{MuteQuery: readsAt(d, &muting)})
	}
	s.pay(leastAccepted, "least", nil)
	sent := map[string]int{}
	for _, url := range s.m.KeyRelays {
		sent[url] = len(s.m.Net.Get(url).Received())
	}
	for check := 1; check <= 2; check++ {
		if status := statusJSON(t, s.c, invoice); fieldOf(status, "state") != "unconfirmed" {
			t.Fatalf("check %d: %s", check, jsonv.Marshal(status))
		}
		time.Sleep(100 * time.Millisecond)
		for _, url := range s.m.KeyRelays {
			for _, e := range s.m.Net.Get(url).Received()[sent[url]:] {
				if e.PubKey == keys.Author {
					t.Errorf("check %d: %s: an acceptance record published", check, url)
				}
			}
			if url != s.m.KeyRelays[0] && !contains(eventIDs(s.m.Net.Get(url).Events()), newer.ID) {
				t.Errorf("check %d: %s: the record of the newer format replaced", check, url)
			}
		}
		muting.Store(false)
	}
	if !s.log.Has("the acceptance record of a payment was not published") || !s.log.Has("an acceptance record of this order has a newer format") {
		t.Errorf("warnings %q", s.log.Messages())
	}
}

// SPEC.md §12: an acceptance record of a newer format is never written over, and the package says
// it should be updated.
func TestTheAcceptanceRecordIsNeverPublishedOverOneOfANewerFormat(t *testing.T) {
	s, invoice := withinTolerance(t)
	keys := derive.RestKeys(s.m.IntegrationKey, "R-1", restAddress, invoice.ExpiresAt)
	newer, err := nostr.Seal(nostr.SealOptions{
		Type:          proto.AcceptanceRecordType,
		Value:         jsonv.O("version", 2),
		Signer:        keys.SecretKey,
		EncryptionKey: keys.EncryptionKey,
		Address:       derive.AcceptanceRecordAddress(keys.Key),
		CreatedAt:     s.m.Now() - 1,
		Tags:          [][]string{{"expiration", itoa(invoice.ExpiresAt + 604_800)}},
		Random:        testkit.Random("newer acceptance record"),
	})
	must(t, err)
	s.m.Net.Get(s.m.KeyRelays[0]).Take(newer)
	// The third relay never answers the read of the acceptance records: the read is the first
	// two's, as node's relays answer in their order (it would not see a record the third alone
	// holds).
	s.m.Net.Get(s.m.KeyRelays[2]).SetBehaviour(testkit.Behaviour{MuteQuery: readsOf(keys.Author)})
	s.pay(47_000, "least", nil)
	sent := map[string]int{}
	for _, url := range s.m.KeyRelays {
		sent[url] = len(s.m.Net.Get(url).Received())
	}
	if status := statusJSON(t, s.c, invoice); fieldOf(status, "state") != "unconfirmed" {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	time.Sleep(100 * time.Millisecond)
	for _, url := range s.m.KeyRelays {
		for _, e := range s.m.Net.Get(url).Received()[sent[url]:] {
			if e.PubKey == keys.Author {
				t.Errorf("%s: an acceptance record published", url)
			}
		}
	}
	if !s.log.Has("an acceptance record of this order has a newer format") {
		t.Errorf("warnings %q", s.log.Messages())
	}
}
