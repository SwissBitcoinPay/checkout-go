package checkout_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/onchain"
	"github.com/sattleme/checkout-go/internal/testkit"
)

// More attacks on on-chain payments (F1, F2, G3, G4, H1, H2, I1, L1, L2, R6, R7, and S6 for
// witnesses), end to end against local relays and fakes of the explorers. Each test states what
// must hold (onchain_security_test.go holds S1 to S6; L3 is internal/relay's).

// leaseKinds are the kinds of the records of author at d a relay holds: "lease", "withdrawn" or
// "unopened".
func leaseKinds(m *testkit.Merchant, r *testkit.Relay, d, author string) []string {
	out := []string{}
	for _, v := range r.Events() {
		if pubkey, _ := jsonv.Field(v, "pubkey"); pubkey != author || nostr.DTagOf(v) != d {
			continue
		}
		if record, ok := onchain.OpenLeaseRecord(v, m.RegistryKey, d); ok {
			out = append(out, record.Outside.Kind)
		} else {
			out = append(out, "unopened")
		}
	}
	return out
}

// holds tells whether a relay holds records of author at d of these kinds.
func holds(m *testkit.Merchant, r *testkit.Relay, d, author string, kinds ...string) bool {
	return slices.Equal(leaseKinds(m, r, d, author), kinds)
}

func refuseWhen(f func(e *nostr.Event) bool) func(e *nostr.Event) string {
	return func(e *nostr.Event) string {
		if f(e) {
			return "error: down"
		}
		return ""
	}
}

func atD(e *nostr.Event, d string) bool { tag, _ := e.DTag(); return tag == d }

// byAuthor tells whether author signed an event.
func byAuthor(author string) func(e *nostr.Event) bool {
	return func(e *nostr.Event) bool { return e.PubKey == author }
}

func TestF1AWithdrawalOnOneRelayOnlyFreesTheAddressThereForAnotherOrder(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:1], 91)
	m.SetPay(pay("maxSendable", 1_000_000))
	r := relaysOf(m)
	start := m.Now()
	d := derive.LeaseAddress(m.RegistryKey, lot[0])
	author := m.Sale("F1-R").Author
	// Another server's short lease of the address reaches every relay right after R's: R's lease
	// is not confirmed. R's withdrawal of it reaches r1 only.
	race := testkit.OnFirst(byAuthor(author), func() { everywhere(m, leaseBy(t, m, "F1-C", lot[0], start, 60, nil)) })
	for i, relay := range r {
		var taken atomic.Int32
		first := i == 0
		relay.SetBehaviour(testkit.Behaviour{
			OnEvent: race,
			RefuseEvent: func(e *nostr.Event) string {
				if !first && e.PubKey == author && atD(e, d) && taken.Add(1) > 1 {
					return "blocked: rate-limited"
				}
				return ""
			},
		})
	}
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "F1-R", Sats: 49_000})
	wantCode(t, err, checkout.CodeOnchainUnavailable)
	for _, relay := range r {
		relay.SetBehaviour(testkit.Behaviour{})
	}
	testkit.Eventually(t, func() bool { return holds(m, r[0], d, author, "withdrawn") }, "the withdrawal on r1")
	same(t, "r2", leaseKinds(m, r[1], d, author), []string{"lease"})
	same(t, "r3", leaseKinds(m, r[2], d, author), []string{"lease"})
	// The other lease over; R's still running on r2 and r3.
	m.SetNow(start + 600)
	r[0].SetBehaviour(testkit.Behaviour{QueryDelay: 500 * time.Millisecond})
	_, err = clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "F1-R", Sats: 49_000})
	// R's lease was never confirmed: it gives R no address, and still holds it where it is.
	wantCode(t, err, checkout.CodeOnchainUnavailable)
	r[0].SetBehaviour(testkit.Behaviour{})
	// r2 never answers a read of leases: the read that finds the free addresses is r1's and r3's
	// (the confirmation, on every relay, waits for it one timeout).
	r[1].SetBehaviour(testkit.Behaviour{MuteQuery: completeReads})
	other, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "F1-D", Sats: 49_000})
	// Read on r1 and r3, R's withdrawal is its newest record: the address is free for D.
	if err != nil || other.OnchainAddress != lot[0] {
		t.Errorf("D got %v, %v", other, err)
	}
}

