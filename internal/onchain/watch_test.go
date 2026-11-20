package onchain

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sattleme/checkout-go/internal/bitcoinnode"
	"github.com/sattleme/checkout-go/internal/httpx"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/relay"
	"github.com/sattleme/checkout-go/internal/testkit"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// watched is a watch of an address on the explorer's WebSocket of a test network: the changes it
// heard and the reconnection delays it waited.
type watched struct {
	mu      sync.Mutex
	changes int
	delays  []time.Duration
	// wake ends a reconnection's wait at once.
	wake chan struct{}
}

func (w *watched) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.changes
}

func (w *watched) waited() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.delays...)
}

func watch(t *testing.T, n *testkit.Net, address string) (*watched, *Watch) {
	t.Helper()
	w := &watched{wake: make(chan struct{}, 100)}
	watch := WatchAddress(context.Background(), WatchOptions{
		Dialer:  n.Dialer(),
		Header:  http.Header{"User-Agent": {proto.UserAgent}},
		Timeout: 2 * time.Second,
		Sleep: func(ctx context.Context, d time.Duration) {
			w.mu.Lock()
			w.delays = append(w.delays, d)
			w.mu.Unlock()
			select {
			case <-ctx.Done():
			case <-w.wake:
			}
		},
	}, address, func() {
		w.mu.Lock()
		w.changes++
		w.mu.Unlock()
	})
	t.Cleanup(watch.Close)
	return w, watch
}

func TestTheExplorersWebSocketIsWatched(t *testing.T) {
	n := testkit.NewNet()
	t.Cleanup(n.Close)
	socket := n.AddExplorerSocket()
	address := testkit.DepositAddress("watched")
	w, _ := watch(t, n, address)
	// What happened before was not pushed: a change at each connection.
	testkit.Eventually(t, func() bool { return len(socket.Messages()) == 2 && w.count() == 1 }, "the subscription")
	tv.Equal(t, "messages", socket.Messages(), []any{jsonv.O("action", "want", "data", []string{"blocks"}), jsonv.O("track-address", address)})
	// The handshake carries the User-Agent of every request (SPEC.md §13, §15), and offers no
	// compression.
	handshake := socket.Handshakes()[0]
	if got := handshake.Get("User-Agent"); got != "sattleme-Checkout/1" {
		t.Errorf("User-Agent %q", got)
	}
	if got := handshake.Get("Sec-WebSocket-Extensions"); got != "" {
		t.Errorf("compression offered: %q", got)
	}

	// A change of the address, a block: checked again. Anything else is no change.
	for i, message := range []any{
		jsonv.O("address-transactions", []any{}),
		jsonv.O("block-transactions", []any{}),
		jsonv.O("address-removed-transactions", []any{}),
		jsonv.O("block", jsonv.O("height", 970_001)),
	} {
		socket.Push(message)
		testkit.Eventually(t, func() bool { return w.count() == i+2 }, "a change")
	}
	socket.Push(jsonv.O("mempoolInfo", jsonv.O("size", 1)))
	socket.Push("not an object")
	socket.Push(jsonv.O("block", nil))
	testkit.Eventually(t, func() bool { return w.count() == 6 }, "a block")
	time.Sleep(50 * time.Millisecond)
	if w.count() != 6 {
		t.Errorf("%d changes", w.count())
	}
}

func TestALostWebSocketConnectsAgainLaterAndLater(t *testing.T) {
	n := testkit.NewNet()
	t.Cleanup(n.Close)
	socket := n.AddExplorerSocket()
	w, _ := watch(t, n, testkit.DepositAddress("lost"))
	testkit.Eventually(t, func() bool { return w.count() == 1 }, "the first connection")
	// Lost, then down: 1 s, then doubled at each failure up to 30 s.
	n.Down(proto.MempoolWebSocketURL)
	socket.Drop()
	testkit.Eventually(t, func() bool { return len(w.waited()) == 1 }, "the first wait")
	for i := 0; i < 6; i++ {
		w.wake <- struct{}{}
		testkit.Eventually(t, func() bool { return len(w.waited()) == i+2 }, "a wait")
	}
	tv.Equal(t, "delays", durations(w.waited()), durations([]time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second,
	}))
	// Up again: connected, a change; the explorer answers (a block), and the next loss waits 1 s
	// again.
	n.AddServer(proto.MempoolWebSocketURL, socket)
	w.wake <- struct{}{}
	testkit.Eventually(t, func() bool { return w.count() == 2 }, "the connection again")
	socket.Push(jsonv.O("block", jsonv.O("height", 970_001)))
	testkit.Eventually(t, func() bool { return w.count() == 3 }, "the block")
	socket.Drop()
	testkit.Eventually(t, func() bool { return len(w.waited()) == 8 }, "the wait after it")
	if last := w.waited()[7]; last != time.Second {
		t.Errorf("waited %v after a connection the explorer answered on", last)
	}
}

