package checkout_test

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/onchain"
	"github.com/sattleme/checkout-go/internal/testkit"
)

// What the on-chain tests share: the lot, and the records the relays hold, as the merchant's app
// reads them.

var lot = []string{testkit.DepositAddress("1"), testkit.DepositAddress("2"), testkit.DepositAddress("3")}

// onchainMerchant is a merchant whose channel names its relays and a web app for checkout links,
// its lot of addresses published at 90.
func onchainMerchant(t *testing.T) *testkit.Merchant {
	t.Helper()
	m := testkit.NewMerchant()
	m.Start(nil, nil)
	t.Cleanup(m.Stop)
	m.PublishChannel(m.ChannelValue(100, jsonv.Member{Name: "relays", Value: m.KeyRelays}, jsonv.Member{Name: "checkout", Value: jsonv.O("origin", origin)}), 100)
	m.PublishLot(lot, 90)
	return m
}

// held is what the first relay holds now.
func held(m *testkit.Merchant) []any { return m.Net.Get(m.KeyRelays[0]).Events() }

// leaseSeen is a record of a lease's address a relay received, and its whole event.
type leaseSeen struct {
	record *onchain.LeaseRecord
	event  *nostr.Event
}

// leasesOf are the leases and withdrawals of address the first relay received, opened by the
// merchant.
func leasesOf(m *testkit.Merchant, address string) []leaseSeen {
	return leasesOn(m, m.KeyRelays[0], address)
}

func leasesOn(m *testkit.Merchant, relay, address string) []leaseSeen {
	d := derive.LeaseAddress(m.RegistryKey, address)
	var out []leaseSeen
	for _, e := range m.Net.Get(relay).Received() {
		if record, ok := onchain.OpenLeaseRecord(testkit.EventValue(e), m.RegistryKey, d); ok {
			out = append(out, leaseSeen{record: record, event: e})
		}
	}
	return out
}

// kindsOf are the kinds of records.
func kindsOf(records []leaseSeen) []string {
	out := []string{}
	for _, r := range records {
		out = append(out, r.record.Outside.Kind)
	}
	return out
}

// openAsMerchant is the inside of a lease, as the merchant's app opens it (registry key and
// reading key), its outside's end first.
func openAsMerchant(t *testing.T, m *testkit.Merchant, e *nostr.Event) *jsonv.Object {
	t.Helper()
	d, _ := e.DTag()
	plaintext, err := nip44.Decrypt(e.Content, derive.OuterKey(m.RegistryKey, d, e.PubKey))
	must(t, err)
	outside, err := jsonv.ParseString(plaintext)
	must(t, err)
	if typ := fieldOf(outside, "t"); typ != "registry-onchain" {
		t.Fatalf("type %v", typ)
	}
	v := fieldOf(outside, "v")
	author, _ := hex.DecodeString(e.PubKey)
	key, err := nip44.ConversationKey(m.ReaderSecret, author)
	must(t, err)
	insideText, err := nip44.Decrypt(fieldOf(v, "sealed").(string), key)
	must(t, err)
	inside, err := jsonv.ParseString(insideText)
	must(t, err)
	out := jsonv.NewObject()
	out.Set("end", fieldOf(v, "end"))
	for _, name := range inside.(*jsonv.Object).Keys() {
		out.Set(name, fieldOf(inside, name))
	}
	return out
}

// witnessSeen is a witness the relays hold, and when it was seen.
type witnessSeen struct {
	onchain.Witness
	SeenAt int64
}

// witnessesOf are the witnesses the first relay holds, as the merchant's app reads them.
func witnessesOf(m *testkit.Merchant) []witnessSeen {
	seen := onchain.SeenOf(m.IntegrationKey)
	var out []witnessSeen
	for _, v := range held(m) {
		if opened, ok := onchain.OpenWitness(v, seen, nil); ok && !opened.Newer {
			out = append(out, witnessSeen{Witness: opened.Witness, SeenAt: opened.Event.CreatedAt})
		}
	}
	return out
}

// iso is a time as BlockCypher writes it.
func iso(seconds int64) string {
	return time.Unix(seconds, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// coinOf is the coin a test payment of id spends.
func coinOf(id string) string { return testkit.Txid("coin of "+id) + ":0" }

// coinsOf are the coins tx spends.
func coinsOf(tx *testkit.EsploraTx) []string {
	var out []string
	for _, in := range tx.Vin {
		out = append(out, in.Txid+":"+itoa(in.Vout))
	}
	return out
}

func itoa(n int64) string { return jsonv.NumberString(float64(n)) }