func TestF2AnInvoiceShownAndPaidKeepsItsAddressWhenTheOrderAsksAgain(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:2], 91)
	m.SetPay(pay("maxSendable", 1_000_000))
	r := relaysOf(m)
	c := clientOf(t, m)
	a, err := c.CreateInvoice(ctx, checkout.Order{Ref: "F2-A", Sats: 49_000, DurationSeconds: 900})
	must(t, err)
	x := a.OnchainAddress
	m.Chain.Add(x, testkit.Payment(testkit.Txid("F2 pays"), x, 49_000, nil, 0))
	status := statusJSON(t, c, a)
	if fieldOf(status, "state") != "unconfirmed" || fieldOf(status, "received") != "49000" {
		t.Fatalf("%s", jsonv.Marshal(status))
	}
	// Another server's lease of x, its withdrawal on r1 only (a blip).
	b := m.Sale("F2-B")
	everywhere(m, leaseBy(t, m, "F2-B", x, m.Now()+1, 900, nil))
	withdrawal, err := onchain.WithdrawalEvent(b, x, m.Now()+2, rand.Reader)
	must(t, err)
	r[0].Take(withdrawal)
	m.SetNow(m.Now() + 30)
	r[0].SetBehaviour(testkit.Behaviour{QueryDelay: 500 * time.Millisecond})
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "F2-A", Sats: 49_000, DurationSeconds: 900})
	if pending := wantCode(t, err, checkout.CodeInvoicePending); pending.Invoice.OnchainAddress != x {
		t.Errorf("pending on %q", pending.Invoice.OnchainAddress)
	}
}

func TestG3TheLeaseAFloodKeptFromBeingConfirmedIsWithdrawn(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:1], 91)
	// On-chain only: no address, no invoice.
	m.SetPay(pay("maxSendable", 1_000_000))
	r := relaysOf(m)
	d := derive.LeaseAddress(m.RegistryKey, lot[0])
	author := m.Sale("G3-1").Author
	// Whoever watches the relays sees the new lease, and fills its d tag at once: 101 records of
	// one second, so that no read of it is complete.
	var records []*nostr.Event
	for i := 0; i < 101; i++ {
		records = append(records, junk(t, d, m.Now(), i, "x"))
	}
	var flooded atomic.Bool
	flood := testkit.OnFirst(byAuthor(author), func() {
		for _, e := range records {
			everywhere(m, e)
		}
		flooded.Store(true)
	})
	for _, relay := range r {
		relay.SetBehaviour(testkit.Behaviour{OnEvent: flood})
	}
	_, err := clientOf(t, m, checkout.WithRelayTimeout(floodTimeout)).CreateInvoice(ctx, checkout.Order{Ref: "G3-1", Sats: 49_000})
	wantCode(t, err, checkout.CodeOnchainUnavailable)
	if !flooded.Load() {
		t.Fatal("not flooded")
	}
	for _, relay := range r {
		testkit.Eventually(t, func() bool { return holds(m, relay, d, author, "withdrawn") }, relay.URL+" holds the withdrawal")
	}
	// The flood over (its records gone): the address is free at once, not at the lease's end.
	junkAuthors := map[string]bool{}
	for _, v := range r[0].Events() {
		if pubkey, _ := jsonv.Field(v, "pubkey"); nostr.DTagOf(v) == d && pubkey != author {
			junkAuthors[pubkey.(string)] = true
		}
	}
	for _, relay := range r {
		relay.SetBehaviour(testkit.Behaviour{Hide: func(event any) bool {
			pubkey, _ := jsonv.Field(event, "pubkey")
			s, _ := pubkey.(string)
			return junkAuthors[s]
		}})
	}
	next, err := clientOf(t, m, checkout.WithRelayTimeout(floodTimeout)).CreateInvoice(ctx, checkout.Order{Ref: "G3-2", Sats: 49_000})
	must(t, err)
	if next.OnchainAddress != lot[0] {
		t.Errorf("leased %q", next.OnchainAddress)
	}
}

