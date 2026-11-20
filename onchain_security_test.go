package checkout_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/onchain"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/relay"
	"github.com/sattleme/checkout-go/internal/secp"
	"github.com/sattleme/checkout-go/internal/testkit"
)

// Attacks on on-chain payments (S1 to S6; SPEC.md §13, §21, §31), end to end against local relays
// and fakes of the explorers. Each test states what must hold.

// securityMerchant is a merchant whose channel names its relays.
func securityMerchant(t *testing.T) *testkit.Merchant {
	t.Helper()
	m := testkit.NewMerchant()
	m.Start(nil, nil)
	t.Cleanup(m.Stop)
	m.PublishChannel(m.ChannelValue(100, jsonv.Member{Name: "relays", Value: m.KeyRelays}), 100)
	return m
}

// seeded is reproducible random bytes.
func seeded(seed int) io.Reader { return testkit.Random("seeded " + strconv.Itoa(seed)) }

// leaseBy is the lease of address by the sale of ref, from createdAt for seconds.
func leaseBy(t *testing.T, m *testkit.Merchant, ref, address string, createdAt, seconds int64, random io.Reader) *nostr.Event {
	t.Helper()
	if random == nil {
		random = rand.Reader
	}
	e, err := onchain.LeaseEventOf(m.Sale(ref), onchain.LeaseTerms{
		Address:   address,
		Sats:      1000,
		CreatedAt: createdAt,
		ExpiresAt: createdAt + seconds,
		Price:     declaration.Price{Minor: 1000, Digits: 0, Currency: "SAT"},
		Note:      ref,
	}, createdAt, random)
	must(t, err)
	return e
}

// junk is a record anybody writes at a registry address (it is public): a throwaway key signs
// it.
func junk(t *testing.T, d string, createdAt int64, seed int, content string) *nostr.Event {
	t.Helper()
	secret := sha256.Sum256([]byte("throwaway " + strconv.Itoa(seed)))
	e, err := nostr.Sign(nostr.Template{CreatedAt: createdAt, Kind: proto.RecordKind, Tags: [][]string{{"d", d}}, Content: content},
		secret[:], bytes.NewReader(make([]byte, 32)))
	must(t, err)
	return e
}

// everywhere has every relay of the key take e.
func everywhere(m *testkit.Merchant, e *nostr.Event) {
	for _, url := range m.KeyRelays {
		m.Net.Get(url).Take(e)
	}
}

func relaysOf(m *testkit.Merchant) []*testkit.Relay {
	var out []*testkit.Relay
	for _, url := range m.KeyRelays {
		out = append(out, m.Net.Get(url))
	}
	return out
}

func onchainAddressOf(t *testing.T, c *checkout.Client, ref string) string {
	t.Helper()
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: ref, Sats: 49_000})
	must(t, err)
	return invoice.OnchainAddress
}

// floodTimeout bounds each relay request of the tests that read floods of records (S5, S4, G3):
// hundreds to thousands of them, signed, sent and read from every relay, take seconds on a busy
// machine with the race detector. What they test is what the read decides, not how fast it is
// (checkout-node gives them the same 4 s).
const floodTimeout = 4 * time.Second

