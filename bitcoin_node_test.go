package checkout_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/testkit"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// The merchant's Bitcoin node (SPEC.md §31, "The merchant's Bitcoin node"), end to end against
// local relays and fakes of breez.tips, of mempool.space, of the node (REST and WebSocket) and of
// BlockCypher: every read of an address and of the chain's height asked of the node, and never of
// mempool.space while it answers; with fallback "mempool", a node that does not answer read from
// mempool.space, and asked after it for 60 s; with "none", mempool.space never asked, network and
// never expired while the node is down; a check's two reads always of one explorer; an invalid
// bitcoinNode read as none.

// localNode is a node on the merchant's network, in clear http.
const localNode = "http://192.168.1.10:3006"

// nodeMerchant is a merchant whose channel names bitcoinNode (absent: testkit.Absent), its node
// at localNode served by the chain, its lot the addresses given (lot's first by default).
func nodeMerchant(t *testing.T, bitcoinNode any, addresses ...string) *testkit.Merchant {
	t.Helper()
	m := testkit.NewMerchant()
	m.Start(nil, nil)
	t.Cleanup(m.Stop)
	members := []jsonv.Member{{Name: "relays", Value: m.KeyRelays}, {Name: "checkout", Value: jsonv.O("origin", origin)}}
	if bitcoinNode != testkit.Absent {
		members = append(members, jsonv.Member{Name: "bitcoinNode", Value: bitcoinNode})
	}
	m.PublishChannel(m.ChannelValue(100, members...), 100)
	if len(addresses) == 0 {
		addresses = lot[:1]
	}
	m.PublishLot(addresses, 90)
	m.Chain.AddNode(localNode)
	return m
}

// nodeOf is a channel's bitcoinNode.
func nodeOf(url, fallback string, payers any) jsonv.Members {
	return jsonv.O("url", url, "fallback", fallback, "payers", payers)
}

// explorersAsked are the explorers the chain's reads went to, in order ("node", "mempool"), and
// whether every read of the node carried the User-Agent of SPEC.md §15.
func explorersAsked(m *testkit.Merchant) []string {
	var asked []string
	for _, call := range m.HTTP.Calls() {
		u := call.URL.String()
		switch {
		case strings.HasPrefix(u, localNode+"/api/"):
			asked = append(asked, "node")
		case strings.HasPrefix(u, proto.MempoolURL+"/"):
			asked = append(asked, "mempool")
		}
	}
	return asked
}

// nodeCalls are the requests the node got.
func nodeCalls(m *testkit.Merchant) []testkit.Call {
	var calls []testkit.Call
	for _, call := range m.HTTP.Calls() {
		if strings.HasPrefix(call.URL.String(), localNode+"/") {
			calls = append(calls, call)
		}
	}
	return calls
}

func count(list []string, item string) int {
	n := 0
	for _, x := range list {
		if x == item {
			n++
		}
	}
	return n
}

func TestTheNodeAnswersEveryReadAndMempoolSpaceIsNeverAsked(t *testing.T) {
	for _, fallback := range []string{"mempool", "none"} {
		t.Run(fallback, func(t *testing.T) {
			m := nodeMerchant(t, nodeOf(localNode, fallback, false))
			c := clientOf(t, m)
			invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "N-1", Sats: 49_000, DurationSeconds: 600})
			must(t, err)
			if invoice.OnchainAddress != lot[0] {
				t.Fatalf("%+v", invoice)
			}
			tx := testkit.Payment(testkit.Txid("node pay"), lot[0], 49_000, nil, 0)
			m.Chain.Add(lot[0], tx)
			same(t, "unconfirmed", statusJSON(t, c, invoice), jsonv.O("state", "unconfirmed", "received", "49000", "confirmations", 0, "txids", []string{tx.Txid}))
			m.Chain.Confirm(lot[0], tx.Txid, m.Chain.Height()-2)
			same(t, "paid", statusJSON(t, c, invoice), jsonv.O("state", "paid", "method", "onchain", "received", "49000", "txids", []string{tx.Txid}))
			// Leasing, and each check's transactions and height: the node's, every one.
			asked := explorersAsked(m)
			if count(asked, "mempool") != 0 || count(asked, "node") != 5 {
				t.Errorf("asked %v", asked)
			}
			for _, call := range nodeCalls(m) {
				if call.Headers.Get("User-Agent") != proto.UserAgent {
					t.Errorf("%s: User-Agent %q", call.URL, call.Headers.Get("User-Agent"))
				}
			}
			want := []string{localNode + "/api/address/" + lot[0] + "/txs", localNode + "/api/blocks/tip/height"}
			for _, u := range want {
				if !contains(m.Chain.Asked(), u) {
					t.Errorf("%s not asked", u)
				}
			}
		})
	}
}

