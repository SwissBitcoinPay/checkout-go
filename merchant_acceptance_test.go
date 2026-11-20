package checkout_test

import (
	"crypto/rand"
	"strings"
	"sync"
	"testing"
	"time"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/onchain"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/testkit"
)

// The merchant's acceptance (SPEC.md §31, "The merchant's acceptance"), end to end against local
// relays and fakes of breez.tips and of the explorers: the team's record, read on every relay with
// the witnesses, pays an order underpaid on-chain down to what the merchant accepted, only when a
// majority of the merchant's relays hold it, whichever majority answers; the pages of the order's
// checkout links told by an acceptance record (the server's); the check publishing nothing of it.

const (
	// acceptedSats is the order's amount; acceptedCame what came on-chain, and what the merchant
	// accepts.
	acceptedSats = 49_000
	acceptedCame = 30_000
)

// acceptanceSetup is a merchant whose channel names its relays and a web app for checkout links
// (with the tolerance given), its lot of one address, a client whose warnings go to log, the
// invoice of the order R-1 (49 000 sats for 600 s, with a checkout link) leased the address, and,
// a minute later, a transaction paying it what came.
type acceptanceSetup struct {
	m       *testkit.Merchant
	c       *checkout.Client
	log     *testkit.Logger
	invoice *checkout.Invoice
	tx      *testkit.EsploraTx
	came    int64
}

// newAcceptanceSetup is the setup with no tolerance and 30 000 sats come.
func newAcceptanceSetup(t *testing.T) *acceptanceSetup {
	return acceptanceSetupWith(t, nil, acceptedCame)
}

// acceptanceSetupWith is the setup with the channel's tolerance (nil: none) and came sats come.
func acceptanceSetupWith(t *testing.T, tolerance any, came int64) *acceptanceSetup {
	t.Helper()
	m := testkit.NewMerchant()
	m.Start(nil, nil)
	t.Cleanup(m.Stop)
	m.PublishLot([]string{restAddress}, 90)
	members := []jsonv.Member{{Name: "relays", Value: m.KeyRelays}, {Name: "checkout", Value: jsonv.O("origin", origin)}}
	if tolerance != nil {
		members = append(members, jsonv.Member{Name: "onchainTolerance", Value: tolerance})
	}
	m.PublishChannel(m.ChannelValue(100, members...), 100)
	s := &acceptanceSetup{m: m, log: &testkit.Logger{}, came: came}
	s.c = clientOf(t, m, checkout.WithLogger(s.log))
	invoice, err := s.c.CreateInvoice(ctx, checkout.Order{Ref: "R-1", Sats: acceptedSats, DurationSeconds: 600, Note: "Order 1", Checkout: true})
	must(t, err)
	if invoice.OnchainAddress != restAddress {
		t.Fatalf("leased %q", invoice.OnchainAddress)
	}
	s.invoice = invoice
	m.SetNow(m.Now() + 60)
	s.tx = testkit.Payment(testkit.Txid("came"), restAddress, came, nil, 0)
	m.Chain.Add(restAddress, s.tx)
	return s
}

// acceptanceOf is what a record changes of the invoice it is the merchant's acceptance of: its
// order, its address, its expiresAt ("" and 0: the invoice's own).
type acceptanceOf struct {
	ref       string
	address   string
	expiresAt int64
}

// addressOf is where the merchant's acceptance of the invoice is (or of what of changes of it).
func (s *acceptanceSetup) addressOf(of acceptanceOf) string {
	ref, address, expiresAt := s.invoice.Ref, s.invoice.OnchainAddress, s.invoice.ExpiresAt
	if of.ref != "" {
		ref = of.ref
	}
	if of.address != "" {
		address = of.address
	}
	if of.expiresAt != 0 {
		expiresAt = of.expiresAt
	}
	return derive.MerchantAcceptanceAddress(s.m.IntegrationKey, s.m.Sale(ref).Author, address, expiresAt)
}