// S5: a paged read of the leases misses a lease every relay holds.
func TestS5AReadOfTheLeasesNeverMissesALeaseEveryRelayHolds(t *testing.T) {
	setup := func(t *testing.T) (*testkit.Merchant, string) {
		m := securityMerchant(t)
		m.PublishLot(lot[:1], 91)
		return m, derive.LeaseAddress(m.RegistryKey, lot[0])
	}
	t.Run("control: another order's lease keeps the address", func(t *testing.T) {
		m, _ := setup(t)
		everywhere(m, leaseBy(t, m, "HOLDER", lot[0], m.Now(), 900, nil))
		if got := onchainAddressOf(t, clientOf(t, m), "S5-0"); got != "" {
			t.Error("leased an address another order holds")
		}
	})
	t.Run("1. ties: a page of records of the lease's very second, with lower ids", func(t *testing.T) {
		m, d := setup(t)
		for _, r := range relaysOf(m) {
			r.SetBehaviour(testkit.Behaviour{Cap: 500})
		}
		// At equal created_at, the lowest id first (NIP-01; strfry: the last stored first).
		var lease *nostr.Event
		for seed := 0; lease == nil || lease.ID < "c"; seed++ {
			lease = leaseBy(t, m, "HOLDER", lot[0], m.Now(), 900, seeded(seed))
		}
		everywhere(m, lease)
		for i, made := 0, 0; made < 500; i++ {
			if record := junk(t, d, lease.CreatedAt, i, "x"); record.ID < lease.ID {
				everywhere(m, record)
				made++
			}
		}
		if got := onchainAddressOf(t, clientOf(t, m, checkout.WithRelayTimeout(floodTimeout)), "S5-1"); got != "" {
			t.Error("leased an address another order holds")
		}
	})
	t.Run("2. unusable dates: a page of records dated beyond 2^53", func(t *testing.T) {
		m, d := setup(t)
		for _, r := range relaysOf(m) {
			r.SetBehaviour(testkit.Behaviour{Cap: 500})
		}
		everywhere(m, leaseBy(t, m, "HOLDER", lot[0], m.Now(), 900, nil))
		for i := 0; i < 500; i++ {
			record := junk(t, d, 1<<53+2*int64(i+1), i, "x")
			// As a relay that keeps any date stores it (strfry refuses one more than 900 s ahead).
			for _, r := range relaysOf(m) {
				r.StoreEvent(record)
			}
		}
		if got := onchainAddressOf(t, clientOf(t, m, checkout.WithRelayTimeout(floodTimeout)), "S5-2"); got != "" {
			t.Error("leased an address another order holds")
		}
	})
	t.Run("3. volume: more than 20 pages of records newer than the lease", func(t *testing.T) {
		m, d := setup(t)
		for _, r := range relaysOf(m) {
			r.SetBehaviour(testkit.Behaviour{Cap: 100})
		}
		everywhere(m, leaseBy(t, m, "HOLDER", lot[0], m.Now()-600, 900, nil))
		for i := 0; i < 2001; i++ {
			everywhere(m, junk(t, d, m.Now()-599+int64(i%500), i, "x"))
		}
		if got := onchainAddressOf(t, clientOf(t, m, checkout.WithRelayTimeout(floodTimeout)), "S5-3"); got != "" {
			t.Error("leased an address another order holds")
		}
	})
	t.Run("4. order: a relay that answers a filter without a limit oldest first", func(t *testing.T) {
		m, d := setup(t)
		for _, r := range relaysOf(m) {
			r.SetBehaviour(testkit.Behaviour{OldestFirstWithoutLimit: true})
		}
		everywhere(m, leaseBy(t, m, "HOLDER", lot[0], m.Now(), 900, nil))
		for i := 0; i < 1000; i++ {
			everywhere(m, junk(t, d, m.Now()-7000+int64(i), i, "x"))
		}
		if got := onchainAddressOf(t, clientOf(t, m, checkout.WithRelayTimeout(floodTimeout)), "S5-4"); got != "" {
			t.Error("leased an address another order holds")
		}
	})
}

// S4: records of a newer format, signed by any key, hold every address.
func TestS4AnyonesRecordsAtTheRegistryAddressesTakeNothingAway(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot, 91)
	for i, address := range lot {
		everywhere(m, junk(t, derive.LeaseAddress(m.RegistryKey, address), m.Now(), 10_000+i, ""))
	}
	if got := onchainAddressOf(t, clientOf(t, m), "S4-1"); got == "" {
		t.Error("an empty content took on-chain away")
	}
}

func TestS4EachSuchRecordCostsNoSignatureCheck(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:1], 91)
	d := derive.LeaseAddress(m.RegistryKey, lot[0])
	for i := 0; i < 1000; i++ {
		everywhere(m, junk(t, d, m.Now()-int64(i), 20_000+i, "#"))
	}
	before := secp.Verified()
	_, err := clientOf(t, m, checkout.WithRelayTimeout(floodTimeout)).CreateInvoice(ctx, checkout.Order{Ref: "S4-2", Sats: 49_000})
	must(t, err)
	// The channel, the lot, the order's own records: a handful. Never one per junk record.
	if n := secp.Verified() - before; n >= 50 {
		t.Errorf("%d signatures checked", n)
	}
}