// A redirect is no answer (SPEC.md §15): never followed, the node failed, its fallback applies.
func TestANodesRedirectIsAFailureNeverFollowed(t *testing.T) {
	m := nodeMerchant(t, nodeOf(localNode, "mempool", false))
	m.SetOverride(func(req *http.Request) (testkit.Answer, bool) {
		if strings.HasPrefix(req.URL.String(), localNode+"/") {
			return testkit.Answer{Status: 302, Headers: map[string]string{"Location": "http://192.168.1.11/api/x"}}, true
		}
		return testkit.Answer{}, false
	})
	invoice, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "N-1", Sats: 49_000})
	must(t, err)
	if invoice.OnchainAddress != lot[0] {
		t.Errorf("%+v", invoice)
	}
	if asked := explorersAsked(m); strings.Join(asked, " ") != "node mempool" {
		t.Errorf("asked %v", asked)
	}
	for _, call := range m.HTTP.Calls() {
		if call.URL.Host == "192.168.1.11" {
			t.Error("the redirect was followed")
		}
	}
}

// With fallback "mempool", a node that does not answer: the same reads of mempool.space; a node
// that failed less than 60 s ago in the same call is asked after mempool.space (leasing reads 3
// addresses, all paid already: the node fails the first read, then mempool.space answers alone);
// 60 s later, the node first again. Both failing: network.
func TestANodeDownIsReadFromMempoolSpaceAndAskedAfterItFor60Seconds(t *testing.T) {
	m := nodeMerchant(t, nodeOf(localNode, "mempool", false), lot...)
	m.Chain.SetDown(localNode, true)
	for i, address := range lot {
		m.Chain.Add(address, testkit.Payment(testkit.Txid("paid already "+itoa(int64(i))), address, 10_000, nil, 969_000))
	}
	start := m.Now()
	mempoolReads := 0
	m.SetOverride(func(req *http.Request) (testkit.Answer, bool) {
		if strings.HasPrefix(req.URL.String(), proto.MempoolURL+"/address/") {
			// The second address read from mempool.space: 60 s later.
			if mempoolReads++; mempoolReads == 2 {
				m.SetNow(start + 60)
			}
		}
		return testkit.Answer{}, false
	})
	invoice, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "N-1", Sats: 49_000})
	must(t, err)
	// Every address paid already: Lightning only.
	if invoice.OnchainAddress != "" || invoice.Bolt11 == "" {
		t.Errorf("%+v", invoice)
	}
	same(t, "asked", explorersAsked(m), []string{"node", "mempool", "mempool", "node", "mempool"})

	// Both down: network.
	m2 := nodeMerchant(t, nodeOf(localNode, "mempool", false))
	c := clientOf(t, m2)
	invoice, err = c.CreateInvoice(ctx, checkout.Order{Ref: "N-2", Sats: 49_000})
	must(t, err)
	m2.Chain.SetDown(localNode, true)
	m2.Chain.SetDown(testkit.Explorer, true)
	_, err = c.GetStatus(ctx, invoice)
	wantCode(t, err, checkout.CodeNetwork)
	m2.Chain.SetDown(testkit.Explorer, false)
	if status := statusJSON(t, c, invoice); fieldOf(status, "state") != "open" {
		t.Errorf("%s", jsonv.Marshal(status))
	}
}