func TestG4OfTwoInvoicesThatMayStillBePaidTheOneShownAndPaidWins(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:2], 91)
	m.SetPay(pay("maxSendable", 1_000_000))
	r := relaysOf(m)
	author := m.Sale("G4-R").Author
	// The lease's mark (its second record) reaches r1 only: network, nothing shown.
	var mu sync.Mutex
	first := ""
	for _, relay := range r[1:] {
		taken := 0
		relay.SetBehaviour(testkit.Behaviour{RefuseEvent: func(e *nostr.Event) string {
			if e.PubKey != author {
				return ""
			}
			d, _ := e.DTag()
			mu.Lock()
			defer mu.Unlock()
			if first == "" {
				first = d
			}
			if d == first {
				taken++
				if taken > 1 {
					return "blocked: rate-limited"
				}
			}
			return ""
		}})
	}
	c := clientOf(t, m)
	_, err := c.CreateInvoice(ctx, checkout.Order{Ref: "G4-R", Sats: 49_000})
	wantCode(t, err, checkout.CodeNetwork)
	// Asked again while r1 is slow: its lease reads as not confirmed, so another invoice is made,
	// shown and paid.
	r[1].SetBehaviour(testkit.Behaviour{})
	r[2].SetBehaviour(testkit.Behaviour{})
	r[0].SetBehaviour(testkit.Behaviour{MuteQuery: readsOf(author)})
	shown, err := c.CreateInvoice(ctx, checkout.Order{Ref: "G4-R", Sats: 49_000})
	must(t, err)
	if shown.OnchainAddress == "" {
		t.Fatal("no address")
	}
	r[0].SetBehaviour(testkit.Behaviour{})
	tx := testkit.Payment(testkit.Txid("G4 shown"), shown.OnchainAddress, 49_000, nil, 0)
	m.Chain.Add(shown.OnchainAddress, tx)
	if status := statusJSON(t, c, shown); fieldOf(status, "state") != "unconfirmed" {
		t.Fatalf("%s", jsonv.Marshal(status))
	}
	m.Chain.Confirm(shown.OnchainAddress, tx.Txid, m.Chain.Height()-5)
	// Asked again, every relay answering: the first invoice is confirmed on r1, older, and may
	// still be paid; the order was paid with the one shown.
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "G4-R", Sats: 49_000})
	if paid := wantCode(t, err, checkout.CodeOrderPaid); paid.Invoice.OnchainAddress != shown.OnchainAddress {
		t.Errorf("paid on %q", paid.Invoice.OnchainAddress)
	}
}

// slowWrites is a WebSocket dialer whose events reach the relays 300 ms late.
type slowWrites struct{ d checkout.WebSocketDialer }

func (s slowWrites) Dial(ctx context.Context, url string, header http.Header) (checkout.WebSocketConn, error) {
	conn, err := s.d.Dial(ctx, url, header)
	if err != nil {
		return nil, err
	}
	return slowConn{conn}, nil
}

type slowConn struct{ checkout.WebSocketConn }

func (c slowConn) Write(ctx context.Context, data []byte) error {
	if !strings.HasPrefix(string(data), `["EVENT"`) {
		return c.WebSocketConn.Write(ctx, data)
	}
	message := append([]byte(nil), data...)
	time.AfterFunc(300*time.Millisecond, func() {
		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Closed meanwhile: nothing.
		_ = c.WebSocketConn.Write(writeCtx, message)
	})
	return nil
}

