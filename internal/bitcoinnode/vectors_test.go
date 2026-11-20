package bitcoinnode

import (
	"testing"

	"github.com/sattleme/checkout-go/internal/jsonv"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// bitcoin-nodes.json's urls, through the check of a node URL and the URLs the node is read at
// (PORTING.md, step 4). Its channels run through the reading of the channel
// (internal/channel), its reads through the explorer of a call (internal/onchain), its writes and
// records through the checkout links' records (internal/checkoutlink).
func TestNodeURLVectors(t *testing.T) {
	urls := tv.Objs(t, tv.Load(t, "bitcoin-nodes.json"), "urls")
	for _, c := range urls {
		name := tv.Str(t, c, "name")
		got, ok := ParseURL(tv.Get(c, "url"))
		var gotJSON any
		if ok {
			gotJSON = jsonv.O("rest", got.REST, "webSocket", got.WebSocket)
		}
		tv.Equal(t, name, gotJSON, tv.Get(c, "expected"))
		if ok && got.URL != tv.Get(c, "url") {
			t.Errorf("%s: %s", name, got.URL)
		}
	}
	t.Logf("%d urls", len(urls))
}