// acceptanceOptions are what a test changes of a merchant's acceptance: its date (0: a minute ago),
// its signer (nil: the team), its value (nil: version 1 of received), what it is of.
type acceptanceOptions struct {
	createdAt int64
	signer    []byte
	value     any
	of        acceptanceOf
}

// acceptance is the merchant's acceptance of the invoice at received, as the merchant's app makes
// it: sealed with the channel's key, signed by the team, at its address, dated a minute ago.
func (s *acceptanceSetup) acceptance(t *testing.T, received int64, o acceptanceOptions) *nostr.Event {
	t.Helper()
	if o.createdAt == 0 {
		o.createdAt = s.m.Now() - 60
	}
	if o.signer == nil {
		o.signer = s.m.TeamSecret
	}
	if o.value == nil {
		o.value = jsonv.O("version", 1, "received", received)
	}
	_, encryptionKey := derive.ChannelKeys(s.m.IntegrationKey)
	e, err := nostr.Seal(nostr.SealOptions{
		Type:          proto.MerchantAcceptanceRecordType,
		Value:         o.value,
		Signer:        o.signer,
		EncryptionKey: encryptionKey,
		Address:       s.addressOf(o.of),
		CreatedAt:     o.createdAt,
		Random:        rand.Reader,
	})
	must(t, err)
	return e
}

// hold has the relays at indexes of the key's (every one when none is given) take e, as a relay
// keeps it.
func (s *acceptanceSetup) hold(e *nostr.Event, indexes ...int) {
	if len(indexes) == 0 {
		indexes = []int{0, 1, 2}
	}
	for _, i := range indexes {
		s.m.Net.Get(s.m.KeyRelays[i]).Take(e)
	}
}

// muteRead has the relays at indexes never answer the read of the invoice's merchant's acceptance
// (every other read answered), and the others answer every read.
func (s *acceptanceSetup) muteRead(indexes ...int) {
	address := s.addressOf(acceptanceOf{})
	for i, url := range s.m.KeyRelays {
		b := testkit.Behaviour{}
		for _, muted := range indexes {
			if muted == i {
				b.MuteQuery = func(filter *jsonv.Object) bool {
					tags, _ := filter.Get("#d")
					list, _ := tags.([]any)
					for _, tag := range list {
						if tag == address {
							return true
						}
					}
					return false
				}
			}
		}
		s.m.Net.Get(url).SetBehaviour(b)
	}
}

// acceptanceRecords are what the acceptance records of the invoice a relay holds received, as a
// checkout link's page opens them.
func (s *acceptanceSetup) acceptanceRecords(relay string) []int64 {
	keys := derive.RestKeys(s.m.IntegrationKey, s.invoice.Ref, s.invoice.OnchainAddress, s.invoice.ExpiresAt)
	out := []int64{}
	for _, v := range s.m.Net.Get(relay).Events() {
		if received, _, newer, ok := onchain.OpenAcceptanceRecord(v, onchain.ReaderOf(keys), s.invoice.Sats); ok && !newer {
			out = append(out, received)
		}
	}
	return out
}

// sentOfTheTeam are the events the check sent the relays of the team's, or at the merchant's
// acceptance's address.
func (s *acceptanceSetup) sentOfTheTeam() []*nostr.Event {
	address := s.addressOf(acceptanceOf{})
	var out []*nostr.Event
	for _, url := range s.m.KeyRelays {
		for _, e := range s.m.Net.Get(url).Received() {
			if d, _ := e.DTag(); e.PubKey == s.m.Team || d == address {
				out = append(out, e)
			}
		}
	}
	return out
}

func (s *acceptanceSetup) status(t *testing.T) any {
	t.Helper()
	return statusJSON(t, s.c, s.invoice)
}