func TestH1TwoCallsOfOneOrderAtOnceTheLoserNeverEndsTheWinnersLease(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:1], 91)
	m.SetPay(pay("maxSendable", 1_000_000))
	r := relaysOf(m)
	d := derive.LeaseAddress(m.RegistryKey, lot[0])
	author := m.Sale("H1-R").Author
	c := m.Now()
	// Once the winner's mark (created_at c + 1) is kept, two seconds pass. The first read of the
	// sale's records is the loser's: both calls read them before either wrote (as node's calls,
	// started at once, do).
	var once sync.Once
	loserReads := make(chan struct{})
	for _, relay := range r {
		relay.SetBehaviour(testkit.Behaviour{
			OnEvent: func(e *nostr.Event, _ *testkit.Relay) {
				if e.PubKey == author && e.CreatedAt == c+1 && atD(e, d) {
					m.SetNow(c + 2)
				}
			},
			Raw: func(message []any, _ func(string)) bool {
				if len(message) > 2 && message[0] == "REQ" {
					if authors, _ := jsonv.Field(message[2], "authors"); fmt.Sprint(authors) == fmt.Sprint([]any{author}) {
						once.Do(func() { close(loserReads) })
					}
				}
				return false
			},
		})
	}
	// The loser's writes reach the relays 300 ms late.
	slow := checkout.WithWebSocketDialer(slowWrites{checkout.NewWebSocketDialer(m.Net.Transport())})
	var wg sync.WaitGroup
	var loserErr, winnerErr error
	var winner *checkout.Invoice
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, loserErr = clientOf(t, m, slow).CreateInvoice(ctx, checkout.Order{Ref: "H1-R", Sats: 49_000})
	}()
	select {
	case <-loserReads:
	case <-time.After(10 * time.Second):
		t.Fatal("the loser never read the sale's records")
	}
	go func() {
		defer wg.Done()
		winner, winnerErr = clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "H1-R", Sats: 49_000})
	}()
	wg.Wait()
	for _, relay := range r {
		relay.SetBehaviour(testkit.Behaviour{})
	}
	wantCode(t, loserErr, checkout.CodeOnchainUnavailable)
	must(t, winnerErr)
	if winner.OnchainAddress != lot[0] {
		t.Fatalf("the winner leased %q", winner.OnchainAddress)
	}
	// The winner's mark stays the order's newest record there, on every relay.
	time.Sleep(500 * time.Millisecond)
	for _, relay := range r {
		same(t, relay.URL, leaseKinds(m, relay, d, author), []string{"lease"})
	}
	m.SetNow(c + 30)
	// Another order gets no address; the order's invoice is still the one handed out.
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "H1-D", Sats: 49_000})
	wantCode(t, err, checkout.CodeOnchainUnavailable)
	_, err = clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "H1-R", Sats: 49_000})
	if pending := wantCode(t, err, checkout.CodeInvoicePending); pending.Invoice.OnchainAddress != lot[0] {
		t.Errorf("pending on %q", pending.Invoice.OnchainAddress)
	}
}

// H2: of two invoices that may still be paid, the one shown is handed out. Call 1 (its server's
// clock skew s ahead): its mark reaches r1 only, network. Call 2 (r1 slow) makes and shows
// another invoice. Call 3 (r2 slow) reads both.
func TestH2OfTwoInvoicesThatMayStillBePaidTheOneShownIsHandedOut(t *testing.T) {
	for _, skew := range []int64{3, 0} {
		t.Run("skew "+itoa(skew), func(t *testing.T) {
			m := securityMerchant(t)
			m.SetPay(pay("maxSendable", 1_000_000))
			x, y := lot[0], lot[1]
			m.PublishLot([]string{x}, 91)
			r := relaysOf(m)
			author := m.Sale("H2-R").Author
			c1 := m.Now() + skew
			refuse := testkit.Behaviour{RefuseEvent: refuseWhen(func(e *nostr.Event) bool { return e.PubKey == author && e.CreatedAt > c1 })}
			r[1].SetBehaviour(refuse)
			r[2].SetBehaviour(refuse)
			ahead := checkout.WithClock(func() time.Time { return time.Unix(m.Now()+skew, 0) })
			_, err := clientOf(t, m, ahead).CreateInvoice(ctx, checkout.Order{Ref: "H2-R", Sats: 49_000})
			wantCode(t, err, checkout.CodeNetwork)
			m.PublishLot([]string{x, y}, 92)
			r[1].SetBehaviour(testkit.Behaviour{})
			r[2].SetBehaviour(testkit.Behaviour{})
			// r1, which holds the first invoice's records, never answers the read of the order's.
			r[0].SetBehaviour(testkit.Behaviour{MuteQuery: readsOf(author)})
			shown, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "H2-R", Sats: 49_000})
			must(t, err)
			if shown.OnchainAddress != y {
				t.Fatalf("shown on %q", shown.OnchainAddress)
			}
			r[0].SetBehaviour(testkit.Behaviour{})
			r[1].SetBehaviour(testkit.Behaviour{QueryDelay: 500 * time.Millisecond})
			_, err = clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "H2-R", Sats: 49_000})
			if pending := wantCode(t, err, checkout.CodeInvoicePending); pending.Invoice.OnchainAddress != shown.OnchainAddress {
				t.Errorf("pending on %q", pending.Invoice.OnchainAddress)
			}
		})
	}
}

