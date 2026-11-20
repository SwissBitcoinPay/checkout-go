package onchain

import (
	"math"
	"testing"
	"time"

	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/testkit"
)

func TestBlockCyphersTimesAreReadAsItWritesThemAndNothingElse(t *testing.T) {
	ms := func(year int, month time.Month, day, hour, minute, second, milli int) int64 {
		return time.Date(year, month, day, hour, minute, second, milli*int(time.Millisecond), time.UTC).UnixMilli()
	}
	for text, want := range map[string]int64{
		"2026-10-10T13:05:12.345Z": ms(2026, 10, 10, 13, 5, 12, 345),
		"2026-10-10T13:05:12.91Z":  ms(2026, 10, 10, 13, 5, 12, 910),
		"2026-10-10T13:05:12Z":     ms(2026, 10, 10, 13, 5, 12, 0),
		// Dropped, never rounded (SPEC.md §31, "Explorers").
		"2026-10-10T13:05:12.345999999Z": ms(2026, 10, 10, 13, 5, 12, 345),
		"2028-02-29T00:00:00Z":           ms(2028, 2, 29, 0, 0, 0, 0),
	} {
		if got, ok := BlockCypherMilliseconds(text); !ok || got != want {
			t.Errorf("%s: %d, want %d", text, got, want)
		}
	}
	// Year 75, not 1975: before 1970, never a time in a lease.
	if got, ok := BlockCypherMilliseconds("0075-01-01T00:00:00Z"); !ok || got >= 0 {
		t.Errorf("year 75: %d", got)
	}
	for _, text := range []string{
		"2026-02-30T00:00:00Z",
		"2027-02-29T00:00:00Z",
		"2026-13-01T00:00:00Z",
		"2026-00-01T00:00:00Z",
		"2026-10-00T00:00:00Z",
		"2026-10-10T24:00:00Z",
		"2026-12-31T23:59:60Z",
		"2026-10-10 13:05:12Z",
		"2026-10-10T13:05:12",
		"2026-10-10t13:05:12z",
		"2026-10-10T15:05:12+02:00",
		"2026-10-10T13:05:12.Z",
		"2026-10-10T13:05:12.0000000000Z",
		" 2026-10-10T13:05:12Z",
		"2026-10-10T13:05:12Z\n",
	} {
		if got, ok := BlockCypherMilliseconds(text); ok {
			t.Errorf("%q read as %d", text, got)
		}
	}
}

// Absent is not null (SPEC.md §31, "Explorers"): an output's scriptpubkey_address is a string when
// present (an output without an address has none); null, or anything else, is a failure.
func TestAnOutputsAddressIsAStringWhenPresent(t *testing.T) {
	// The transaction as an explorer's answer reads.
	tx := func(output jsonv.Members) any {
		v, err := jsonv.Parse(jsonv.Marshal(jsonv.O(
			"txid", testkit.Txid("outputs"),
			"vin", []any{jsonv.O("txid", testkit.Txid("coin"), "vout", 0)},
			"vout", []any{output},
			"status", jsonv.O("confirmed", false),
		)))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	address := testkit.DepositAddress("1")
	if parsed, ok := ParseTransaction(tx(jsonv.O("value", 1000, "scriptpubkey_address", address))); !ok || parsed.Outputs[0].Address != address {
		t.Errorf("with an address: %+v, %v", parsed, ok)
	}
	if parsed, ok := ParseTransaction(tx(jsonv.O("value", 1000))); !ok || parsed.Outputs[0].Address != "" {
		t.Errorf("without one: %+v, %v", parsed, ok)
	}
	for _, bad := range []any{nil, 1, false, []any{}} {
		if parsed, ok := ParseTransaction(tx(jsonv.O("value", 1000, "scriptpubkey_address", bad))); ok {
			t.Errorf("scriptpubkey_address %v: %+v", bad, parsed)
		}
	}
}

// SPEC.md §31, Explorers: a transaction whose outputs together hold more than every bitcoin there
// will ever be (2 100 000 000 000 000 sats) is none, whichever addresses they pay: the explorer's
// answer is a failure. Up to that sum, it is one.
func TestATransactionWhoseOutputsHoldMoreThanEveryBitcoinIsNone(t *testing.T) {
	address, other := testkit.DepositAddress("1"), testkit.DepositAddress("2")
	answer := func(outputs ...any) any {
		v, err := jsonv.Parse(jsonv.Marshal(jsonv.O(
			"txid", testkit.Txid("outputs"),
			"vin", []any{jsonv.O("txid", testkit.Txid("coin"), "vout", 0)},
			"vout", outputs,
			"status", jsonv.O("confirmed", false),
		)))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	output := func(sats int64, to string) any { return jsonv.O("value", sats, "scriptpubkey_address", to) }
	for _, c := range []struct {
		name    string
		outputs []any
		paid    int64
	}{
		{"every bitcoin, to the address", []any{output(proto.MaxSats, address)}, proto.MaxSats},
		{"every bitcoin, in two outputs", []any{output(proto.MaxSats-1, address), output(1, other)}, proto.MaxSats - 1},
	} {
		tx, ok := ParseTransaction(answer(c.outputs...))
		if !ok || PaysTo(tx, address) != c.paid {
			t.Errorf("%s: %+v, %v", c.name, tx, ok)
		}
	}
	many := make([]any, 2000)
	for i := range many {
		many[i] = output(int64(jsonv.MaxSafeInteger), address)
	}
	for name, outputs := range map[string][]any{
		"one output more":                  {output(proto.MaxSats+1, address)},
		"two outputs, one to another":      {output(proto.MaxSats-1, address), output(2, other)},
		"an output without an address":     {output(proto.MaxSats, address), jsonv.O("value", 1)},
		"2000 outputs of 2^53 - 1 sats":    many,
		"2^53 - 1 sats to another address": {output(int64(jsonv.MaxSafeInteger), other), output(0, address)},
	} {
		if tx, ok := ParseTransaction(answer(outputs...)); ok {
			t.Errorf("%s: %d outputs, read as a transaction", name, len(tx.Outputs))
		}
	}
}

// SPEC.md §7: an amount is never wrapped. What a transaction pays an address (of outputs an
// explorer's answer could not hold together, now: two thousand of 2^53 − 1 sats) stops at
// math.MaxInt64, never below 0, and so does every sum of such amounts.
func TestWhatATransactionPaysIsNeverWrapped(t *testing.T) {
	address := testkit.DepositAddress("1")
	tx := &ExplorerTx{Txid: testkit.Txid("many outputs"), Outputs: make([]Output, 2000)}
	for i := range tx.Outputs {
		tx.Outputs[i] = Output{Sats: int64(jsonv.MaxSafeInteger), Address: address}
	}
	if got := PaysTo(tx, address); got != math.MaxInt64 {
		t.Errorf("pays %d", got)
	}
	for _, c := range [][3]int64{{1, 2, 3}, {0, 0, 0}, {math.MaxInt64, 1, math.MaxInt64}, {math.MaxInt64 - 1, 1, math.MaxInt64}, {math.MaxInt64, math.MaxInt64, math.MaxInt64}} {
		if got := AddSats(c[0], c[1]); got != c[2] {
			t.Errorf("%d + %d: %d", c[0], c[1], got)
		}
	}
}
