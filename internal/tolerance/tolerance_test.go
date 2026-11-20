package tolerance

import (
	"strconv"
	"testing"

	tv "github.com/sattleme/checkout-go/internal/testvec"
)

func TestToleranceVectors(t *testing.T) {
	file := tv.Load(t, "onchain-tolerances.json")
	values := tv.Objs(t, file, "values")
	for _, c := range values {
		if got := Of(tv.Get(c, "value")); got != tv.Int(t, c, "expected") {
			t.Errorf("%s: %d", tv.Str(t, c, "name"), got)
		}
	}
	accepted := tv.Objs(t, file, "accepted")
	for _, c := range accepted {
		sats, _ := strconv.ParseInt(tv.Str(t, c, "sats"), 10, 64)
		if got := strconv.FormatInt(LeastAccepted(sats, tv.Int(t, c, "tolerance")), 10); got != tv.Str(t, c, "expected") {
			t.Errorf("%s at %d: %s", tv.Str(t, c, "sats"), tv.Int(t, c, "tolerance"), got)
		}
	}
	due := tv.Objs(t, file, "due")
	for _, c := range due {
		sats, _ := strconv.ParseInt(tv.Str(t, c, "sats"), 10, 64)
		paid, _ := strconv.ParseInt(tv.Str(t, c, "paidByRests"), 10, 64)
		// The received of the merchant's acceptance that counts (absent: none, 0).
		var accepted int64
		if c.Has("merchantAccepted") {
			accepted, _ = strconv.ParseInt(tv.Str(t, c, "merchantAccepted"), 10, 64)
		}
		if got := strconv.FormatInt(DueOnchain(sats, tv.Int(t, c, "tolerance"), paid, accepted), 10); got != tv.Str(t, c, "expected") {
			t.Errorf("%s at %d, %s paid by rests, accepted %d: %s", tv.Str(t, c, "sats"), tv.Int(t, c, "tolerance"), tv.Str(t, c, "paidByRests"), accepted, got)
		}
	}
	t.Logf("%d values, %d accepted, %d due", len(values), len(accepted), len(due))
}