func TestI1AnObjectThatOnlyCarriesTheIDOfTheMarkNeverShownCountsForNothing(t *testing.T) {
	m := securityMerchant(t)
	m.SetPay(pay("maxSendable", 1_000_000))
	x, y := lot[0], lot[1]
	m.PublishLot([]string{x}, 91)
	r := relaysOf(m)
	author := m.Sale("I1-R").Author
	c := m.Now()
	// Call 1: its mark reaches r1 only, network.
	later := testkit.Behaviour{RefuseEvent: refuseWhen(func(e *nostr.Event) bool { return e.PubKey == author && e.CreatedAt > c })}
	r[1].SetBehaviour(later)
	r[2].SetBehaviour(later)
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "I1-R", Sats: 49_000})
	wantCode(t, err, checkout.CodeNetwork)
	// Call 2: r1 slow and refusing the order's records; its invoice, on y, is shown.
	m.PublishLot([]string{x, y}, 92)
	m.SetNow(c + 30)
	r[1].SetBehaviour(testkit.Behaviour{})
	r[2].SetBehaviour(testkit.Behaviour{})
	r[0].SetBehaviour(testkit.Behaviour{MuteQuery: readsOf(author), RefuseEvent: refuseWhen(func(e *nostr.Event) bool { return e.PubKey == author })})
	shown, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "I1-R", Sats: 49_000})
	must(t, err)
	if shown.OnchainAddress != y {
		t.Fatalf("shown on %q", shown.OnchainAddress)
	}
	at := func(relay *testkit.Relay, address string) *jsonv.Object {
		d := derive.LeaseAddress(m.RegistryKey, address)
		for _, v := range relay.Events() {
			if pubkey, _ := jsonv.Field(v, "pubkey"); pubkey == author && nostr.DTagOf(v) == d {
				return v.(*jsonv.Object)
			}
		}
		t.Fatalf("no record of %s on %s", address, relay.URL)
		return nil
	}
	markX := at(r[0], x)
	testkit.Eventually(t, func() bool {
		d := derive.LeaseAddress(m.RegistryKey, y)
		for _, v := range r[1].Events() {
			if pubkey, _ := jsonv.Field(v, "pubkey"); pubkey == author && nostr.DTagOf(v) == d {
				return true
			}
		}
		return false
	}, "y's mark on r2")
	markY := at(r[1], y)
	// Call 3: r3 hides y's mark, and sends an object that only carries x's mark's id. It answers
	// last (in time for the read on every relay), as it does in node's test: the first read, on r1
	// and r2, finds both invoices. Each of its reads 1.5 s late, well within the call's timeout
	// (8 s): r1 and r2 answer first, whatever the load of the machine running the test.
	r[0].SetBehaviour(testkit.Behaviour{})
	markYID, _ := markY.Get("id")
	r[2].SetBehaviour(testkit.Behaviour{
		Hide:       func(event any) bool { id, _ := jsonv.Field(event, "id"); return id == markYID },
		QueryDelay: 1500 * time.Millisecond,
	})
	fake := jsonv.NewObject()
	for _, name := range markX.Keys() {
		value, _ := markX.Get(name)
		fake.Set(name, value)
	}
	fake.Set("content", "not the event")
	fake.Set("sig", strings.Repeat("00", 64))
	r[2].Store(fake)
	m.SetNow(c + 60)
	_, err = clientOf(t, m, checkout.WithRelayTimeout(8*time.Second)).CreateInvoice(ctx, checkout.Order{Ref: "I1-R", Sats: 49_000})
	if pending := wantCode(t, err, checkout.CodeInvoicePending); pending.Invoice.OnchainAddress != y {
		t.Errorf("pending on %q", pending.Invoice.OnchainAddress)
	}
}

func TestS6WitnessesReadAgainOnEveryRelayStillShowAnotherOrdersWitness(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:1], 91)
	c := clientOf(t, m, checkout.WithRelayTimeout(8*time.Second))
	p, err := c.CreateInvoice(ctx, checkout.Order{Ref: "W6-P", Sats: 49_000})
	must(t, err)
	x := p.OnchainAddress
	tx := testkit.Payment(testkit.Txid("W6 paid once"), x, 49_000, nil, 0)
	// Another order's witness of tx, in time for it: on r1 (which hides it) and r2 (late), not r3.
	seen := onchain.SeenOf(m.IntegrationKey)
	witness, err := onchain.WitnessEvent(seen, onchain.Witness{
		Address: x, Txid: tx.Txid, Sale: m.Sale("W6-N").Author, Sats: 49_000, Inputs: coinsOf(tx), ExpiresAt: p.ExpiresAt,
	}, m.Now(), rand.Reader)
	must(t, err)
	r := relaysOf(m)
	r[0].Take(witness)
	r[1].Take(witness)
	r[0].SetBehaviour(testkit.Behaviour{Hide: func(event any) bool { id, _ := jsonv.Field(event, "id"); return id == witness.ID }})
	r[1].SetBehaviour(testkit.Behaviour{QueryDelay: 1500 * time.Millisecond})
	m.Chain.Add(x, tx)
	// The first read (r1 and r3) misses it, so this check publishes its own witness; read again on
	// every relay, r2 shows the other's: tx is another invoice's, and counts for nothing here.
	status := statusJSON(t, c, p)
	if fieldOf(status, "state") != "open" || fieldOf(status, "received") != "0" {
		t.Errorf("%s", jsonv.Marshal(status))
	}
}