// A connection the explorer drops before it sent a message is a failure: each connection starts a
// full check, and an explorer that accepts and drops every connection must not have one every
// second. The delay doubles up to 30 s, and goes back to 1 s once a connection received a
// message.
func TestAConnectionTheExplorerDropsBeforeAMessageIsAFailure(t *testing.T) {
	n := testkit.NewNet()
	t.Cleanup(n.Close)
	socket := n.AddExplorerSocket()
	w, _ := watch(t, n, testkit.DepositAddress("dropped"))
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i := range want {
		// Connected (a change), subscribed, then dropped without a word.
		testkit.Eventually(t, func() bool { return w.count() == i+1 && len(socket.Messages()) == 2*(i+1) }, "a connection")
		socket.Drop()
		testkit.Eventually(t, func() bool { return len(w.waited()) == i+1 }, "a wait")
		w.wake <- struct{}{}
	}
	tv.Equal(t, "delays", durations(w.waited()), durations(want))
	// A connection the explorer answers on: the next loss waits 1 s again.
	testkit.Eventually(t, func() bool { return w.count() == len(want)+1 && len(socket.Messages()) == 2*(len(want)+1) }, "a connection")
	socket.Push(jsonv.O("address-transactions", []any{}))
	testkit.Eventually(t, func() bool { return w.count() == len(want)+2 }, "the explorer's message")
	socket.Drop()
	testkit.Eventually(t, func() bool { return len(w.waited()) == len(want)+1 }, "the wait after it")
	if last := w.waited()[len(want)]; last != time.Second {
		t.Errorf("waited %v after a connection the explorer answered on", last)
	}
}

func TestAWatchEndsWhenClosed(t *testing.T) {
	n := testkit.NewNet()
	t.Cleanup(n.Close)
	socket := n.AddExplorerSocket()
	w, watch := watch(t, n, testkit.DepositAddress("closed"))
	testkit.Eventually(t, func() bool { return w.count() == 1 }, "the connection")
	watch.Close()
	socket.Push(jsonv.O("block", jsonv.O("height", 1)))
	time.Sleep(50 * time.Millisecond)
	if w.count() != 1 {
		t.Errorf("%d changes after it closed", w.count())
	}
	// A handshake that never ends is bounded too: Close does not wait for it.
	hole := testkit.NewNet()
	t.Cleanup(hole.Close)
	hole.AddBlackhole(proto.MempoolWebSocketURL)
	_, stuck := watchTimeout(t, hole, time.Minute)
	done := make(chan struct{})
	go func() { stuck.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close waited for the handshake")
	}
}

// A watch ends when closed whatever its connection heeds: one whose reads heed no context is
// closed, so that Close (and WaitForPayment, which closes its watch before it returns) does not
// wait for it.
func TestAWatchEndsWhenClosedWhateverItsConnectionHeeds(t *testing.T) {
	conn := &deafConn{closed: make(chan struct{})}
	t.Cleanup(func() { conn.Close() })
	connected := make(chan struct{}, 1)
	watch := WatchAddress(context.Background(), WatchOptions{Dialer: deafDialer{conn}, Timeout: time.Second},
		testkit.DepositAddress("deaf"), func() {
			select {
			case connected <- struct{}{}:
			default:
			}
		})
	<-connected
	done := make(chan struct{})
	go func() { watch.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close waited for a read that heeds no context")
	}
}

// deafConn takes every message and reads nothing until it is closed, whatever its context.
type deafConn struct {
	once   sync.Once
	closed chan struct{}
}

func (c *deafConn) Read(context.Context) ([]byte, error) {
	<-c.closed
	return nil, errors.New("closed")
}

func (c *deafConn) Write(context.Context, []byte) error { return nil }

func (c *deafConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// deafDialer gives its connection to every dial.
type deafDialer struct{ conn *deafConn }

func (d deafDialer) Dial(context.Context, string, http.Header) (relay.Conn, error) {
	return d.conn, nil
}

func watchTimeout(t *testing.T, n *testkit.Net, timeout time.Duration) (*watched, *Watch) {
	t.Helper()
	w := &watched{}
	return w, WatchAddress(context.Background(), WatchOptions{Dialer: n.Dialer(), Header: http.Header{"User-Agent": {proto.UserAgent}}, Timeout: timeout},
		testkit.DepositAddress("stuck"), func() {})
}

func durations(list []time.Duration) []any {
	out := make([]any, len(list))
	for i, d := range list {
		out[i] = d.String()
	}
	return out
}

// loggingDialer dials over d, and records each URL dialed, in order.
type loggingDialer struct {
	d      relay.Dialer
	mu     sync.Mutex
	dialed []string
}

func (l *loggingDialer) Dial(ctx context.Context, url string, header http.Header) (relay.Conn, error) {
	l.mu.Lock()
	l.dialed = append(l.dialed, url)
	l.mu.Unlock()
	return l.d.Dial(ctx, url, header)
}

func (l *loggingDialer) urls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.dialed...)
}