// A check's two reads, the address's transactions and the chain's height, come from one explorer
// (SPEC.md §31): the node failing either, both are asked of mempool.space, never the node's
// transactions with mempool.space's height. With "none", the check fails.
func TestAChecksTransactionsAndHeightComeFromOneExplorer(t *testing.T) {
	for _, failing := range []string{"height", "transactions"} {
		t.Run(failing, func(t *testing.T) {
			m := nodeMerchant(t, nodeOf(localNode, "mempool", false))
			c := clientOf(t, m)
			invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "N-1", Sats: 49_000, DurationSeconds: 600})
			must(t, err)
			tx := testkit.Payment(testkit.Txid("one explorer"), lot[0], 49_000, nil, m.Chain.Height()-2)
			m.Chain.Add(lot[0], tx)
			// The node answers its height or its transactions, not both; mempool.space is two
			// blocks behind the node.
			if failing == "height" {
				m.Chain.SetHeightDown(localNode, true)
			} else {
				m.SetOverride(func(req *http.Request) (testkit.Answer, bool) {
					if strings.HasPrefix(req.URL.String(), localNode+"/api/address/") {
						return testkit.Text(500, "down"), true
					}
					return testkit.Answer{}, false
				})
			}
			m.Chain.SetExplorerHeight(testkit.Explorer, m.Chain.Height()-2)
			before := len(m.Chain.Asked())
			// 1 confirmation on mempool.space: its height, with its transactions.
			same(t, "unconfirmed", statusJSON(t, c, invoice), jsonv.O("state", "unconfirmed", "received", "49000", "confirmations", 1, "txids", []string{tx.Txid}))
			asked := m.Chain.Asked()[before:]
			for _, u := range []string{proto.MempoolURL + "/address/" + lot[0] + "/txs", proto.MempoolURL + "/blocks/tip/height"} {
				if !contains(asked, u) {
					t.Errorf("%s not asked: %v", u, asked)
				}
			}
		})
	}
	m := nodeMerchant(t, nodeOf(localNode, "none", false))
	c := clientOf(t, m)
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "N-1", Sats: 49_000, DurationSeconds: 600})
	must(t, err)
	m.Chain.SetHeightDown(localNode, true)
	_, err = c.GetStatus(ctx, invoice)
	wantCode(t, err, checkout.CodeNetwork)
	if asked := explorersAsked(m); count(asked, "mempool") != 0 {
		t.Errorf("asked %v", asked)
	}
}

// With fallback "none" and the node down (SPEC.md §31): mempool.space never asked; no address
// leased (a Lightning invoice only, network for an order paid on-chain only); every check of an
// order with an address network, never expired nor final underpaid however long the node is
// down; the order paid once the node answers again.
func TestWithFallbackNoneANodeDownJudgesNothingOnchain(t *testing.T) {
	m := nodeMerchant(t, nodeOf(localNode, "none", false), lot...)
	c := clientOf(t, m)
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "N-1", Sats: 49_000, DurationSeconds: 600})
	must(t, err)
	address := invoice.OnchainAddress
	if address == "" {
		t.Fatalf("%+v", invoice)
	}
	m.Chain.SetDown(localNode, true)
	// No address: a Lightning invoice only.
	log := quiet()
	other, err := clientOf(t, m, checkout.WithLogger(log)).CreateInvoice(ctx, checkout.Order{Ref: "N-2", Sats: 49_000})
	must(t, err)
	if other.OnchainAddress != "" || other.Bolt11 == "" || !log.Has("no deposit address leased") {
		t.Errorf("%+v", other)
	}
	// Paid on-chain only (above the Lightning address's bounds): network.
	m.SetPay(jsonv.Member{Name: "maxSendable", Value: 10_000_000})
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "N-3", Sats: 49_000})
	wantCode(t, err, checkout.CodeNetwork)
	m.SetPay(jsonv.Member{Name: "maxSendable", Value: int64(100_000_000_000)})
	// Paid in time, the node down: network, until long after the lease's end.
	tx := testkit.Payment(testkit.Txid("while down"), address, 49_000, nil, 0)
	m.Chain.Add(address, tx)
	for _, at := range []int64{0, invoice.ExpiresAt + 121, invoice.ExpiresAt + 7200} {
		if at > 0 {
			m.SetNow(at)
		}
		_, err := c.GetStatus(ctx, invoice)
		wantCode(t, err, checkout.CodeNetwork)
	}
	if asked := explorersAsked(m); count(asked, "mempool") != 0 {
		t.Errorf("mempool.space asked: %v", asked)
	}
	// The node answers again: the transaction was seen by no check in time; BlockCypher tells it
	// came in time, then 3 confirmations: paid.
	m.Chain.SetDown(localNode, false)
	m.Chain.SetReceived(tx.Txid, iso(invoice.ExpiresAt-60))
	m.Chain.Confirm(address, tx.Txid, m.Chain.Height()-2)
	same(t, "paid", statusJSON(t, c, invoice), jsonv.O("state", "paid", "method", "onchain", "received", "49000", "txids", []string{tx.Txid}))
	if asked := explorersAsked(m); count(asked, "mempool") != 0 {
		t.Errorf("mempool.space asked: %v", asked)
	}
}