// underpaid is the status of the order with 30 000 sats in time, its rest of 19 000 asked.
func (s *acceptanceSetup) underpaid(t *testing.T) any {
	t.Helper()
	made := madeOf(s.m, acceptedSats-acceptedCame)
	if len(made) != 1 {
		t.Fatalf("%d invoices of the rest", len(made))
	}
	return jsonv.O("state", "open", "expiresAt", s.invoice.ExpiresAt, "received", itoa(acceptedCame),
		"rest", jsonv.O("sats", "19000", "bolt11", made[0].PR, "paymentUri", "bitcoin:"+restAddress+"?amount=0.00019&lightning="+made[0].PR))
}

// unconfirmed is the status of the order paid down to what came, being confirmed.
func (s *acceptanceSetup) unconfirmed() any {
	return jsonv.O("state", "unconfirmed", "received", itoa(s.came), "confirmations", 0, "txids", []string{s.tx.Txid})
}

// warnedOfANewerFormat is how many warnings told of a merchant's acceptance of a newer format.
func (s *acceptanceSetup) warnedOfANewerFormat() int {
	n := 0
	for _, message := range s.log.Messages() {
		if strings.Contains(message, "a merchant's acceptance of this order has a newer format") {
			n++
		}
	}
	return n
}

// A merchant's acceptance a majority of the relays hold.

func TestAMerchantsAcceptanceAMajorityHoldsPaysTheOrderDownToIt(t *testing.T) {
	s := newAcceptanceSetup(t)
	s.hold(s.acceptance(t, acceptedCame, acceptanceOptions{}), 0, 1)
	same(t, "unconfirmed", s.status(t), s.unconfirmed())
	// No rest asked: the order is paid.
	if n := len(madeOf(s.m, acceptedSats-acceptedCame)); n != 0 {
		t.Errorf("%d invoices of the rest", n)
	}
	// The acceptance record (the server's, for the pages of the order's checkout links), on every
	// relay: what was received, nothing of the merchant's acceptance itself.
	for _, relay := range s.m.KeyRelays {
		testkit.Eventually(t, func() bool { return len(s.acceptanceRecords(relay)) == 1 }, relay+" holds the acceptance record")
		same(t, relay, s.acceptanceRecords(relay), []int64{acceptedCame})
	}
	// The merchant's acceptance never published again, nor anything of the team's.
	if sent := s.sentOfTheTeam(); len(sent) != 0 {
		t.Errorf("%d events of the team's sent", len(sent))
	}
	for _, v := range s.m.Net.Get(s.m.KeyRelays[2]).Events() {
		if pubkey, _ := jsonv.Field(v, "pubkey"); pubkey == s.m.Team && nostr.DTagOf(v) == s.addressOf(acceptanceOf{}) {
			t.Error("the third relay holds the merchant's acceptance")
		}
	}

	s.m.Chain.Confirm(restAddress, s.tx.Txid, s.m.Chain.Height()-2)
	s.m.SetNow(s.invoice.ExpiresAt + 3600)
	paid := jsonv.O("state", "paid", "method", "onchain", "received", itoa(acceptedCame), "txids", []string{s.tx.Txid})
	same(t, "paid", s.status(t), paid)
	_, err := s.c.CreateInvoice(ctx, checkout.Order{Ref: "R-1", Sats: acceptedSats})
	e := wantCode(t, err, checkout.CodeOrderPaid)
	if e.Status == nil {
		t.Fatal("no status")
	}
	same(t, "the order's status", jsonOf(t, e.Status), paid)
	if msgs := s.log.Messages(); len(msgs) != 0 {
		t.Errorf("warnings %q", msgs)
	}
}

func TestAMerchantsAcceptanceAMajorityHoldsCountsWhicheverMajorityAnswers(t *testing.T) {
	for _, c := range []struct {
		name             string
		holders, failing []int
	}{
		{"the third failing", []int{0, 1}, []int{2}},
		{"the first failing, the 2 others holding it", []int{1, 2}, []int{0}},
		{"every relay answering", []int{0, 1}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newAcceptanceSetup(t)
			s.hold(s.acceptance(t, acceptedCame, acceptanceOptions{}), c.holders...)
			s.muteRead(c.failing...)
			same(t, "unconfirmed", s.status(t), s.unconfirmed())
		})
	}
}

