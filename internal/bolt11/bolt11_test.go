package bolt11

import (
	"strconv"
	"strings"
	"testing"

	"github.com/sattleme/checkout-go/internal/jsonv"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// fields writes a decoded invoice as the vectors do (amounts in millisatoshis, as strings).
func fields(inv *Invoice) any {
	if inv == nil {
		return nil
	}
	var amount, description any
	if inv.HasAmount {
		amount = strconv.FormatInt(inv.AmountMsat, 10)
	}
	if inv.DescriptionHash != "" {
		description = inv.DescriptionHash
	}
	return jsonv.Members{
		{Name: "network", Value: inv.Network},
		{Name: "amountMsat", Value: amount},
		{Name: "timestamp", Value: inv.Timestamp},
		{Name: "expirySeconds", Value: inv.ExpirySeconds},
		{Name: "paymentHash", Value: inv.PaymentHash},
		{Name: "descriptionHash", Value: description},
	}
}

func TestBolt11Vectors(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "bolt11.json"), "cases")
	for _, c := range cases {
		decoded, ok := Decode(tv.Str(t, c, "invoice"))
		if !ok {
			decoded = nil
		}
		got, want := string(jsonv.Marshal(fields(decoded))), string(jsonv.Marshal(tv.Get(c, "expected")))
		if got != want {
			t.Errorf("%s: got %s, want %s", tv.Str(t, c, "name"), got, want)
		}
	}
	t.Logf("%d cases", len(cases))
}

// Case is ASCII's alone (SPEC.md §6): an invoice in upper case holding U+212A KELVIN SIGN, which
// Unicode case mapping lowers to "k", is not bech32.
func TestKelvinSignIsNotK(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "bolt11.json"), "cases")
	upper := tv.Str(t, cases[1], "invoice")
	if _, ok := Decode(upper); !ok {
		t.Fatal("the upper case invoice does not decode")
	}
	i := strings.IndexByte(upper, 'K')
	if i < 0 {
		t.Fatal("no K in the invoice")
	}
	kelvin := upper[:i] + "\xe2\x84\xaa" + upper[i+1:]
	if _, ok := Decode(kelvin); ok {
		t.Error("an invoice with a Kelvin sign decodes")
	}
	if _, ok := Decode(strings.ToLower(upper[:i]) + "k" + strings.ToLower(upper[i+1:])); !ok {
		t.Error("the lowercase invoice does not decode")
	}
}