// An invalid bitcoinNode is none (SPEC.md §14): the channel stays valid, mempool.space is read.
func TestAnInvalidNodeReadsMempoolSpace(t *testing.T) {
	for name, node := range map[string]any{
		"http to a public host":       nodeOf("http://node.example", "mempool", false),
		"a fallback of another case":  nodeOf(localNode, "Mempool", false),
		"payers absent":               jsonv.O("url", localNode, "fallback", "none"),
		"payers not a boolean":        nodeOf(localNode, "none", "false"),
		"payers for a local node":     nodeOf(localNode, "none", true),
		"payers for an IPv4 address":  nodeOf("https://192.168.1.10:3006", "none", true),
		"the node's API, not its URL": nodeOf(localNode+"/api", "none", false),
		"a string":                    localNode,
	} {
		t.Run(name, func(t *testing.T) {
			m := nodeMerchant(t, node)
			invoice, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "N-1", Sats: 49_000})
			must(t, err)
			if invoice.OnchainAddress != lot[0] {
				t.Errorf("%+v", invoice)
			}
			if asked := explorersAsked(m); strings.Join(asked, " ") != "mempool" {
				t.Errorf("asked %v", asked)
			}
		})
	}
}

// waitWith runs WaitForPayment of invoice (Lightning once a minute: what moves is the socket);
// its outcome comes on the channel.
func waitWith(c *checkout.Client, invoice *checkout.Invoice) <-chan waited {
	done := make(chan waited, 1)
	go func() {
		status, err := c.WaitForPayment(ctx, invoice, checkout.WithInterval(60*time.Second))
		done <- waited{status, err}
	}()
	return done
}

// WaitForPayment watches the address on the node's WebSocket (ws:// for an http:// node, its
// handshake with the User-Agent), never mempool.space's while it answers; a block it pushes
// starts a check, which reads the node.
func TestWaitForPaymentWatchesTheNodesWebSocket(t *testing.T) {
	m := nodeMerchant(t, nodeOf(localNode, "mempool", false))
	nodeSocket := m.Net.AddNodeSocket(localNode)
	mempoolSocket := m.Net.AddExplorerSocket()
	c := clientOf(t, m)
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "N-1", Sats: 49_000, DurationSeconds: 600})
	must(t, err)
	done := waitWith(c, invoice)
	testkit.Eventually(t, func() bool { return len(nodeSocket.Messages()) == 2 }, "the node's subscription")
	tv.Equal(t, "messages", nodeSocket.Messages(), []any{jsonv.O("action", "want", "data", []string{"blocks"}), jsonv.O("track-address", invoice.OnchainAddress)})
	if got := nodeSocket.Handshakes()[0].Get("User-Agent"); got != proto.UserAgent {
		t.Errorf("User-Agent %q", got)
	}
	tx := testkit.Payment(testkit.Txid("node socket"), invoice.OnchainAddress, 49_000, nil, m.Chain.Height()-2)
	m.Chain.Add(invoice.OnchainAddress, tx)
	nodeSocket.Push(jsonv.O("block", jsonv.O("height", m.Chain.Height())))
	w := outcomeOf(t, done)
	must(t, w.err)
	same(t, "paid", jsonOf(t, w.status), jsonv.O("state", "paid", "method", "onchain", "received", "49000", "txids", []string{tx.Txid}))
	if n := mempoolSocket.Connections(); n != 0 {
		t.Errorf("mempool.space's WebSocket connected %d times", n)
	}
	if asked := explorersAsked(m); count(asked, "mempool") != 0 {
		t.Errorf("asked %v", asked)
	}
}