func TestR6ARelayThatFailedIsWaitedForOncePerCall(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:2], 91)
	m.SetPay(pay("maxSendable", 1_000_000))
	r := relaysOf(m)
	author := m.Sale("R6-R").Author
	// Each lease of the order meets another server's lease of its address at once: not confirmed.
	var mu sync.Mutex
	competing := map[string]bool{}
	for _, relay := range r[:2] {
		relay.SetBehaviour(testkit.Behaviour{OnEvent: func(e *nostr.Event, _ *testkit.Relay) {
			d, ok := e.DTag()
			mu.Lock()
			if e.PubKey != author || !ok || competing[d] {
				mu.Unlock()
				return
			}
			competing[d] = true
			mu.Unlock()
			for _, address := range lot {
				if derive.LeaseAddress(m.RegistryKey, address) == d {
					other := leaseBy(t, m, "R6-OTHER-"+address, address, m.Now(), 900, nil)
					r[0].Take(other)
					r[1].Take(other)
				}
			}
		}})
	}
	r[2].SetBehaviour(testkit.Behaviour{Mute: true})
	const timeout = 3 * time.Second
	started := time.Now()
	_, err := clientOf(t, m, checkout.WithRelayTimeout(timeout)).CreateInvoice(ctx, checkout.Order{Ref: "R6-R", Sats: 49_000})
	elapsed := time.Since(started)
	wantCode(t, err, checkout.CodeOnchainUnavailable)
	if len(competing) != 2 {
		t.Errorf("%d leases met another", len(competing))
	}
	// The first confirmation waits for r3 until its timeout; the second counts it failed at once.
	if elapsed >= 2*timeout {
		t.Errorf("took %v: the mute relay waited for twice", elapsed)
	}
}

func TestR7AReadOnEveryRelayKeepsThePagesARelaySentBeforeTheCut(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:1], 91)
	m.SetPay(pay("maxSendable", 1_000_000))
	d := derive.LeaseAddress(m.RegistryKey, lot[0])
	r := relaysOf(m)
	// Anybody's records at the address's d tag, older than the lease: 20 pages to read.
	for i := 0; i < 1950; i++ {
		everywhere(m, junk(t, d, m.Now()-10-int64(i), i, "x"))
	}
	lease := leaseBy(t, m, "R7-A", lot[0], m.Now(), 900, nil)
	// r1 missed the lease; h hides it; r2 answers each page in 150 ms, its 20 pages outlasting the
	// one timeout the read waits after a majority.
	r[1].Take(lease)
	r[2].Take(lease)
	r[1].SetBehaviour(testkit.Behaviour{Hide: func(event any) bool { id, _ := jsonv.Field(event, "id"); return id == lease.ID }})
	r[2].SetBehaviour(testkit.Behaviour{QueryDelay: 150 * time.Millisecond})
	_, err := clientOf(t, m, checkout.WithRelayTimeout(time.Second)).CreateInvoice(ctx, checkout.Order{Ref: "R7-D", Sats: 49_000})
	wantCode(t, err, checkout.CodeOnchainUnavailable)
}

// markOf is the mark of another call of the order ref: its lease of address at c, confirmed at
// c + 1.
func markOf(t *testing.T, m *testkit.Merchant, ref, address string, c int64) *nostr.Event {
	t.Helper()
	e, err := onchain.LeaseEventOf(m.Sale(ref), onchain.LeaseTerms{
		Address:      address,
		Sats:         49_000,
		CreatedAt:    c,
		ExpiresAt:    c + 900,
		Price:        declaration.Price{Minor: 49_000, Digits: 0, Currency: "SAT"},
		Note:         ref,
		Confirmed:    true,
		HasConfirmed: true,
	}, c+1, rand.Reader)
	must(t, err)
	return e
}