// handedOut is the address an outcome of createInvoice hands out: its invoice's, or the pending
// one's.
func handedOut(t *testing.T, invoice *checkout.Invoice, err error) string {
	t.Helper()
	var e *checkout.Error
	switch {
	case err == nil:
		return invoice.OnchainAddress
	case errors.As(err, &e) && e.Code == checkout.CodeInvoicePending:
		return e.Invoice.OnchainAddress
	}
	return ""
}

// S1: an earlier invoice whose lease was never confirmed is handed out again.
func TestS1AnOrderPaidOnchainOnlyWhoseLeaseReachedOneRelay(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:1], 91)
	m.SetPay(pay("maxSendable", 1_000_000))
	r := relaysOf(m)
	author := m.Sale("S1-R").Author
	start := m.Now()
	// r1 takes the order's lease and nothing after it (its withdrawal is lost); r2 and r3 refuse.
	r[0].SetBehaviour(testkit.Behaviour{RefuseEvent: func(e *nostr.Event) string {
		if e.PubKey == author && e.CreatedAt > start {
			return "error: down"
		}
		return ""
	}})
	refuse := testkit.Behaviour{RefuseEvent: func(e *nostr.Event) string {
		if e.PubKey == author {
			return "error: down"
		}
		return ""
	}}
	r[1].SetBehaviour(refuse)
	r[2].SetBehaviour(refuse)
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "S1-R", Sats: 49_000})
	wantCode(t, err, checkout.CodeNetwork)

	// Another order while r1 is slow: r2 and r3, which never got that lease, answer first.
	r[0].SetBehaviour(testkit.Behaviour{QueryDelay: 500 * time.Millisecond})
	r[1].SetBehaviour(testkit.Behaviour{})
	r[2].SetBehaviour(testkit.Behaviour{})
	x, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "S1-X", Sats: 49_000})
	xHolds := err == nil && x.OnchainAddress == lot[0]

	// The first order again while r2 is slow: r1, which holds its lease, answers.
	r[0].SetBehaviour(testkit.Behaviour{})
	r[1].SetBehaviour(testkit.Behaviour{QueryDelay: 500 * time.Millisecond})
	again, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "S1-R", Sats: 49_000})
	// Never both: the address handed out to this order while the other holds it.
	if xHolds && handedOut(t, again, err) == lot[0] {
		t.Error("one address handed out to two orders")
	}
}

func TestS1AnInvoiceWithLightningWhoseDeclarationFailedItsWithdrawalOnOneRelay(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:1], 91)
	r := relaysOf(m)
	author := m.Sale("S1-L").Author
	d := derive.LeaseAddress(m.RegistryKey, lot[0])
	start := m.Now()
	isLease := func(e *nostr.Event) bool { tag, _ := e.DTag(); return tag == d }
	mine := func(e *nostr.Event) bool { return e.PubKey == author }
	refuseWhen := func(f func(e *nostr.Event) bool) testkit.Behaviour {
		return testkit.Behaviour{RefuseEvent: func(e *nostr.Event) string {
			if f(e) {
				return "error: down"
			}
			return ""
		}}
	}
	// r1: the lease and the declaration, not the withdrawal. r2: the lease and the withdrawal,
	// not the declaration. r3: the lease only.
	r[0].SetBehaviour(refuseWhen(func(e *nostr.Event) bool { return mine(e) && isLease(e) && e.CreatedAt > start }))
	r[1].SetBehaviour(refuseWhen(func(e *nostr.Event) bool { return mine(e) && !isLease(e) }))
	r[2].SetBehaviour(refuseWhen(func(e *nostr.Event) bool { return mine(e) && (!isLease(e) || e.CreatedAt > start) }))
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "S1-L", Sats: 49_000})
	wantCode(t, err, checkout.CodeNetwork)

	// Another order while r1 is slow: r2's withdrawal frees the address.
	r[0].SetBehaviour(testkit.Behaviour{QueryDelay: 500 * time.Millisecond})
	r[1].SetBehaviour(testkit.Behaviour{})
	r[2].SetBehaviour(testkit.Behaviour{})
	x, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "S1-X", Sats: 49_000})
	must(t, err)
	xHolds := x.OnchainAddress == lot[0]

	// The first order again while r2 is slow.
	r[0].SetBehaviour(testkit.Behaviour{})
	r[1].SetBehaviour(testkit.Behaviour{QueryDelay: 500 * time.Millisecond})
	again, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "S1-L", Sats: 49_000})
	if xHolds && handedOut(t, again, err) == lot[0] {
		t.Error("one address handed out to two orders")
	}
}

