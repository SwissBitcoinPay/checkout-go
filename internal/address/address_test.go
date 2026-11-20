package address

import (
	"strconv"
	"testing"

	"github.com/sattleme/checkout-go/internal/derive"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

func TestDepositAddressVectors(t *testing.T) {
	file := tv.Load(t, "onchain-addresses.json")
	addresses := tv.Objs(t, file, "addresses")
	for _, c := range addresses {
		if got := IsDeposit(tv.Str(t, c, "address")); got != (tv.Get(c, "valid") == true) {
			t.Errorf("%s: %v", tv.Str(t, c, "name"), got)
		}
	}
	registry := tv.Obj(t, file, "registry")
	key := unhex(t, tv.Str(t, registry, "registryKey"))
	cases := tv.Objs(t, registry, "cases")
	for _, c := range cases {
		if got := derive.LeaseAddress(key, tv.Str(t, c, "address")); got != tv.Str(t, c, "registryAddress") {
			t.Errorf("%s: %s", tv.Str(t, c, "address"), got)
		}
	}
	uris := tv.Objs(t, file, "uris")
	for _, c := range uris {
		sats, _ := strconv.ParseInt(tv.Str(t, c, "sats"), 10, 64)
		deposit, _ := tv.Get(c, "address").(string)
		bolt11, _ := tv.Get(c, "bolt11").(string)
		if got := PaymentURI(deposit, sats, bolt11); got != tv.Str(t, c, "uri") {
			t.Errorf("%s: %s", tv.Str(t, c, "name"), got)
		}
	}
	amounts := tv.Objs(t, file, "amounts")
	for _, c := range amounts {
		sats, _ := strconv.ParseInt(tv.Str(t, c, "sats"), 10, 64)
		if got := BTCAmount(sats); got != tv.Str(t, c, "btc") {
			t.Errorf("%d sats: %s", sats, got)
		}
	}
	t.Logf("%d addresses, %d registry, %d uris, %d amounts", len(addresses), len(cases), len(uris), len(amounts))
	if IsDeposit(nil) || IsDeposit(1.0) {
		t.Error("not a string")
	}
}
