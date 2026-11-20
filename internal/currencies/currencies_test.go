package currencies

import (
	"testing"

	"github.com/sattleme/checkout-go/internal/jsonv"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// The table is spec/currencies.json's, code for code.
func TestTableIsTheSpecifications(t *testing.T) {
	file := tv.Load(t, "../currencies.json")
	spec := tv.Obj(t, file, "currencies")
	table := Table()
	if spec.Len() != len(table) {
		t.Errorf("%d currencies, the specification has %d", len(table), spec.Len())
	}
	for _, code := range spec.Keys() {
		v, _ := spec.Get(code)
		want, _ := jsonv.AsSafeInteger(v)
		if got, ok := Digits(code); !ok || int64(got) != want {
			t.Errorf("%s: %d, %v; want %d", code, got, ok, want)
		}
	}
	for _, code := range []string{"chf", "Chf", "XAU", "XXX", "SAT", "BTC", ""} {
		if _, ok := Digits(code); ok {
			t.Errorf("%q is a currency", code)
		}
	}
}