// explorerSocket is an explorer's WebSocket of a test: once subscribed, it answers (a block) and
// keeps the connection when answers; else it drops it before any message.
type explorerSocket struct {
	answers  bool
	mu       sync.Mutex
	answered int
}

func (s *explorerSocket) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	c, err := websocket.Accept(w, req, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled, InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	if !s.answers {
		return
	}
	for range 2 {
		if _, _, err := c.Read(req.Context()); err != nil {
			return
		}
	}
	if err := c.Write(req.Context(), websocket.MessageText, jsonv.Marshal(jsonv.O("block", jsonv.O("height", 970_001)))); err != nil {
		return
	}
	s.mu.Lock()
	s.answered++
	s.mu.Unlock()
	for {
		if _, _, err := c.Read(req.Context()); err != nil {
			return
		}
	}
}

func (s *explorerSocket) answeredOnce() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.answered > 0
}

// bitcoin-nodes.json's reads, as a WebSocket's connection (SPEC.md §31: the WebSocket follows the
// rule too, a connection failing when it fails or is lost before any message): the WebSockets
// tried, in order, and the one the watch stays on, or none ("network": it waits, then tries again
// as the rule says). A node's WebSocket of a local network (ws://), mempool.space's (wss://). An
// explorer that fails refuses the connection, or drops it before any message.
func TestTheWebSocketFollowsTheReadsVectors(t *testing.T) {
	reads := tv.Objs(t, tv.Load(t, "bitcoin-nodes.json"), "reads")
	nodeSocket := readsNode + "/api/v1/ws"
	nodeSocket = "ws" + strings.TrimPrefix(nodeSocket, "http")
	now := time.Unix(1_791_381_280, 0)
	for _, c := range reads {
		for _, failure := range []string{"refused", "dropped"} {
			name := tv.Str(t, c, "name") + ", " + failure
			expected := tv.Obj(t, c, "expected")
			n := testkit.NewNet()
			var node *bitcoinnode.Node
			failures := NewNodeFailures()
			nodeServer := &explorerSocket{}
			if v := tv.Get(c, "node"); v != nil {
				o, _ := jsonv.AsObject(v)
				node = &bitcoinnode.Node{URL: readsNode, Fallback: bitcoinnode.Fallback(tv.Str(t, o, "fallback"))}
				if tv.Get(o, "failedRecently") == true {
					failures.failed(readsNode, now.Add(-30*time.Second))
				}
				nodeServer.answers = tv.Get(o, "answers") == true
				if nodeServer.answers || failure == "dropped" {
					n.AddServer(nodeSocket, nodeServer)
				}
			}
			mempoolServer := &explorerSocket{answers: tv.Get(c, "mempoolAnswers") == true}
			if mempoolServer.answers || failure == "dropped" {
				n.AddServer(proto.MempoolWebSocketURL, mempoolServer)
			}
			dialer := &loggingDialer{d: n.Dialer()}
			waited := make(chan struct{}, 1)
			x := NewExplorer(httpx.New(testkit.NewFakeHTTP(func(*http.Request) testkit.Answer { return testkit.Answer{} }), time.Second), node, failures,
				func() time.Time { return now })
			watch := WatchAddress(context.Background(), WatchOptions{
				Dialer: dialer, Timeout: 2 * time.Second, Explorer: x,
				Sleep: func(ctx context.Context, _ time.Duration) {
					select {
					case waited <- struct{}{}:
					default:
					}
					<-ctx.Done()
				},
			}, testkit.DepositAddress("watched"), func() {})
			result := ""
			deadline := time.After(10 * time.Second)
			for result == "" {
				select {
				case <-waited:
					result = "network"
				case <-deadline:
					t.Fatalf("%s: neither answered nor waited", name)
				case <-time.After(5 * time.Millisecond):
					if nodeServer.answeredOnce() {
						result = NodeExplorer
					} else if mempoolServer.answeredOnce() {
						result = MempoolExplorer
					}
				}
			}
			watch.Close()
			n.Close()
			asked := []any{}
			for _, u := range dialer.urls() {
				asked = append(asked, map[string]string{nodeSocket: NodeExplorer, proto.MempoolWebSocketURL: MempoolExplorer}[u])
			}
			tv.Equal(t, name+": asked", asked, tv.Get(expected, "asked"))
			tv.Equal(t, name+": result", result, tv.Get(expected, "result"))
			// A node's connection that failed is remembered for the call.
			if at, failed := failures.failedAt(readsNode); node != nil && !nodeServer.answers && asked[0] == NodeExplorer && (!failed || !at.Equal(now)) {
				t.Errorf("%s: the node's failure not remembered", name)
			}
		}
	}
}