// With "mempool", a connection to the node's WebSocket that fails, or is lost before any message,
// goes to mempool.space's at once (SPEC.md §31); with "none", only the node's, connected again as
// the rule says (1 s, then 2 s), never mempool.space's.
func TestTheWebSocketFallsBackAsTheNodesFallbackSays(t *testing.T) {
	for _, c := range []struct {
		fallback string
		// dropped: the node's WebSocket accepts and drops each connection; else it refuses it.
		dropped bool
	}{{"mempool", false}, {"mempool", true}, {"none", false}, {"none", true}} {
		name := c.fallback + map[bool]string{false: ", refused", true: ", dropped"}[c.dropped]
		t.Run(name, func(t *testing.T) {
			m := nodeMerchant(t, nodeOf(localNode, c.fallback, false))
			var nodeSocket *testkit.FakeExplorerSocket
			if c.dropped {
				nodeSocket = m.Net.AddNodeSocket(localNode)
			}
			mempoolSocket := m.Net.AddExplorerSocket()
			client := clientOf(t, m)
			invoice, err := client.CreateInvoice(ctx, checkout.Order{Ref: "N-1", Sats: 49_000, DurationSeconds: 600})
			must(t, err)
			waitCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan waited, 1)
			go func() {
				status, err := client.WaitForPayment(waitCtx, invoice, checkout.WithInterval(60*time.Second))
				done <- waited{status, err}
			}()
			nodeConnections := func() int { return m.Net.Connections(testkit.NodeSocketURL(localNode)) }
			if c.dropped {
				// Connected, subscribed, then dropped without a word: a failure.
				testkit.Eventually(t, func() bool { return len(nodeSocket.Messages()) == 2 }, "the node's subscription")
				nodeSocket.Drop()
			}
			if c.fallback == "mempool" {
				testkit.Eventually(t, func() bool { return len(mempoolSocket.Messages()) == 2 }, "mempool.space's subscription")
				tx := testkit.Payment(testkit.Txid("fallback socket"), invoice.OnchainAddress, 49_000, nil, m.Chain.Height()-2)
				m.Chain.Add(invoice.OnchainAddress, tx)
				mempoolSocket.Push(jsonv.O("block", jsonv.O("height", m.Chain.Height())))
				w := outcomeOf(t, done)
				must(t, w.err)
				if w.status.State != checkout.StatePaid {
					t.Errorf("%+v", w.status)
				}
				return
			}
			// "none": the node's again after 1 s, then 2 s; never mempool.space's.
			first := nodeConnections()
			testkit.Eventually(t, func() bool { return nodeConnections() >= first+1 }, "the node's again")
			if c.dropped {
				nodeSocket.Drop()
			}
			time.Sleep(1500 * time.Millisecond)
			if mempoolSocket.Connections() != 0 {
				t.Errorf("mempool.space's WebSocket connected %d times", mempoolSocket.Connections())
			}
			cancel()
			if w := outcomeOf(t, done); !errors.Is(w.err, context.Canceled) {
				t.Errorf("%+v, %v", w.status, w.err)
			}
		})
	}
}

// internetNode is a node of the Internet, which the pages of checkout links may read.
const internetNode = "https://mempool.shop.example"

// The record of a checkout link to an order with an address holds the node's url and fallback
// (never payers) when its payers is true (SPEC.md §30), and the page reads it; no bitcoinNode with
// payers false, without a node, nor for an order without an address (version 1). The link made
// again by invoice_pending holds it too.
func TestTheCheckoutRecordNamesTheNodeOnlyForThePayers(t *testing.T) {
	for _, c := range []struct {
		name string
		node any
		// sats of the order: below 1 000 sats, no address (version 1).
		sats int64
		want any
	}{
		{"payers true, never mempool.space", nodeOf(internetNode, "none", true), 49_000, jsonv.O("url", internetNode, "fallback", "none")},
		{"payers true, falling back to mempool.space", nodeOf(internetNode, "mempool", true), 49_000, jsonv.O("url", internetNode, "fallback", "mempool")},
		{"payers false", nodeOf(internetNode, "mempool", false), 49_000, nil},
		{"payers false, a local node", nodeOf(localNode, "none", false), 49_000, nil},
		{"no node", testkit.Absent, 49_000, nil},
		{"payers true, no address (version 1)", nodeOf(internetNode, "mempool", true), 900, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := nodeMerchant(t, c.node)
			m.Chain.AddNode(internetNode)
			client := clientOf(t, m)
			order := checkout.Order{Ref: "L-1", Sats: c.sats, Note: "Order 1", Checkout: true}
			invoice, err := client.CreateInvoice(ctx, order)
			must(t, err)
			if (invoice.OnchainAddress == "") != (c.sats < 1000) {
				t.Fatalf("%+v", invoice)
			}
			_, err = client.CreateInvoice(ctx, order)
			again := wantCode(t, err, checkout.CodeInvoicePending)
			for _, url := range []string{invoice.CheckoutURL, again.CheckoutURL} {
				l := openLink(t, m, url)
				node, has := jsonv.Field(l.value, "bitcoinNode")
				if has != (c.want != nil) {
					t.Errorf("bitcoinNode %v in %s", node, jsonv.Marshal(l.value))
				}
				tv.Equal(t, "bitcoinNode", node, c.want)
				var read any
				if l.record != nil && l.record.BitcoinNode != nil {
					read = jsonv.O("url", l.record.BitcoinNode.URL, "fallback", string(l.record.BitcoinNode.Fallback))
				}
				tv.Equal(t, "what the page reads", read, c.want)
				if version := fieldOf(l.value, "version"); version != map[bool]float64{true: 1, false: 2}[c.sats < 1000] {
					t.Errorf("version %v", version)
				}
			}
		})
	}
}
