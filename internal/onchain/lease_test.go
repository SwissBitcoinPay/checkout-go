package onchain

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"strconv"
	"testing"
	"time"

	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/relay"
	"github.com/sattleme/checkout-go/internal/testkit"
)

// The reads of leases (SPEC.md §13, "Reads on every relay"; §31, "Leasing an address").

var leaseRegistryKey = bytes.Repeat([]byte{3}, 32)

// throwaway is a record anybody writes at a registry address: a throwaway key signs it.
func throwaway(t *testing.T, d string, createdAt int64, seed int) *nostr.Event {
	t.Helper()
	secret := sha256.Sum256([]byte("throwaway " + strconv.Itoa(seed)))
	e, err := nostr.Sign(nostr.Template{CreatedAt: createdAt, Kind: proto.RecordKind, Tags: [][]string{{"d", d}}, Content: "x"},
		secret[:], rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func leasePool(t *testing.T, n *testkit.Net, timeout time.Duration) *relay.Pool {
	t.Helper()
	p := relay.NewPool(context.Background(), relay.Options{Dialer: n.Dialer(), Timeout: timeout, Now: func() int64 { return 1_791_378_000 }, Random: rand.Reader})
	t.Cleanup(p.Close)
	return p
}

// A relay still reading when a read on every relay goes on did not answer: its pages are kept as
// events, and that is all. One relay read (incompletely), one failed, one still paging: fewer than
// a majority answered.
func TestARelayStillReadingCountsForNoAnswer(t *testing.T) {
	n := testkit.NewNet()
	t.Cleanup(n.Close)
	address := testkit.DepositAddress("1")
	d := derive.LeaseAddress(leaseRegistryKey, address)
	const now = 1_791_378_000
	// r1 holds 101 records of one second at the address: its read ends, incomplete, once r3 sent
	// its first page (each of its two pages 1 s late, well within the timeout of 5 s: r3's first
	// page comes first, whatever the load of the machine running the test).
	r1 := n.Add("wss://r1.example", testkit.Behaviour{QueryDelay: time.Second})
	for i := 0; i < 101; i++ {
		r1.Take(throwaway(t, d, now-10, i))
	}
	// r2 fails at once.
	n.Add("wss://r2.example", testkit.Behaviour{Raw: func(message []any, send func(string)) bool {
		if len(message) > 1 && message[0] == "REQ" {
			send(string(jsonv.Marshal([]any{"CLOSED", message[1], "error: down"})))
			return true
		}
		return false
	}})
	// r3 sends a full first page (a lease, which a read keeps, and 99 others), then never answers
	// the next.
	r3 := n.Add("wss://r3.example", testkit.Behaviour{MuteQuery: func(f *jsonv.Object) bool { return f.Has("until") }})
	m := testkit.NewMerchant()
	m.RegistryKey = leaseRegistryKey
	lease, err := LeaseEventOf(m.Sale("R-1"), LeaseTerms{Address: address, Sats: 1000, CreatedAt: now, ExpiresAt: now + 900,
		Price: declaration.Price{Minor: 1000, Currency: "SAT"}, Note: "R-1"}, now, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r3.Take(lease)
	for i := 0; i < 99; i++ {
		r3.Take(throwaway(t, d, now-20-int64(i), 1000+i))
	}
	relays := []string{"wss://r1.example", "wss://r2.example", "wss://r3.example"}
	p := leasePool(t, n, 5*time.Second)
	read, err := ReadLeases(context.Background(), p, leaseRegistryKey, []string{address}, relays, now, rand.Reader, true)
	if err != nil {
		t.Fatal(err)
	}
	if read.Answered || read.Complete {
		t.Errorf("answered %v, complete %v: r3 was still reading", read.Answered, read.Complete)
	}
	// What r3 sent is kept all the same.
	if records := read.Records[address]; len(records) != 1 || records[0].Event.ID != lease.ID {
		t.Errorf("%d records", len(records))
	}
}

// Absent is not null (SPEC.md §31, "Leases"): an order paid on-chain only has "invoice": null and
// "verifyUrl": null in its lease; a lease without them does not open as the sale's.
func TestALeaseWithoutItsInvoiceMembersDoesNotOpenAsTheSales(t *testing.T) {
	m := testkit.NewMerchant()
	m.RegistryKey = leaseRegistryKey
	sale := m.Sale("R-2")
	address := testkit.DepositAddress("2")
	const now = 1_791_378_000
	sealed := func(drop string) any {
		var inside jsonv.Members
		for _, member := range LeaseInside(sale, LeaseTerms{Address: address, Sats: 1000, CreatedAt: now, ExpiresAt: now + 900,
			Price: declaration.Price{Minor: 1000, Currency: "SAT"}, Note: "R-2"}) {
			if member.Name != drop {
				inside = append(inside, member)
			}
		}
		text, err := nip44.Encrypt(string(jsonv.Marshal(inside)), sale.InnerKey, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		e, err := sealSaleRecord(sale, address, jsonv.O("version", proto.LeaseVersion, "end", now+900+proto.GraceSeconds, "sealed", text), now, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return testkit.EventValue(e)
	}
	record, ok := OpenSaleLease(sealed(""), sale, nil)
	if !ok || record.Kind != SaleLease || record.Lease.Terms.Invoice != "" || record.Lease.Terms.Address != address {
		t.Fatalf("an order paid on-chain only: %+v, %v", record, ok)
	}
	for _, drop := range []string{"invoice", "verifyUrl"} {
		if record, ok := OpenSaleLease(sealed(drop), sale, nil); ok {
			t.Errorf("opened without %s: %+v", drop, record)
		}
	}
}
