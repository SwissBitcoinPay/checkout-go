package onchain

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sattleme/checkout-go/internal/bitcoinnode"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/httpx"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/testkit"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// readsNode is the node of the reads' runner: a node of a local network, in clear http.
const readsNode = "http://192.168.1.10:3006"

// readsAddress is the address the reads ask about.
var readsAddress = testkit.DepositAddress("reads")

// explorerFake answers the reads of the explorers: the node's and mempool.space's, each answering
// or failing (HTTP 503) as the case says; failing names the request of a failing explorer that
// fails ("txs" or "height"; "" both), the other answering. It records the explorers asked, in
// order, each once in a row (askedSoFar).
type explorerFake struct {
	mu    sync.Mutex
	asked []any
}

func (f *explorerFake) transport(nodeAnswers, mempoolAnswers bool, failing string) http.RoundTripper {
	return testkit.NewFakeHTTP(func(req *http.Request) testkit.Answer {
		url := req.URL.String()
		name, answers, path := MempoolExplorer, mempoolAnswers, strings.TrimPrefix(url, proto.MempoolURL)
		if strings.HasPrefix(url, readsNode+"/api/") {
			name, answers, path = NodeExplorer, nodeAnswers, strings.TrimPrefix(url, readsNode+"/api")
		} else if !strings.HasPrefix(url, proto.MempoolURL+"/") {
			return testkit.Answer{Err: errNotAsked}
		}
		// A request of an attempt that ended (the other request of a step 1 whose first failed)
		// asks nothing: checked with the record, so that no request of an explorer the read left
		// is recorded after the next one's.
		f.mu.Lock()
		if err := req.Context().Err(); err != nil {
			f.mu.Unlock()
			return testkit.Answer{Err: err}
		}
		if len(f.asked) == 0 || f.asked[len(f.asked)-1] != name {
			f.asked = append(f.asked, name)
		}
		f.mu.Unlock()
		height := path == "/blocks/tip/height"
		fails := !answers && (failing == "" || failing == "height" && height || failing == "txs" && !height)
		switch {
		case fails:
			return testkit.Text(503, "down")
		case height:
			return testkit.Text(200, "970000")
		case path == "/address/"+readsAddress+"/txs":
			return testkit.JSON([]any{})
		}
		return testkit.Answer{Err: errNotAsked}
	})
}

func (f *explorerFake) askedSoFar() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]any{}, f.asked...)
}

var errNotAsked = errs.NewNetwork("not asked in this vector", nil)

// bitcoin-nodes.json's reads, through the function that decides which explorers a read asks and in
// what order (ExplorersToAsk), and through the reads of the explorer of a call (ReadExplorer: a
// read of an address's transactions, as leasing asks it; and step 1 of a check, an address's
// transactions and the chain's height of one explorer, failing when either fails), over fakes of
// the node and of mempool.space (PORTING.md, step 4). The node failed less than 60 s ago (30 s,
// and 59.999 s), or not lately (never, or 60 s ago exactly: not less than 60 s). The WebSocket's
// connections run through the same cases in watch_test.go.
func TestReadsVectors(t *testing.T) {
	reads := tv.Objs(t, tv.Load(t, "bitcoin-nodes.json"), "reads")
	now := time.Unix(1_791_381_280, 0)
	for _, c := range reads {
		name := tv.Str(t, c, "name")
		expected := tv.Obj(t, c, "expected")
		var node *bitcoinnode.Node
		nodeAnswers, recently := false, false
		if v := tv.Get(c, "node"); v != nil {
			n, _ := jsonv.AsObject(v)
			fallback := bitcoinnode.Fallback(tv.Str(t, n, "fallback"))
			node = &bitcoinnode.Node{URL: readsNode, Fallback: fallback}
			nodeAnswers, recently = tv.Get(n, "answers") == true, tv.Get(n, "failedRecently") == true
			// The explorers asked are the first of ExplorersToAsk's, the next asked only when one
			// failed.
			got, want := ExplorersToAsk(true, fallback, recently), tv.Arr(t, expected, "asked")
			if len(want) > len(got) {
				t.Errorf("%s: ExplorersToAsk %v", name, got)
			}
			for i := range min(len(want), len(got)) {
				if want[i] != got[i] {
					t.Errorf("%s: ExplorersToAsk %v", name, got)
				}
			}
		} else if got := ExplorersToAsk(false, "", false); len(got) != 1 || got[0] != MempoolExplorer {
			t.Errorf("%s: ExplorersToAsk %v", name, got)
		}
		failedAgo := []time.Duration{-1, 60 * time.Second}
		if recently {
			failedAgo = []time.Duration{30 * time.Second, 60*time.Second - time.Millisecond}
		}
		for _, ago := range failedAgo {
			for _, read := range []string{"transactions", "step 1", "step 1, the height failing", "step 1, the transactions failing"} {
				failing := map[string]string{"step 1, the height failing": "height", "step 1, the transactions failing": "txs"}[read]
				f := &explorerFake{asked: []any{}}
				client := httpx.New(f.transport(nodeAnswers, tv.Get(c, "mempoolAnswers") == true, failing), time.Second)
				failures := NewNodeFailures()
				if ago >= 0 {
					failures.failed(readsNode, now.Add(-ago))
				}
				x := NewExplorer(client, node, failures, func() time.Time { return now })
				var err error
				if read == "transactions" {
					_, err = ReadTransactions(context.Background(), x, readsAddress)
				} else {
					_, err = ReadAddress(context.Background(), x, readsAddress)
				}
				// Whose answer the read gives: the last explorer asked, when it answered.
				asked := f.askedSoFar()
				result := "network"
				if err == nil {
					result = asked[len(asked)-1].(string)
				} else if !errs.Is(err, errs.Network) {
					t.Errorf("%s, %s: %v", name, read, err)
				}
				label := name + ", " + read
				tv.Equal(t, label+": asked", asked, tv.Get(expected, "asked"))
				tv.Equal(t, label+": result", result, tv.Get(expected, "result"))
				// A node that failed is remembered for the call: asked after mempool.space for 60 s.
				if at, failed := failures.failedAt(readsNode); node != nil && !nodeAnswers && asked[0] == NodeExplorer && (!failed || !at.Equal(now)) {
					t.Errorf("%s: the node's failure not remembered", label)
				}
			}
		}
	}
	t.Logf("%d reads", len(reads))
}