func isPaid(t *testing.T, c *checkout.Client, invoice *checkout.Invoice) bool {
	t.Helper()
	status, err := c.GetStatus(ctx, invoice)
	must(t, err)
	return status.State == checkout.StatePaid
}

func isOrderPaid(err error) bool {
	var e *checkout.Error
	return errors.As(err, &e) && e.Code == checkout.CodeOrderPaid
}

// S2: a transaction evicted, then broadcast again during a later lease, pays two orders.
func TestS2TheFirstOrdersWitnessNamesIt(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:1], 91)
	c := clientOf(t, m)
	p, err := c.CreateInvoice(ctx, checkout.Order{Ref: "S2-P", Sats: 49_000, DurationSeconds: 600})
	must(t, err)
	address := p.OnchainAddress
	// Paid with a very low fee, seen while P's lease runs: P's witness.
	tx := testkit.Payment(testkit.Txid("S2 low fee"), address, 49_000, nil, 0)
	m.Chain.Add(address, tx)
	status, err := c.GetStatus(ctx, p)
	must(t, err)
	if status.State != checkout.StateUnconfirmed {
		t.Fatalf("state %s", status.State)
	}
	// Evicted from the mempools: the explorer lists nothing for the address.
	m.Chain.Drop(address, tx.Txid)
	m.SetNow(p.ExpiresAt + 120 + 301)
	same(t, "expired", statusJSON(t, c, p), jsonv.O("state", "expired", "received", "0"))

	n, err := c.CreateInvoice(ctx, checkout.Order{Ref: "S2-N", Sats: 49_000, DurationSeconds: 600})
	must(t, err)
	// The payer's wallet broadcasts T again during N's lease; it is mined.
	m.Chain.Add(address, tx)
	if n.OnchainAddress == address {
		_, err := c.GetStatus(ctx, n)
		must(t, err)
	}
	m.Chain.Confirm(address, tx.Txid, m.Chain.Height()-5)
	// The payer asks for P again: the shop checks P's earlier invoice.
	_, pAgain := c.CreateInvoice(ctx, checkout.Order{Ref: "S2-P", Sats: 49_000})
	nPaid := n.OnchainAddress == address && isPaid(t, c, n)
	// One transaction, one order.
	if nPaid && isOrderPaid(pAgain) {
		t.Error("one transaction paid two orders")
	}
}

func TestS2NoCheckSawItWhileTheFirstLeaseRanBlockCyphersTime(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:1], 91)
	c := clientOf(t, m)
	p, err := c.CreateInvoice(ctx, checkout.Order{Ref: "S2-Q", Sats: 49_000, DurationSeconds: 600})
	must(t, err)
	address := p.OnchainAddress
	tx := testkit.Payment(testkit.Txid("S2 unseen"), address, 49_000, nil, 0)
	// BlockCypher received it while P's lease ran; no check of P saw it; it is evicted.
	m.Chain.SetReceived(tx.Txid, iso(p.CreatedAt+10))
	m.SetNow(p.ExpiresAt + 120 + 301)
	same(t, "expired", statusJSON(t, c, p), jsonv.O("state", "expired", "received", "0"))

	n, err := c.CreateInvoice(ctx, checkout.Order{Ref: "S2-M", Sats: 49_000, DurationSeconds: 600})
	must(t, err)
	if n.OnchainAddress != address {
		t.Fatalf("leased %q", n.OnchainAddress)
	}
	m.Chain.Add(address, tx)
	status := statusJSON(t, c, n)
	if fieldOf(status, "state") != "unconfirmed" || fieldOf(status, "received") != "49000" {
		t.Errorf("%s", jsonv.Marshal(status))
	}
	m.Chain.Confirm(address, tx.Txid, m.Chain.Height()-5)
	_, pAgain := c.CreateInvoice(ctx, checkout.Order{Ref: "S2-Q", Sats: 49_000})
	if isPaid(t, c, n) && isOrderPaid(pAgain) {
		t.Error("one transaction paid two orders")
	}
}