func TestAMerchantsAcceptanceIsWaitedForFromARelayThatHoldsItAndAnswersLate(t *testing.T) {
	s := newAcceptanceSetup(t)
	s.hold(s.acceptance(t, acceptedCame, acceptanceOptions{}), 0, 1)
	s.m.Net.Get(s.m.KeyRelays[0]).SetBehaviour(testkit.Behaviour{QueryDelay: 400 * time.Millisecond})
	same(t, "unconfirmed", s.status(t), s.unconfirmed())
}

func TestAMerchantsAcceptanceMadeBeforeTheEndCountsAfterTheLeasesEnd(t *testing.T) {
	s := newAcceptanceSetup(t)
	same(t, "underpaid", s.status(t), s.underpaid(t))
	s.hold(s.acceptance(t, acceptedCame, acceptanceOptions{createdAt: s.invoice.ExpiresAt - 60}), 1, 2)
	// Never expired: it counts once a majority holds it, whenever the check.
	s.m.SetNow(s.invoice.ExpiresAt + 3600)
	same(t, "unconfirmed", s.status(t), s.unconfirmed())
}

func TestAMerchantsAcceptanceOfLessThanWhatCameLetsWhatCamePay(t *testing.T) {
	s := newAcceptanceSetup(t)
	s.hold(s.acceptance(t, 20_000, acceptanceOptions{}))
	same(t, "unconfirmed", s.status(t), s.unconfirmed())
	testkit.Eventually(t, func() bool { return len(s.acceptanceRecords(s.m.KeyRelays[0])) == 1 }, "the acceptance record")
	same(t, "acceptance records", s.acceptanceRecords(s.m.KeyRelays[0]), []int64{acceptedCame})
}

func TestAMerchantsAcceptanceOfMoreThanWhatCameAsksTheWholeRest(t *testing.T) {
	s := newAcceptanceSetup(t)
	s.hold(s.acceptance(t, 35_000, acceptanceOptions{}))
	same(t, "underpaid", s.status(t), s.underpaid(t))
	time.Sleep(100 * time.Millisecond)
	same(t, "acceptance records", s.acceptanceRecords(s.m.KeyRelays[0]), []int64{})
}

// A merchant's acceptance only a minority of the relays holds.

