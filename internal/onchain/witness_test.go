package onchain

import (
	"bytes"
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/testkit"
)

// The witnesses of an invoice, as a check reads them from what relays sent (orderWitnessesOf).

var (
	witnessSeen      = SeenOf(bytes.Repeat([]byte{2}, 32))
	witnessHere      = testkit.DepositAddress("this")
	witnessElsewhere = testkit.DepositAddress("elsewhere")
	witnessSale      = strings.Repeat("ab", 32)
)

const witnessExpiresAt = 1_791_379_180

func witnessOf(t *testing.T, of, tx, sale string) any {
	t.Helper()
	w := Witness{Address: of, Txid: testkit.Txid(tx), Sale: sale, Sats: 1000, Inputs: []string{}}
	if sale != "" {
		w.ExpiresAt = witnessExpiresAt
	}
	e, err := WitnessEvent(witnessSeen, w, 1_791_378_400, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testkit.EventValue(e)
}

// newerWitness is a record of a newer format (its content "#") at the witness address of tx at
// of, signed by the seen secret or not.
func newerWitness(t *testing.T, of, tx string, tags [][]string, sign bool) any {
	t.Helper()
	e, err := nostr.Sign(nostr.Template{
		CreatedAt: 1_791_378_400,
		Kind:      proto.RecordKind,
		Tags:      append([][]string{{"d", WitnessAddress(witnessSeen.AddressKey, of, testkit.Txid(tx), witnessSale)}}, tags...),
		Content:   "#",
	}, witnessSeen.SecretKey, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v := testkit.EventValue(e).(*jsonv.Object)
	if !sign {
		v.Set("sig", strings.Repeat("00", 64))
	}
	return v
}

func readWitnesses(events ...any) *OrderWitnesses {
	return OrderWitnessesOf(events, witnessSeen, witnessHere, witnessSale, witnessExpiresAt, nil)
}

func keysOf(m map[string]SaleWitness) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestTheWitnessesOfThisReadAreRead(t *testing.T) {
	w := readWitnesses(witnessOf(t, witnessHere, "mine", witnessSale), witnessOf(t, witnessHere, "none", ""), witnessOf(t, witnessHere, "another's", strings.Repeat("cd", 32)))
	if got := keysOf(w.Sale); len(got) != 1 || got[0] != testkit.Txid("mine") {
		t.Errorf("of the sale: %v", got)
	}
	if len(w.None) != 1 || !w.None[testkit.Txid("none")] {
		t.Errorf("of none: %v", w.None)
	}
	if got := keysOf(w.Others); len(got) != 1 || got[0] != testkit.Txid("another's") {
		t.Errorf("of others: %v", got)
	}
}

// A transaction paying two orders pays each its part: the witness at another address is of no
// invoice of this read.
func TestAWitnessAtAnotherAddressIsIgnored(t *testing.T) {
	w := readWitnesses(witnessOf(t, witnessElsewhere, "batch", strings.Repeat("cd", 32)))
	if len(w.Sale)+len(w.Others)+len(w.None) != 0 {
		t.Errorf("%+v", w)
	}
}

func TestAWitnessOfANewerFormatIsKeptOnlyWithTheReadsTagsAndTheAuthorsSignature(t *testing.T) {
	tag := [][]string{{"o", AddressTag(witnessSeen.AddressKey, witnessHere)}}
	otherTag := [][]string{{"o", AddressTag(witnessSeen.AddressKey, witnessElsewhere)}}
	if n := len(readWitnesses(newerWitness(t, witnessHere, "t", tag, true)).Newer); n != 1 {
		t.Errorf("%d newer", n)
	}
	// Another address's, which a relay sends against the filter: of no invoice of this read.
	if n := len(readWitnesses(newerWitness(t, witnessHere, "t", otherTag, true)).Newer); n != 0 {
		t.Errorf("%d newer of another address", n)
	}
	// Without the seen secret's signature: nothing (SPEC.md §12), not an undecided payment.
	if n := len(readWitnesses(newerWitness(t, witnessHere, "t", tag, false)).Newer); n != 0 {
		t.Errorf("%d newer without a signature", n)
	}
}

// Of one witness address, the newest that opens counts.
func TestTheNewestWitnessOfAnAddressCounts(t *testing.T) {
	older := Witness{Address: witnessHere, Txid: testkit.Txid("x"), Sale: witnessSale, Sats: 1000, Inputs: []string{}, Replacement: true, ExpiresAt: witnessExpiresAt}
	newer := older
	newer.Replacement = false
	a, err := WitnessEvent(witnessSeen, older, 100, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, err := WitnessEvent(witnessSeen, newer, 200, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range [][]any{{testkit.EventValue(a), testkit.EventValue(b)}, {testkit.EventValue(b), testkit.EventValue(a)}} {
		w := readWitnesses(order...)
		if got := w.Sale[testkit.Txid("x")]; got.Replacement || got.SeenAt != 200 {
			t.Errorf("%+v", got)
		}
	}
}

// SPEC.md §31, "Leasing an address", step 3: an address is named by any event of the seen author
// that checks out and carries the address's tag as an o tag, whether it opens as a witness or not;
// one that does not check out names nothing.
func TestAnyEventOfTheSeenAuthorWithAnAddressTagNamesTheAddress(t *testing.T) {
	n := testkit.NewNet()
	t.Cleanup(n.Close)
	r := n.Add("wss://r.example", testkit.Behaviour{})
	tagOf := func(address string) [][]string { return [][]string{{"o", AddressTag(witnessSeen.AddressKey, address)}} }
	// No witness: no d tag, a content that opens to nothing.
	named, err := nostr.Sign(nostr.Template{CreatedAt: 1_791_378_400, Kind: proto.RecordKind, Tags: tagOf(witnessHere), Content: "not a witness"}, witnessSeen.SecretKey, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r.Take(named)
	forged, err := nostr.Sign(nostr.Template{CreatedAt: 1_791_378_400, Kind: proto.RecordKind, Tags: tagOf(witnessElsewhere), Content: "x"}, witnessSeen.SecretKey, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	value := testkit.EventValue(forged).(*jsonv.Object)
	value.Set("sig", strings.Repeat("00", 64))
	r.Store(value)
	p := leasePool(t, n, 2*time.Second)
	got, err := ReadNamedAddresses(context.Background(), p, witnessSeen, []string{witnessHere, witnessElsewhere}, []string{"wss://r.example"}, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if !got[witnessHere] || got[witnessElsewhere] {
		t.Errorf("named %v", got)
	}
}

// Absent is not null (SPEC.md §31, "Witnesses"): a witness of none has "sale": null; one without
// the member is no witness.
func TestAWitnessWithoutItsSaleMemberDoesNotOpen(t *testing.T) {
	tx := testkit.Txid("no sale")
	seal := func(withSale bool) any {
		value := jsonv.O("version", proto.WitnessVersion, "address", witnessHere, "txid", tx)
		if withSale {
			value = append(value, jsonv.Member{Name: "sale", Value: nil})
		}
		value = append(value, jsonv.O("sats", 1000, "inputs", []string{}, "replacement", false, "expiresAt", nil)...)
		e, err := nostr.Seal(nostr.SealOptions{
			Type: proto.WitnessRecordType, Value: value, Signer: witnessSeen.SecretKey, EncryptionKey: witnessSeen.EncryptionKey,
			Address: WitnessAddress(witnessSeen.AddressKey, witnessHere, tx, ""), CreatedAt: 1_791_378_400,
			Tags: [][]string{
				{proto.OrderTag, OrderTag(witnessSeen.AddressKey, witnessHere, "")},
				{proto.OrderTag, AddressTag(witnessSeen.AddressKey, witnessHere)},
			},
			Random: rand.Reader,
		})
		if err != nil {
			t.Fatal(err)
		}
		return testkit.EventValue(e)
	}
	if opened, ok := OpenWitness(seal(true), witnessSeen, nil); !ok || opened.Newer || opened.Witness.Sale != "" || opened.Witness.Txid != tx {
		t.Fatalf("a witness of none: %+v, %v", opened, ok)
	}
	if opened, ok := OpenWitness(seal(false), witnessSeen, nil); ok {
		t.Errorf("opened without its sale: %+v", opened)
	}
	if read := readWitnesses(seal(false)); len(read.None) != 0 {
		t.Errorf("read as none: %v", read.None)
	}
}