// S3: the witness of an earlier invoice of the order counts for a later one.
func TestS3AnUnderpaymentOfAnExpiredInvoiceNeverCountsAgainForTheNext(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:1], 91)
	// On-chain only: no rest invoice is asked.
	m.SetPay(pay("maxSendable", 1_000_000))
	c := clientOf(t, m)
	first, err := c.CreateInvoice(ctx, checkout.Order{Ref: "S3-R", Sats: 49_000, DurationSeconds: 600})
	must(t, err)
	address := first.OnchainAddress
	tx := testkit.Payment(testkit.Txid("S3 part"), address, 30_000, nil, 0)
	m.Chain.Add(address, tx)
	status := statusJSON(t, c, first)
	if fieldOf(status, "state") != "open" || fieldOf(status, "received") != "30000" {
		t.Fatalf("%s", jsonv.Marshal(status))
	}
	m.SetNow(first.ExpiresAt + 121)
	// Underpaid for good: the merchant settles the 30 000 sats with the customer.
	same(t, "expired", statusJSON(t, c, first), jsonv.O("state", "expired", "received", "30000"))
	// T leaves the mempool (evicted).
	m.Chain.Drop(address, tx.Txid)
	m.SetNow(first.ExpiresAt + 120 + 301)
	second, err := c.CreateInvoice(ctx, checkout.Order{Ref: "S3-R", Sats: 49_000, DurationSeconds: 600})
	if err != nil {
		// No address for it: nothing can count twice.
		wantCode(t, err, checkout.CodeOnchainUnavailable)
		return
	}
	if second.OnchainAddress != address {
		t.Fatalf("leased %q", second.OnchainAddress)
	}
	// T broadcast again after the second invoice's end: late, whatever its witness.
	m.SetNow(second.ExpiresAt + 3600)
	m.Chain.Add(address, tx)
	same(t, "expired", statusJSON(t, c, second), jsonv.O("state", "expired", "received", "0"))
}

func TestS3AWitnessOfTheEarlierInvoiceMadeAfterTheLaterOneLeasedTheAddress(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:1], 91)
	m.SetPay(pay("maxSendable", 1_000_000))
	c := clientOf(t, m)
	first, err := c.CreateInvoice(ctx, checkout.Order{Ref: "S3-Q", Sats: 49_000, DurationSeconds: 600})
	must(t, err)
	address := first.OnchainAddress
	tx := testkit.Payment(testkit.Txid("S3 unseen part"), address, 30_000, nil, 0)
	// Received by BlockCypher while the first lease ran; no check saw it; then evicted.
	m.Chain.SetReceived(tx.Txid, iso(first.CreatedAt+10))
	// Beyond the reads of leases (7200 s), where the order's own records keep it off the address.
	m.SetNow(first.CreatedAt + 7300)
	same(t, "expired", statusJSON(t, c, first), jsonv.O("state", "expired", "received", "0"))
	second, err := c.CreateInvoice(ctx, checkout.Order{Ref: "S3-Q", Sats: 49_000, DurationSeconds: 600})
	must(t, err)
	if second.OnchainAddress != address {
		t.Fatalf("leased %q", second.OnchainAddress)
	}
	// T comes back after the second invoice's end; the first invoice is checked first.
	m.SetNow(second.ExpiresAt + 3600)
	m.Chain.Add(address, tx)
	counted := 0
	for _, invoice := range []*checkout.Invoice{first, second} {
		if fieldOf(statusJSON(t, c, invoice), "received") == "30000" {
			counted++
		}
	}
	// T was received once: it counts for one invoice at most.
	if counted > 1 {
		t.Errorf("counted %d times", counted)
	}
}