func TestAMerchantsAcceptanceAMinorityHoldsNeverCountsWhicheverRelaysAnswer(t *testing.T) {
	s := newAcceptanceSetup(t)
	s.hold(s.acceptance(t, acceptedCame, acceptanceOptions{}), 0)
	same(t, "underpaid", s.status(t), s.underpaid(t))
	// The relay that holds it and one that does not answer: still a minority's.
	s.muteRead(2)
	same(t, "underpaid with a relay failing", s.status(t), s.underpaid(t))
	s.muteRead()
	s.m.SetNow(s.invoice.ExpiresAt + 60)
	if status := s.status(t); fieldOf(status, "state") != "open" || fieldOf(status, "rest") != nil {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	s.m.SetNow(s.invoice.ExpiresAt + 3600)
	same(t, "expired", s.status(t), jsonv.O("state", "expired", "received", itoa(acceptedCame)))
	same(t, "acceptance records", s.acceptanceRecords(s.m.KeyRelays[0]), []int64{})
}

// SPEC.md §27: a relay that holds it and fails at a check's time, the other relays that hold it
// fewer than a majority, makes that check not count it; a later one, which that relay answers,
// does.
func TestAMerchantsAcceptanceIsNotCountedByACheckAHolderFailsWhenTheOtherHoldersAreAMinority(t *testing.T) {
	s := newAcceptanceSetup(t)
	s.hold(s.acceptance(t, acceptedCame, acceptanceOptions{}), 0, 1)
	s.muteRead(0)
	same(t, "underpaid", s.status(t), s.underpaid(t))
	s.muteRead()
	same(t, "unconfirmed", s.status(t), s.unconfirmed())
}

func TestAMerchantsAcceptanceCountsARelayOnceWhateverCopiesOfItItSends(t *testing.T) {
	s := newAcceptanceSetup(t)
	e := s.acceptance(t, acceptedCame, acceptanceOptions{})
	// The same event under another signature (BIP-340: any of them verifies): the same record.
	resigned, err := nostr.Sign(nostr.Template{CreatedAt: e.CreatedAt, Kind: proto.RecordKind, Tags: e.Tags, Content: e.Content}, s.m.TeamSecret, rand.Reader)
	must(t, err)
	if resigned.ID != e.ID || resigned.Sig == e.Sig {
		t.Fatal("not another signature of the same event")
	}
	first := s.m.Net.Get(s.m.KeyRelays[0])
	first.StoreEvent(e)
	first.StoreEvent(e)
	first.StoreEvent(resigned)
	same(t, "underpaid", s.status(t), s.underpaid(t))
	// The count itself, of what the relays sent (the pool already reads a relay's copies of one id
	// once): one relay, whatever copies it sent; then a second one, a majority.
	keys := onchain.MerchantAcceptanceKeysOf(s.m.IntegrationKey, s.m.Team, s.m.Sale("R-1").Author, restAddress, s.invoice.ExpiresAt)
	terms := onchain.AcceptedInvoice{Sats: acceptedSats, ExpiresAt: s.invoice.ExpiresAt}
	mixed := *e
	mixed.Sig = resigned.Sig
	value := testkit.EventValue
	answers := [][]any{{value(e), value(e), value(resigned), value(&mixed)}, {}, {}}
	if got := onchain.MerchantAcceptanceOf(answers, keys, terms, 3, nostr.NewSigMemo()); got != (onchain.MerchantAcceptance{}) {
		t.Errorf("one relay: %+v", got)
	}
	answers[1] = []any{value(resigned)}
	if got := onchain.MerchantAcceptanceOf(answers, keys, terms, 3, nostr.NewSigMemo()); got != (onchain.MerchantAcceptance{Received: acceptedCame}) {
		t.Errorf("two relays: %+v", got)
	}
}

// A record that is not this invoice's merchant's acceptance.

func TestARecordThatIsNotTheInvoicesMerchantsAcceptanceIsNotCounted(t *testing.T) {
	for _, c := range []struct {
		name    string
		options func(s *acceptanceSetup) acceptanceOptions
	}{
		{"of another invoice of the order (another expiresAt)", func(s *acceptanceSetup) acceptanceOptions {
			return acceptanceOptions{of: acceptanceOf{expiresAt: s.invoice.ExpiresAt + 3600}}
		}},
		{"of another order", func(*acceptanceSetup) acceptanceOptions { return acceptanceOptions{of: acceptanceOf{ref: "R-2"}} }},
		{"of another address", func(*acceptanceSetup) acceptanceOptions { return acceptanceOptions{of: acceptanceOf{address: lot[1]}} }},
		{"of another team", func(*acceptanceSetup) acceptanceOptions { return acceptanceOptions{signer: testkit.Bytes(9, 32)} }},
		{"of the whole amount", func(*acceptanceSetup) acceptanceOptions {
			return acceptanceOptions{value: jsonv.O("version", 1, "received", acceptedSats)}
		}},
		{"of 0 sats", func(*acceptanceSetup) acceptanceOptions {
			return acceptanceOptions{value: jsonv.O("version", 1, "received", 0)}
		}},
		{"without a version", func(*acceptanceSetup) acceptanceOptions {
			return acceptanceOptions{value: jsonv.O("received", acceptedCame)}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newAcceptanceSetup(t)
			s.hold(s.acceptance(t, acceptedCame, c.options(s)))
			same(t, "underpaid", s.status(t), s.underpaid(t))
			if msgs := s.log.Messages(); len(msgs) != 0 {
				t.Errorf("warnings %q", msgs)
			}
		})
	}
}

func TestAMerchantsAcceptanceWithADamagedSignatureIsNotCounted(t *testing.T) {
	s := newAcceptanceSetup(t)
	damaged := *s.acceptance(t, acceptedCame, acceptanceOptions{})
	last := "0"
	if strings.HasSuffix(damaged.Sig, "0") {
		last = "1"
	}
	damaged.Sig = damaged.Sig[:len(damaged.Sig)-1] + last
	for _, url := range s.m.KeyRelays {
		s.m.Net.Get(url).StoreEvent(&damaged)
	}
	same(t, "underpaid", s.status(t), s.underpaid(t))
}

// Dated after now: its created_at is never compared with now, only with the lease's end.
func TestAMerchantsAcceptanceDatedAtTheLeasesEndCountsASecondLaterNot(t *testing.T) {
	for _, c := range []struct {
		name    string
		after   int64
		counted bool
	}{
		{"at the lease's end: counted", 120, true},
		{"a second after the lease's end: not counted", 121, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newAcceptanceSetup(t)
			s.hold(s.acceptance(t, acceptedCame, acceptanceOptions{createdAt: s.invoice.ExpiresAt + c.after}))
			status := s.status(t)
			if c.counted {
				same(t, "unconfirmed", status, s.unconfirmed())
			} else {
				same(t, "underpaid", status, s.underpaid(t))
			}
		})
	}
}