// withdrawalsOf are the withdrawals of author at d the relays received.
func withdrawalsOf(m *testkit.Merchant, d, author string) []*nostr.Event {
	var out []*nostr.Event
	for _, relay := range relaysOf(m) {
		for _, e := range relay.Received() {
			if record, ok := onchain.OpenLeaseRecord(testkit.EventValue(e), m.RegistryKey, d); ok && e.PubKey == author && record.Outside.Kind == onchain.OutsideWithdrawn {
				out = append(out, e)
			}
		}
	}
	return out
}

func TestL1FewerThanAMajorityAnswerTheConfirmationTheMinorityShowingTheMarkNotWithdrawn(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:1], 91)
	m.SetPay(pay("maxSendable", 1_000_000))
	d := derive.LeaseAddress(m.RegistryKey, lot[0])
	author := m.Sale("L1-R").Author
	mark := markOf(t, m, "L1-R", lot[0], m.Now())
	r := relaysOf(m)
	// Once this call's lease is taken, the other call's mark is everywhere, and r2 and r3 answer
	// reads too late (their writes at once).
	marking := testkit.OnFirst(byAuthor(author), func() {
		everywhere(m, mark)
		r[1].SetBehaviour(testkit.Behaviour{QueryDelay: 5 * time.Second})
		r[2].SetBehaviour(testkit.Behaviour{QueryDelay: 5 * time.Second})
	})
	for _, relay := range r {
		relay.SetBehaviour(testkit.Behaviour{OnEvent: marking})
	}
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "L1-R", Sats: 49_000})
	wantCode(t, err, checkout.CodeNetwork)
	for _, relay := range r {
		relay.SetBehaviour(testkit.Behaviour{})
	}
	// r1 showed the mark: no withdrawal, which could have replaced it (same second, a lower id).
	time.Sleep(200 * time.Millisecond)
	if w := withdrawalsOf(m, d, author); len(w) != 0 {
		t.Errorf("%d withdrawals", len(w))
	}
	for _, relay := range r {
		same(t, relay.URL, leaseKinds(m, relay, d, author), []string{"lease"})
	}
	m.SetNow(m.Now() + 30)
	_, err = clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "L1-D", Sats: 49_000})
	wantCode(t, err, checkout.CodeOnchainUnavailable)
}

func TestL2ACopyOfTheMarkWithADamagedSignatureSentFirstHidesNoValidCopy(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:1], 91)
	m.SetPay(pay("maxSendable", 1_000_000))
	d := derive.LeaseAddress(m.RegistryKey, lot[0])
	author := m.Sale("L2-R").Author
	mark := markOf(t, m, "L2-R", lot[0], m.Now())
	damaged := testkit.EventValue(mark).(*jsonv.Object)
	sig := mark.Sig
	if sig[0] == '0' {
		sig = "1" + sig[1:]
	} else {
		sig = "0" + sig[1:]
	}
	damaged.Set("sig", sig)
	r := relaysOf(m)
	// r1 sends the damaged copy alone, first; r2 and r3 the valid one, 100 ms later.
	marking := testkit.OnFirst(byAuthor(author), func() {
		everywhere(m, mark)
		r[0].Store(damaged)
		r[0].SetBehaviour(testkit.Behaviour{Hide: func(event any) bool {
			id, _ := jsonv.Field(event, "id")
			s, _ := jsonv.Field(event, "sig")
			return id == mark.ID && s == mark.Sig
		}})
		r[1].SetBehaviour(testkit.Behaviour{QueryDelay: 100 * time.Millisecond})
		r[2].SetBehaviour(testkit.Behaviour{QueryDelay: 100 * time.Millisecond})
	})
	for _, relay := range r {
		relay.SetBehaviour(testkit.Behaviour{OnEvent: marking})
	}
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "L2-R", Sats: 49_000})
	wantCode(t, err, checkout.CodeOnchainUnavailable)
	for _, relay := range r {
		relay.SetBehaviour(testkit.Behaviour{})
	}
	time.Sleep(200 * time.Millisecond)
	if w := withdrawalsOf(m, d, author); len(w) != 0 {
		t.Errorf("%d withdrawals", len(w))
	}
	for _, relay := range r[1:] {
		same(t, relay.URL, leaseKinds(m, relay, d, author), []string{"lease"})
	}
}