// S6: a hostile relay hides a lease from the majority that reads. The confirmation reads on every
// relay (SPEC.md §13): r2, late but in time, shows the lease.
func TestS6AHostileRelayHidesALeaseTheReadOnEveryRelayStillSees(t *testing.T) {
	m := securityMerchant(t)
	m.PublishLot(lot[:1], 91)
	r := relaysOf(m)
	lease := leaseBy(t, m, "S6-A", lot[0], m.Now(), 900, nil)
	// r1 refused it.
	r[1].Take(lease)
	r[2].Take(lease)
	r[1].SetBehaviour(testkit.Behaviour{Hide: func(event any) bool { id, _ := jsonv.Field(event, "id"); return id == lease.ID }})
	// r2 answers its reads 1.5 s late, well within the timeout (8 s): H and r1 answer first,
	// whatever the load of the machine running the test.
	r[2].SetBehaviour(testkit.Behaviour{QueryDelay: 1500 * time.Millisecond})
	if got := onchainAddressOf(t, clientOf(t, m, checkout.WithRelayTimeout(8*time.Second)), "S6-B"); got != "" {
		t.Error("leased an address a relay hid the lease of")
	}
}

// copyOf is event with another tag and date, its id recomputed, its signature left as it is.
func copyOf(e *nostr.Event, index int, filler string) *jsonv.Object {
	tags := append(append([][]string{}, e.Tags...), []string{"x", filler})
	createdAt := e.CreatedAt - 1 - int64(index)
	hash := sha256.Sum256(jsonv.Marshal([]any{0, e.PubKey, createdAt, e.Kind, tags, e.Content}))
	return jsonv.O("id", hex.EncodeToString(hash[:]), "pubkey", e.PubKey, "created_at", createdAt, "kind", e.Kind, "tags", tags,
		"content", e.Content, "sig", e.Sig).Object()
}

// Copies of a record under ids of their own: a read keeps none of them whole.
func TestOfACopyOfALeaseOnlyItsIDAuthorDateAndSignatureAreKept(t *testing.T) {
	m := securityMerchant(t)
	lease := leaseBy(t, m, "OTHER", lot[0], m.Now()-60, 900, nil)
	everywhere(m, lease)
	filler := strings.Repeat("y", 50_000)
	// Relays that keep them all (anybody can write at a lease's address): read on any majority.
	for _, r := range relaysOf(m) {
		for i := 0; i < 200; i++ {
			r.Store(copyOf(lease, i, filler))
		}
	}
	pool := relay.NewPool(ctx, relay.Options{Dialer: m.Net.Dialer(), Timeout: 10 * time.Second, Now: m.Now, Random: rand.Reader})
	defer pool.Close()
	read, err := onchain.ReadLeases(ctx, pool, m.RegistryKey, lot[:1], m.KeyRelays, m.Now(), seeded(1), false)
	must(t, err)
	records := read.Records[lot[0]]
	// Every copy opens as the lease does (its id is its hash, its content decrypts).
	if len(records) != 201 {
		t.Fatalf("%d records", len(records))
	}
	// A record keeps a LeaseEvent: its id, author, date and signature, nothing else.
	var signed []string
	for _, r := range records {
		if r.Outside.Sealed != "" {
			t.Fatal("a record keeps the sealed inside")
		}
		if r.Signed() {
			signed = append(signed, r.Event.ID)
		}
	}
	// Only the lease is signed: the walk still finds the address leased.
	same(t, "signed", signed, []string{lease.ID})
	if !onchain.IsLeased(records, m.Now()) {
		t.Error("not leased")
	}
}

func TestACopyOfASignedRecordIsNotKept(t *testing.T) {
	m := securityMerchant(t)
	_, encryptionKey := derive.ChannelKeys(m.IntegrationKey)
	keep := nostr.RecordsThatOpen(func(_, author string) []byte {
		if author == m.Team {
			return encryptionKey
		}
		return nil
	}, nostr.NewSigMemo())
	channel := m.ChannelEvent(m.ChannelValue(100), 100, nil)
	if _, kept := keep(testkit.EventValue(channel)); !kept {
		t.Error("the channel not kept")
	}
	if _, kept := keep(copyOf(channel, 0, strings.Repeat("z", 1000))); kept {
		t.Error("a copy kept")
	}
	unsigned := testkit.EventValue(channel).(*jsonv.Object)
	unsigned.Set("sig", strings.Repeat("00", 64))
	if _, kept := keep(unsigned); kept {
		t.Error("an unsigned copy kept")
	}
	// Another key's: not opened, not kept.
	other := testkit.EventValue(channel).(*jsonv.Object)
	other.Set("pubkey", strings.Repeat("ab", 32))
	if _, kept := keep(other); kept {
		t.Error("another key's kept")
	}
}