// A merchant's acceptance of a newer format.

// newerWith has every relay hold others and a record of version 2 dated createdAt (relays send
// both).
func (s *acceptanceSetup) newerWith(t *testing.T, createdAt int64, others ...*nostr.Event) {
	t.Helper()
	newer := s.acceptance(t, acceptedCame, acceptanceOptions{createdAt: createdAt, value: jsonv.O("version", 2)})
	for _, url := range s.m.KeyRelays {
		for _, e := range append(append([]*nostr.Event(nil), others...), newer) {
			s.m.Net.Get(url).StoreEvent(e)
		}
	}
}

func TestAMerchantsAcceptanceOfANewerFormatNewerThanAValidOneLeavesTheOrderWithoutOne(t *testing.T) {
	s := newAcceptanceSetup(t)
	s.newerWith(t, s.m.Now()-30, s.acceptance(t, acceptedCame, acceptanceOptions{createdAt: s.m.Now() - 60}))
	same(t, "underpaid", s.status(t), s.underpaid(t))
	if n := s.warnedOfANewerFormat(); n != 1 {
		t.Errorf("warned %d times", n)
	}
}

func TestAMerchantsAcceptanceOfANewerFormatOlderThanAValidOneLeavesItCounted(t *testing.T) {
	s := newAcceptanceSetup(t)
	s.newerWith(t, s.m.Now()-90, s.acceptance(t, acceptedCame, acceptanceOptions{createdAt: s.m.Now() - 60}))
	same(t, "unconfirmed", s.status(t), s.unconfirmed())
	if n := s.warnedOfANewerFormat(); n != 1 {
		t.Errorf("warned %d times", n)
	}
}

func TestAMerchantsAcceptanceOfANewerFormatDatedAfterTheLeasesEndIsSetAside(t *testing.T) {
	s := newAcceptanceSetup(t)
	s.newerWith(t, s.invoice.ExpiresAt+121, s.acceptance(t, acceptedCame, acceptanceOptions{}))
	same(t, "unconfirmed", s.status(t), s.unconfirmed())
	if n := s.warnedOfANewerFormat(); n != 1 {
		t.Errorf("warned %d times", n)
	}
}

// A merchant's acceptance with rest invoices, the tolerance, the Lightning part.

func TestAMerchantsAcceptanceTakesWhatRestInvoicesPaidFromItOnce(t *testing.T) {
	s := newAcceptanceSetup(t)
	// 40 000 on-chain: a rest invoice of 9 000, paid; then the transaction replaced (RBF) by one
	// paying 21 000: 30 000 in all, 19 000 asked.
	s.m.Chain.Drop(restAddress, s.tx.Txid)
	first := testkit.Payment(testkit.Txid("first"), restAddress, 40_000, nil, 0)
	s.m.Chain.Add(restAddress, first)
	s.status(t)
	made := madeOf(s.m, 9000)
	if len(made) != 1 {
		t.Fatalf("%d invoices of 9 000 sats", len(made))
	}
	rest := made[0]
	s.m.PayInvoice(rest.PaymentHash)
	s.m.SetNow(s.m.Now() + 1)
	s.m.Chain.Drop(restAddress, first.Txid)
	bump := testkit.Payment(testkit.Txid("bump"), restAddress, 21_000, coinsOf(first), 0)
	s.m.Chain.Add(restAddress, bump)
	if status := s.status(t); fieldOf(status, "state") != "open" || fieldOf(status, "received") != "30000" || fieldOf(restOf(t, status), "sats") != "19000" {
		t.Fatalf("%s", jsonv.Marshal(status))
	}
	// The merchant accepts what came: 21 000 due on-chain (30 000 less the 9 000 of the rest).
	s.hold(s.acceptance(t, acceptedCame, acceptanceOptions{}))
	same(t, "unconfirmed", s.status(t), jsonv.O("state", "unconfirmed", "received", "30000", "confirmations", 0, "txids", []string{bump.Txid}))
	testkit.Eventually(t, func() bool { return len(s.acceptanceRecords(s.m.KeyRelays[0])) == 1 }, "the acceptance record")
	same(t, "acceptance records", s.acceptanceRecords(s.m.KeyRelays[0]), []int64{30_000})
	s.m.Chain.Confirm(restAddress, bump.Txid, s.m.Chain.Height()-2)
	s.m.SetNow(s.invoice.ExpiresAt + 3600)
	same(t, "paid", s.status(t), jsonv.O("state", "paid", "method", "mixed", "received", "30000", "txids", []string{bump.Txid}, "preimages", []string{rest.Preimage}))
}

func TestAMerchantsAcceptanceAndTheToleranceJudgeWithTheLower(t *testing.T) {
	for _, c := range []struct {
		name           string
		accepted, came int64
	}{
		{"the merchant's acceptance's, lower", acceptedCame, acceptedCame},
		{"the tolerance's, lower", 48_000, leastAccepted},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := acceptanceSetupWith(t, tolerance, c.came)
			s.hold(s.acceptance(t, c.accepted, acceptanceOptions{}))
			same(t, "unconfirmed", s.status(t), s.unconfirmed())
			testkit.Eventually(t, func() bool { return len(s.acceptanceRecords(s.m.KeyRelays[0])) == 1 }, "the acceptance record")
			same(t, "acceptance records", s.acceptanceRecords(s.m.KeyRelays[0]), []int64{c.came})
		})
	}
}

func TestAMerchantsAcceptanceChangesNothingOnceTheLightningInvoiceIsPaid(t *testing.T) {
	s := newAcceptanceSetup(t)
	s.hold(s.acceptance(t, acceptedCame, acceptanceOptions{}))
	s.m.PayInvoice(s.invoice.PaymentHash)
	if status := s.status(t); fieldOf(status, "state") != "paid" || fieldOf(status, "method") != "lightning" {
		t.Errorf("%s", jsonv.Marshal(status))
	}
}

func TestAMerchantsAcceptanceReadFewerThanAMajorityAnswerIsNetwork(t *testing.T) {
	s := newAcceptanceSetup(t)
	// A first check published the witness and asked the rest: the next one publishes nothing
	// before it reads (the relays that fail its read fail every later read on every relay too).
	same(t, "underpaid", s.status(t), s.underpaid(t))
	s.hold(s.acceptance(t, acceptedCame, acceptanceOptions{}))
	s.muteRead(1, 2)
	_, err := s.c.GetStatus(ctx, s.invoice)
	wantCode(t, err, checkout.CodeNetwork)
	// Its Lightning invoice paid: paid all the same.
	s.m.PayInvoice(s.invoice.PaymentHash)
	if status := s.status(t); fieldOf(status, "state") != "paid" || fieldOf(status, "method") != "lightning" {
		t.Errorf("%s", jsonv.Marshal(status))
	}
}

func TestWaitForPaymentSeesAMerchantsAcceptanceAtItsNextCheckOfTheOnchainPartGivingTheRestTillThen(t *testing.T) {
	s := newAcceptanceSetup(t)
	socket := s.m.Net.AddExplorerSocket()
	// The full checks so far: each asks the explorer for the address's transactions once.
	asked := len(s.m.Chain.Asked())
	fullChecks := func() int {
		n := 0
		for _, url := range s.m.Chain.Asked()[asked:] {
			if strings.HasPrefix(url, proto.MempoolURL) && strings.HasSuffix(url, "/txs") {
				n++
			}
		}
		return n
	}
	var mu sync.Mutex
	var statuses []*checkout.PaymentStatus
	all := func() []any {
		mu.Lock()
		defer mu.Unlock()
		out := []any{}
		for _, st := range statuses {
			out = append(out, jsonOf(t, st))
		}
		return out
	}
	done := make(chan waited, 1)
	go func() {
		status, err := s.c.WaitForPayment(ctx, s.invoice, checkout.WithInterval(250*time.Millisecond), checkout.WithOnStatus(func(st *checkout.PaymentStatus) {
			mu.Lock()
			statuses = append(statuses, st)
			mu.Unlock()
		}))
		done <- waited{status, err}
	}()
	testkit.Eventually(t, func() bool { return len(socket.Messages()) == 2 && len(all()) > 0 }, "the subscription and a status")
	time.Sleep(500 * time.Millisecond)
	underpaid := s.underpaid(t)
	same(t, "underpaid", all()[len(all())-1], underpaid)
	// The merchant accepts: no message of the explorer, no check of the on-chain part; the checks
	// of the Lightning invoices give the status with its rest still.
	checks := fullChecks()
	s.hold(s.acceptance(t, acceptedCame, acceptanceOptions{}))
	seen := len(all())
	testkit.Eventually(t, func() bool { return len(all()) > seen+1 }, "two statuses more")
	if n := fullChecks(); n != checks {
		t.Errorf("%d full checks more", n-checks)
	}
	for _, status := range all()[seen:] {
		same(t, "underpaid", status, underpaid)
	}
	// The next check of the on-chain part (a block here; every 60 s at least) counts it.
	socket.Push(jsonv.O("block", jsonv.O("height", s.m.Chain.Height())))
	testkit.Eventually(t, func() bool { l := all(); return fieldOf(l[len(l)-1], "state") == "unconfirmed" }, "unconfirmed")
	same(t, "unconfirmed", all()[len(all())-1], s.unconfirmed())
	s.m.Chain.Confirm(restAddress, s.tx.Txid, s.m.Chain.Height()-2)
	socket.Push(jsonv.O("block", jsonv.O("height", s.m.Chain.Height())))
	w := outcomeOf(t, done)
	must(t, w.err)
	same(t, "paid", jsonOf(t, w.status), jsonv.O("state", "paid", "method", "onchain", "received", itoa(acceptedCame), "txids", []string{s.tx.Txid}))
	testkit.Eventually(t, func() bool { return len(s.acceptanceRecords(s.m.KeyRelays[0])) == 1 }, "the acceptance record")
	same(t, "acceptance records", s.acceptanceRecords(s.m.KeyRelays[0]), []int64{acceptedCame})
}
