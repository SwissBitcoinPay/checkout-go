package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/testkit"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

var signer = bytes.Repeat([]byte{7}, 32)

func newPool(t *testing.T, ctx context.Context, n *testkit.Net, timeout time.Duration) *Pool {
	t.Helper()
	p := NewPool(ctx, Options{
		Dialer:  n.Dialer(),
		Timeout: timeout,
		Now:     func() int64 { return 1_791_378_000 },
		Random:  rand.Reader,
		Header:  http.Header{"User-Agent": {proto.UserAgent}},
	})
	t.Cleanup(p.Close)
	return p
}

func network(t *testing.T) *testkit.Net {
	t.Helper()
	n := testkit.NewNet()
	t.Cleanup(n.Close)
	return n
}

func event(t *testing.T, createdAt int64, content string) *nostr.Event {
	t.Helper()
	e, err := nostr.Sign(nostr.Template{CreatedAt: createdAt, Kind: 1, Tags: [][]string{}, Content: content}, signer, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func keepAll(v any) (any, bool) { return v, true }

var authKey = bytes.Repeat([]byte{9}, 32)

func TestReadsGoOnOnceAMajorityAnswered(t *testing.T) {
	n := network(t)
	a := n.Add("wss://a.example", testkit.Behaviour{})
	b := n.Add("wss://b.example", testkit.Behaviour{})
	n.Add("wss://c.example", testkit.Behaviour{Mute: true})
	a.StoreEvent(event(t, 100, "a"))
	b.StoreEvent(event(t, 101, "b"))
	p := newPool(t, context.Background(), n, 60*time.Second)
	start := time.Now()
	answers := p.QueryRelays([]string{"wss://a.example", "wss://b.example", "wss://c.example"}, Filter{Kinds: []int64{1}}, authKey, QueryOptions{Needed: 2, Keep: keepAll})
	if len(answers) != 2 || time.Since(start) > 30*time.Second {
		t.Errorf("%d answers after %v", len(answers), time.Since(start))
	}
	// The muted relay was dropped, not failed: no timeout ran out for it.
	if p.Failed("wss://c.example") {
		t.Error("a dropped relay counts as failed")
	}
	// Each subscription closed after its EOSE, on the connection kept for the call.
	testkit.Eventually(t, func() bool { return len(a.ClosedSubscriptions()) == 1 }, "the subscription closed")
	if n.Connections("wss://a.example") != 1 {
		t.Errorf("%d connections", n.Connections("wss://a.example"))
	}
	p.QueryRelays([]string{"wss://a.example"}, Filter{Kinds: []int64{1}}, authKey, QueryOptions{Needed: 1, Keep: keepAll})
	if n.Connections("wss://a.example") != 1 {
		t.Error("a second connection for the call")
	}
}

func TestEveryRelayWaitsOneTimeoutMoreAndSkipsFailedRelays(t *testing.T) {
	n := network(t)
	n.Add("wss://a.example", testkit.Behaviour{})
	n.Add("wss://b.example", testkit.Behaviour{})
	slow := n.Add("wss://c.example", testkit.Behaviour{MuteQuery: func(*jsonv.Object) bool { return true }})
	relays := []string{"wss://a.example", "wss://b.example", "wss://c.example"}
	p := newPool(t, context.Background(), n, 300*time.Millisecond)
	start := time.Now()
	answers := p.QueryRelays(relays, Filter{Kinds: []int64{1}}, authKey, QueryOptions{Needed: 2, Keep: keepAll, EveryRelay: true})
	elapsed := time.Since(start)
	if len(answers) != 2 || elapsed < 250*time.Millisecond {
		t.Errorf("%d answers after %v: not waited for the third", len(answers), elapsed)
	}
	// The stalled relay's request ran out of time: failed for the call, not asked again.
	time.Sleep(100 * time.Millisecond)
	if !p.Failed("wss://c.example") {
		t.Fatal("the stalled relay is not failed")
	}
	queries := len(slow.Queries())
	answers = p.QueryRelays(relays, Filter{Kinds: []int64{1}}, authKey, QueryOptions{Needed: 2, Keep: keepAll, EveryRelay: true})
	if len(answers) != 2 || len(slow.Queries()) != queries {
		t.Errorf("%d answers; the failed relay asked again", len(answers))
	}
}

func TestFewerThanAMajority(t *testing.T) {
	n := network(t)
	n.Add("wss://a.example", testkit.Behaviour{})
	n.Down("wss://b.example")
	n.AddBlackhole("wss://c.example")
	p := newPool(t, context.Background(), n, 300*time.Millisecond)
	answers := p.QueryRelays([]string{"wss://a.example", "wss://b.example", "wss://c.example"}, Filter{Kinds: []int64{1}}, authKey, QueryOptions{Needed: 2, Keep: keepAll})
	if len(answers) != 1 {
		t.Errorf("%d answers", len(answers))
	}
	if !p.Failed("wss://b.example") || !p.Failed("wss://c.example") || p.Failed("wss://a.example") {
		t.Error("failed relays")
	}
}

func TestAQueryEndsAtItsThousandthEvent(t *testing.T) {
	n := network(t)
	r := n.Add("wss://a.example", testkit.Behaviour{})
	for i := 0; i < 1005; i++ {
		r.Store(jsonv.O("id", hex.EncodeToString([]byte{byte(i >> 8), byte(i)}), "created_at", 1000+i, "kind", 1))
	}
	p := newPool(t, context.Background(), n, 5*time.Second)
	entries, err := p.query(context.Background(), "wss://a.example", Filter{}, authKey, nil)
	if err != nil || len(entries) != proto.MaxEventsPerQuery {
		t.Errorf("%d entries, %v", len(entries), err)
	}
}

func TestAuthenticatesOncePerKeyAndChallenge(t *testing.T) {
	n := network(t)
	r := n.Add("wss://a.example", testkit.Behaviour{RequireAuth: true})
	r.StoreEvent(event(t, 100, "x"))
	p := newPool(t, context.Background(), n, 5*time.Second)
	for i := 0; i < 3; i++ {
		answers := p.QueryRelays([]string{"wss://a.example"}, Filter{Kinds: []int64{1}}, authKey, QueryOptions{Needed: 1, Keep: keepAll})
		if len(answers) != 1 || len(answers[0].Events) != 1 {
			t.Fatalf("read %d: %v", i, answers)
		}
	}
	if got := len(r.Authenticated()); got != 1 {
		t.Errorf("%d authentications", got)
	}
	// A write, authenticated with its own key.
	e := event(t, 200, "w")
	if err := p.PublishToQuorum([]string{"wss://a.example"}, e, signer, 1); err != nil {
		t.Fatal(err)
	}
	if got := r.Authenticated(); len(got) != 2 || got[1] != e.PubKey {
		t.Errorf("authenticated %v", got)
	}
}

// SPEC.md §13 (NIP-42): an authentication is sent as ["AUTH", <event>], never as an EVENT. The
// local relay refuses one sent as an EVENT, whether it asks for authentication or not: it neither
// keeps it nor authenticates its author, so that a test of the package fails if it sent one so;
// sent as AUTH, it authenticates.
func TestTheLocalRelayRefusesAnAuthenticationSentAsAnEvent(t *testing.T) {
	for _, requireAuth := range []bool{false, true} {
		n := network(t)
		url := fmt.Sprintf("wss://auth-%v.example", requireAuth)
		r := n.Add(url, testkit.Behaviour{RequireAuth: requireAuth})
		ws, err := n.Dialer().Dial(context.Background(), url, http.Header{})
		if err != nil {
			t.Fatal(err)
		}
		read := func() []any {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			data, err := ws.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			message, _ := jsonv.Parse(data)
			return message.([]any)
		}
		challenge := "none asked"
		if requireAuth {
			challenge = read()[1].(string)
		}
		auth, err := nostr.Sign(nostr.Template{CreatedAt: 1_791_378_000, Kind: proto.AuthKind,
			Tags: [][]string{{"relay", url}, {"challenge", challenge}}, Content: ""}, signer, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		send := func(kind string) []any {
			t.Helper()
			if err := ws.Write(context.Background(), jsonv.Marshal([]any{kind, auth.JSON()})); err != nil {
				t.Fatal(err)
			}
			return read()
		}
		if ok := send("EVENT"); ok[0] != "OK" || ok[2] != false {
			t.Errorf("RequireAuth %v: an authentication sent as an EVENT: %v", requireAuth, ok)
		}
		if len(r.Events()) != 0 || len(r.Authenticated()) != 0 {
			t.Errorf("RequireAuth %v: kept %d events, authenticated %v", requireAuth, len(r.Events()), r.Authenticated())
		}
		if requireAuth {
			if ok := send("AUTH"); ok[0] != "OK" || ok[2] != true {
				t.Errorf("sent as AUTH: %v", ok)
			}
			if got := r.Authenticated(); len(got) != 1 || got[0] != auth.PubKey {
				t.Errorf("authenticated %v", got)
			}
		}
		ws.Close()
	}
}

// SPEC.md §13: the wait for the OK of a NIP-42 authentication is part of the request's time. Of
// relays that ask every request to authenticate and never answer the AUTH, a read's request and a
// write's run out of time waiting for it: they ran out of time, never a refusal, and each relay
// counts as failed for the rest of the call. A later read on every relay neither asks such a relay
// again nor waits for it.
func TestARequestThatRunsOutOfTimeWaitingForItsAuthenticationFailsItsRelay(t *testing.T) {
	n := network(t)
	n.Add("wss://a.example", testkit.Behaviour{})
	n.Add("wss://b.example", testkit.Behaviour{})
	silent := func(url string) *testkit.Relay {
		return n.Add(url, testkit.Behaviour{RequireAuth: true, Raw: func(message []any, _ func(string)) bool { return message[0] == "AUTH" }})
	}
	c := silent("wss://c.example")
	silent("wss://d.example")
	const timeout = 500 * time.Millisecond
	p := newPool(t, context.Background(), n, timeout)
	start := time.Now()
	if answers := p.QueryRelays([]string{"wss://c.example"}, Filter{Kinds: []int64{1}}, authKey, QueryOptions{Needed: 1, Keep: keepAll}); len(answers) != 0 {
		t.Fatalf("%d answers", len(answers))
	}
	if elapsed := time.Since(start); elapsed < timeout {
		t.Fatalf("the read ended after %v, before its timeout", elapsed)
	}
	if len(c.Queries()) != 1 {
		t.Fatalf("%d queries: the read did not ask to authenticate", len(c.Queries()))
	}
	if !p.Failed("wss://c.example") {
		t.Error("a relay whose read ran out of time waiting for its authentication does not count as failed")
	}
	if err := p.PublishToQuorum([]string{"wss://d.example"}, event(t, 100, "x"), signer, 1); err == nil {
		t.Error("published on a relay that never took it")
	}
	if !p.Failed("wss://d.example") {
		t.Error("a relay whose write ran out of time waiting for its authentication does not count as failed")
	}
	queries := len(c.Queries())
	answers := p.QueryRelays([]string{"wss://a.example", "wss://b.example", "wss://c.example"}, Filter{Kinds: []int64{1}}, authKey,
		QueryOptions{Needed: 2, Keep: keepAll, EveryRelay: true})
	if !MajorityAnswered(answers, 3) {
		t.Errorf("%d answers", len(answers))
	}
	if len(c.Queries()) != queries {
		t.Error("the read on every relay asked again a relay that failed earlier in the call")
	}
}

func TestPublication(t *testing.T) {
	n := network(t)
	n.Add("wss://a.example", testkit.Behaviour{})
	n.Add("wss://b.example", testkit.Behaviour{Refuse: "blocked: no"})
	n.Add("wss://c.example", testkit.Behaviour{})
	p := newPool(t, context.Background(), n, 2*time.Second)
	relays := []string{"wss://a.example", "wss://b.example", "wss://c.example"}
	if err := p.PublishToQuorum(relays, event(t, 100, "x"), signer, 2); err != nil {
		t.Errorf("a majority took it: %v", err)
	}
	n.Get("wss://c.example").SetBehaviour(testkit.Behaviour{Refuse: "no"})
	if err := p.PublishToQuorum(relays, event(t, 101, "y"), signer, 2); err == nil {
		t.Error("published on one relay of three")
	}
}

// The relays past the quorum still get the event once the call returned: Release waits for them
// (each bounded by its timeout), then closes the connections.
func TestReleaseWaitsForTheRelaysPastTheQuorum(t *testing.T) {
	n := network(t)
	n.Add("wss://a.example", testkit.Behaviour{})
	n.Add("wss://b.example", testkit.Behaviour{})
	// c answers 300 ms late, after the quorum.
	c := n.Add("wss://c.example", testkit.Behaviour{OnEvent: func(*nostr.Event, *testkit.Relay) { time.Sleep(300 * time.Millisecond) }})
	p := NewPool(context.Background(), Options{Dialer: n.Dialer(), Timeout: 2 * time.Second, Now: func() int64 { return 1 }, Random: rand.Reader})
	e := event(t, 100, "x")
	if err := p.PublishToQuorum([]string{"wss://a.example", "wss://b.example", "wss://c.example"}, e, signer, 2); err != nil {
		t.Fatal(err)
	}
	p.Release()
	testkit.Eventually(t, func() bool { return len(c.Events()) == 1 }, "the event on c")
	testkit.Eventually(t, func() bool { return c.Open() == 0 }, "the connection to c closed")
}

// SPEC.md §13: a relay past the quorum finishes its own request once the call returned, NIP-42's
// one authentication and write again included, and nothing more. c asks for authentication once
// the pool was released: it gets one AUTH, then the event again, which it takes; the clock is read
// once and the random source for that AUTH alone, then the connection closes.
func TestARelayPastTheQuorumMayAuthenticateOnceTheCallReturned(t *testing.T) {
	n := network(t)
	n.Add("wss://a.example", testkit.Behaviour{})
	n.Add("wss://b.example", testkit.Behaviour{})
	released := make(chan struct{})
	var mu sync.Mutex
	var events, auths int
	c := n.Add("wss://c.example", testkit.Behaviour{Raw: func(m []any, send func(string)) bool {
		var id any
		if len(m) > 1 {
			id, _ = jsonv.Field(m[1], "id")
		}
		mu.Lock()
		defer mu.Unlock()
		switch m[0] {
		case "EVENT":
			events++
			if events == 1 {
				go func() {
					<-released
					send(relayMessage("AUTH", "challenge"))
					send(relayMessage("OK", id, false, "auth-required: sign in"))
				}()
			} else {
				send(relayMessage("OK", id, true, ""))
			}
			return true
		case "AUTH":
			auths++
			send(relayMessage("OK", id, true, ""))
			return true
		}
		return false
	}})
	var made calls
	var clock atomic.Int32
	now := func() int64 {
		made.begin()
		defer made.end()
		clock.Add(1)
		return 1_791_378_000
	}
	p := NewPool(context.Background(), Options{Dialer: n.Dialer(), Timeout: 5 * time.Second, Now: now, Random: &made})
	if err := p.PublishToQuorum([]string{"wss://a.example", "wss://b.example", "wss://c.example"}, event(t, 100, "x"), signer, 2); err != nil {
		t.Fatal(err)
	}
	p.Release()
	before, _ := made.counts()
	made.markReturned()
	close(released)
	// c may connect only once the call returned (a and b made the quorum): its authentication and
	// the event again come later, then its connection closes.
	testkit.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return events == 2 && auths == 1
	}, "c's authentication and the event again")
	testkit.Eventually(t, func() bool { return c.Open() == 0 }, "the connection to c closed")
	mu.Lock()
	defer mu.Unlock()
	if events != 2 || auths != 1 {
		t.Errorf("c got the event %d times, %d authentications", events, auths)
	}
	if got := clock.Load(); got != 1 {
		t.Errorf("the clock was read %d times", got)
	}
	if made, late := made.counts(); before != 0 || late != made || late < 2 {
		t.Errorf("%d calls of the clock and the random source before the call returned, %d after", before, late)
	}
}

// SPEC.md §13: only the relays past a publication's majority may finish their own request once
// the call returned. A publication that failed (a and b refused) ends c's at once: c asks for
// authentication only once it failed, and gets no AUTH, nor the event again. Whether c got the
// event at all depends on which came first, its write or the refusals: once, or not at all.
func TestAFailedPublicationEndsItsRequests(t *testing.T) {
	n := network(t)
	n.Add("wss://a.example", testkit.Behaviour{Refuse: "blocked: not here"})
	n.Add("wss://b.example", testkit.Behaviour{Refuse: "blocked: not here"})
	failed := make(chan struct{})
	var mu sync.Mutex
	var events, auths int
	c := n.Add("wss://c.example", testkit.Behaviour{Raw: func(m []any, send func(string)) bool {
		var id any
		if len(m) > 1 {
			id, _ = jsonv.Field(m[1], "id")
		}
		mu.Lock()
		defer mu.Unlock()
		switch m[0] {
		case "EVENT":
			events++
			go func() {
				<-failed
				send(relayMessage("AUTH", "challenge"))
				send(relayMessage("OK", id, false, "auth-required: sign in"))
			}()
			return true
		case "AUTH":
			auths++
			send(relayMessage("OK", id, true, ""))
			return true
		}
		return false
	}})
	p := newPool(t, context.Background(), n, 5*time.Second)
	err := p.PublishToQuorum([]string{"wss://a.example", "wss://b.example", "wss://c.example"}, event(t, 100, "x"), signer, 2)
	if err == nil {
		t.Fatal("the publication succeeded")
	}
	close(failed)
	p.Release()
	testkit.Eventually(t, func() bool { return c.Open() == 0 }, "the connection to c closed")
	// What c would have been sent meanwhile.
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if events > 1 || auths != 0 {
		t.Errorf("c got the event %d times, %d authentications", events, auths)
	}
}

func TestCancellationStopsEveryRequest(t *testing.T) {
	n := network(t)
	n.Add("wss://a.example", testkit.Behaviour{Mute: true})
	ctx, cancel := context.WithCancel(context.Background())
	p := newPool(t, ctx, n, 30*time.Second)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	p.QueryRelays([]string{"wss://a.example"}, Filter{}, authKey, QueryOptions{Needed: 1, Keep: keepAll})
	if err := p.PublishToQuorum([]string{"wss://a.example"}, event(t, 1, "x"), signer, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("publish: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("stopped after %v", time.Since(start))
	}
	if p.Failed("wss://a.example") {
		t.Error("a cancelled request counts as failed")
	}
}

// calls counts a pool's calls of its clock and its random source (Read): all of them, those
// running, and those that began once the read that made them returned.
type calls struct {
	mu       sync.Mutex
	made     int
	running  int
	returned bool
	late     int
}

func (c *calls) begin() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.made++
	c.running++
	if c.returned {
		c.late++
	}
}

func (c *calls) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running--
}

// markReturned marks the read returned: how many calls ran then.
func (c *calls) markReturned() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.returned = true
	return c.running
}

func (c *calls) counts() (made, late int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.made, c.late
}

func (c *calls) Read(b []byte) (int, error) {
	c.begin()
	defer c.end()
	return rand.Read(b)
}

// relayMessage is a relay's message v, as it sends it.
func relayMessage(v ...any) string { return string(jsonv.Marshal(v)) }

// SPEC.md §13: a read goes on once a majority answered, and drops the others; nothing it started
// runs on once it returned (the call may return next, and nothing a call started may start
// anything then). Of a relay whose request is still authenticating when the majority answers, its
// clock still being read (on its first page, or on a later one): the read returns once that
// request stopped, and calls neither the clock nor the random source once it returned. Without the
// wait, the read returned while the authentication was running, and its signature drew random
// bytes afterwards.
func TestAReadReturnsOnceTheRelaysItWentOnWithoutStopped(t *testing.T) {
	relays := []string{"wss://a.example", "wss://b.example", "wss://c.example"}
	filter := Filter{Kinds: []int64{1}}
	for _, c := range []struct {
		name string
		// pages is how many pages c answers before it asks for authentication.
		pages int
		read  func(p *Pool) int
	}{
		{"not paged", 0, func(p *Pool) int {
			return len(p.QueryRelays(relays, filter, authKey, QueryOptions{Needed: 2, Keep: keepAll}))
		}},
		{"paged, on its second page", 1, func(p *Pool) int {
			return len(p.QueryRelays(relays, filter, authKey, QueryOptions{Needed: 2, Paged: true, Keep: keepAll}))
		}},
		{"complete", 0, func(p *Pool) int { return len(p.QueryComplete(relays, filter, authKey, 2, keepAll, false).Answers) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := network(t)
			var made calls
			// The clock is read by c's authentication alone, which waits there until released.
			clockRead, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			now := func() int64 {
				made.begin()
				defer made.end()
				once.Do(func() { close(clockRead) })
				<-release
				return 1_791_378_000
			}
			// a and b answer once c's authentication reads the clock: a majority, c still reading.
			later := func(m []any, send func(string)) bool {
				if m[0] != "REQ" {
					return false
				}
				go func() {
					<-clockRead
					send(relayMessage("EOSE", m[1]))
				}()
				return true
			}
			n.Add("wss://a.example", testkit.Behaviour{Raw: later})
			n.Add("wss://b.example", testkit.Behaviour{Raw: later})
			var asked, auths atomic.Int32
			n.Add("wss://c.example", testkit.Behaviour{Raw: func(m []any, send func(string)) bool {
				switch m[0] {
				case "AUTH":
					// Never answered.
					auths.Add(1)
					return true
				case "REQ":
					if int(asked.Add(1)) <= c.pages {
						send(relayMessage("EVENT", m[1], jsonv.O("id", strings.Repeat("ab", 32), "created_at", 100, "kind", 1)))
						send(relayMessage("EOSE", m[1]))
						return true
					}
					send(relayMessage("AUTH", "challenge"))
					send(relayMessage("CLOSED", m[1], "auth-required: sign in"))
					return true
				}
				return false
			}})
			p := NewPool(context.Background(), Options{Dialer: n.Dialer(), Timeout: 10 * time.Second, Now: now, Random: &made})
			t.Cleanup(p.Close)
			returned := make(chan struct{})
			go func() {
				<-clockRead
				// Released once the read returned, or a while after the majority answered, the read
				// waiting for c's request.
				select {
				case <-returned:
				case <-time.After(500 * time.Millisecond):
				}
				close(release)
			}()
			answers := c.read(p)
			running := made.markReturned()
			close(returned)
			if answers != 2 {
				t.Errorf("%d answers", answers)
			}
			if running != 0 {
				t.Errorf("the read returned while %d calls of its clock or random source ran", running)
			}
			// Released, c's authentication went on as far as its AUTH, its read gone on without it.
			testkit.Eventually(t, func() bool { return auths.Load() == 1 }, "c's AUTH")
			if _, late := made.counts(); late != 0 {
				t.Errorf("%d calls of the clock or the random source once the read returned", late)
			}
			if int(asked.Load()) != c.pages+1 {
				t.Errorf("c was asked %d queries", asked.Load())
			}
			if p.Failed("wss://c.example") {
				t.Error("a dropped relay counts as failed")
			}
		})
	}
}

// SPEC.md §13: the relays a read went on without ask nothing more. A relay that answers once the
// read went on, with a page that would have it page on, or asking for authentication: it gets
// nothing more for that read but the CLOSE of its subscription, and the random source is not
// called for it.
func TestTheRelaysAReadWentOnWithoutAskNothingMore(t *testing.T) {
	relays := []string{"wss://a.example", "wss://b.example", "wss://c.example"}
	for _, c := range []struct {
		name string
		late func(subscription any) []string
	}{
		{"a page that would page on", func(s any) []string {
			return []string{relayMessage("EVENT", s, jsonv.O("id", strings.Repeat("ab", 32), "created_at", 100, "kind", 1)), relayMessage("EOSE", s)}
		}},
		{"asking for authentication", func(s any) []string {
			return []string{relayMessage("AUTH", "challenge"), relayMessage("CLOSED", s, "auth-required: sign in")}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := network(t)
			// a and b answer once c got its query.
			cAsked := make(chan struct{})
			later := func(m []any, send func(string)) bool {
				if m[0] != "REQ" {
					return false
				}
				go func() {
					<-cAsked
					send(relayMessage("EOSE", m[1]))
				}()
				return true
			}
			n.Add("wss://a.example", testkit.Behaviour{Raw: later})
			n.Add("wss://b.example", testkit.Behaviour{Raw: later})
			var mu sync.Mutex
			var queries, auths int
			var answer func()
			cRelay := n.Add("wss://c.example", testkit.Behaviour{Raw: func(m []any, send func(string)) bool {
				mu.Lock()
				defer mu.Unlock()
				switch m[0] {
				case "AUTH":
					auths++
					return true
				case "REQ":
					queries++
					if answer == nil {
						subscription := m[1]
						answer = func() {
							for _, text := range c.late(subscription) {
								send(text)
							}
						}
						close(cAsked)
					}
					return true
				}
				return false
			}})
			var made calls
			p := NewPool(context.Background(), Options{Dialer: n.Dialer(), Timeout: 10 * time.Second, Now: func() int64 { return 1_791_378_000 }, Random: &made})
			t.Cleanup(p.Close)
			answers := p.QueryRelays(relays, Filter{Kinds: []int64{1}}, authKey, QueryOptions{Needed: 2, Paged: true, Keep: keepAll})
			if len(answers) != 2 {
				t.Fatalf("%d answers", len(answers))
			}
			made.markReturned()
			mu.Lock()
			lateAnswer := answer
			mu.Unlock()
			lateAnswer()
			testkit.Eventually(t, func() bool { return len(cRelay.ClosedSubscriptions()) == 1 }, "c's subscription closed")
			// Time for anything more to come.
			time.Sleep(200 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			if queries != 1 || auths != 0 {
				t.Errorf("c got %d queries and %d authentications", queries, auths)
			}
			if _, late := made.counts(); late != 0 {
				t.Errorf("the random source was called %d times once the read went on", late)
			}
		})
	}
}

// A request dropped before it starts (its read went on, or its call was cancelled) starts nothing:
// it opens no connection, and draws no subscription id nor sends a query, an authentication done
// or not.
func TestARequestWhoseContextIsDoneStartsNothing(t *testing.T) {
	n := network(t)
	a := n.Add("wss://a.example", testkit.Behaviour{})
	n.Add("wss://b.example", testkit.Behaviour{})
	var made calls
	p := NewPool(context.Background(), Options{Dialer: n.Dialer(), Timeout: 5 * time.Second, Now: func() int64 { return 1_791_378_000 }, Random: &made})
	t.Cleanup(p.Close)
	// a's connection, open for the call's next requests.
	if _, err := p.query(context.Background(), "wss://a.example", Filter{}, authKey, nil); err != nil {
		t.Fatal(err)
	}
	drawn, _ := made.counts()
	dropped, drop := context.WithCancel(context.Background())
	drop()
	if _, err := p.query(dropped, "wss://b.example", Filter{}, authKey, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("a query to a relay not connected: %v", err)
	}
	c, _ := p.connection("wss://a.example")
	if _, err := p.ask(dropped, c, Filter{}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("a query over an open connection (after an authentication, say): %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := n.Connections("wss://b.example"); got != 0 {
		t.Errorf("%d connections to b", got)
	}
	if got := len(a.Subscriptions()); got != 1 {
		t.Errorf("a got %d queries", got)
	}
	if made, _ := made.counts(); made != drawn {
		t.Errorf("%d random draws for dropped requests", made-drawn)
	}
	if p.Failed("wss://b.example") {
		t.Error("a dropped request counts as failed")
	}
}

// heedless is a connection that keeps what the pool writes on it and reads nothing until it is
// closed, whatever their context: what the pool itself sends, whatever its connections heed.
type heedless struct {
	mu      sync.Mutex
	written []string
	once    sync.Once
	closed  chan struct{}
}

func newHeedless() *heedless { return &heedless{closed: make(chan struct{})} }

func (h *heedless) Read(context.Context) ([]byte, error) {
	<-h.closed
	return nil, errors.New("closed")
}

func (h *heedless) Write(_ context.Context, data []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.written = append(h.written, string(data))
	return nil
}

func (h *heedless) Close() error {
	h.once.Do(func() { close(h.closed) })
	return nil
}

func (h *heedless) Written() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.written...)
}

// dialing gives its connection to each dial, and counts them.
type dialing struct {
	conn  *heedless
	mu    sync.Mutex
	dials int
}

func (d *dialing) Dial(context.Context, string, http.Header) (Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials++
	return d.conn, nil
}

func (d *dialing) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

// SPEC.md §4: a cancelled call stops its requests. Once it is, a publication sends its event to no
// relay, nor sends it again once authenticated, even over a connection still open.
func TestACancelledCallPublishesNothing(t *testing.T) {
	conn := newHeedless()
	ctx, cancel := context.WithCancel(context.Background())
	p := NewPool(ctx, Options{Dialer: &dialing{conn: conn}, Timeout: 5 * time.Second, Now: func() int64 { return 1 }, Random: rand.Reader})
	t.Cleanup(p.Close)
	c, _ := p.connection("wss://a.example")
	<-c.opened
	cancel()
	e := event(t, 100, "x")
	if err := p.PublishToQuorum([]string{"wss://a.example"}, e, signer, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("published: %v", err)
	}
	if _, _, err := p.write(ctx, c, e); !errors.Is(err, context.Canceled) {
		t.Errorf("written again: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if written := conn.Written(); len(written) != 0 {
		t.Errorf("sent %v", written)
	}
}

// A connection made during a call whose goroutine runs only once its call returned (the pool
// released) is dialed only for a publication's relay past the majority, its event queued; never
// for nothing, nor once the pool is closed or its call cancelled: nothing else is asked of the
// dialer once the call returned.
func TestAConnectionIsDialedOnceItsCallReturnedOnlyForAPublication(t *testing.T) {
	for _, c := range []struct {
		name      string
		published bool
		end       func(p *Pool, cancel context.CancelFunc)
		dials     int
	}{
		{"released", false, func(p *Pool, _ context.CancelFunc) { p.Release() }, 0},
		{"released, a publication's event queued", true, func(p *Pool, _ context.CancelFunc) { p.Release() }, 1},
		{"closed, a publication's event queued", true, func(p *Pool, _ context.CancelFunc) { p.Close() }, 0},
		{"its call cancelled, a publication's event queued", true, func(_ *Pool, cancel context.CancelFunc) { cancel() }, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn := newHeedless()
			d := &dialing{conn: conn}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := NewPool(ctx, Options{Dialer: d, Timeout: time.Second, Now: func() int64 { return 1 }, Random: rand.Reader})
			defer p.Close()
			late := newConn("wss://a.example")
			if c.published {
				// As PublishToQuorum does: counted (Release waits for it), its event queued.
				if !p.begin() {
					t.Fatal("the pool takes no request")
				}
				defer p.pending.Done()
				late.startWrite(event(t, 100, "x")).stop()
			}
			c.end(p, cancel)
			ran := make(chan struct{})
			go func() {
				defer close(ran)
				late.run(p)
			}()
			if c.dials == 0 {
				select {
				case <-late.dead:
				case <-time.After(time.Second):
				}
				if !late.isDead() {
					t.Error("the connection did not end")
				}
			} else {
				select {
				case <-late.opened:
				case <-time.After(10 * time.Second):
					t.Error("the connection did not open")
				}
				testkit.Eventually(t, func() bool { return len(conn.Written()) == 1 }, "the event written")
			}
			if n := d.count(); n != c.dials {
				t.Errorf("%d dials", n)
			}
			late.die(errClosed)
			<-ran
		})
	}
}

// SPEC.md §13 and §15: the handshake carries the User-Agent, offers no compression, follows no
// redirect; a message larger than 1 MiB ends the connection.
func TestTheHandshake(t *testing.T) {
	n := network(t)
	r := n.Add("wss://a.example", testkit.Behaviour{})
	p := newPool(t, context.Background(), n, 2*time.Second)
	p.QueryRelays([]string{"wss://a.example"}, Filter{}, authKey, QueryOptions{Needed: 1, Keep: keepAll})
	h := r.Handshakes()
	if len(h) != 1 || h[0].Get("User-Agent") != "sattleme-Checkout/1" || h[0].Get("Sec-WebSocket-Extensions") != "" {
		t.Errorf("handshake headers %v", h)
	}

	target := n.Add("wss://target.example", testkit.Behaviour{})
	n.AddServer("wss://moved.example", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "https://target.example/", http.StatusMovedPermanently)
	}))
	answers := p.QueryRelays([]string{"wss://moved.example"}, Filter{}, authKey, QueryOptions{Needed: 1, Keep: keepAll})
	if len(answers) != 0 || !p.Failed("wss://moved.example") || n.Connections("wss://target.example") != 0 || len(target.Queries()) != 0 {
		t.Errorf("a redirect followed: %d answers, %d connections", len(answers), n.Connections("wss://target.example"))
	}

	n.AddServer("wss://big.example", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_, data, err := c.Read(req.Context())
		if err != nil {
			return
		}
		message, _ := jsonv.Parse(data)
		id := message.([]any)[1]
		huge := string(jsonv.Marshal([]any{"EVENT", id, jsonv.O("content", strings.Repeat("x", 1<<20))}))
		_ = c.Write(req.Context(), websocket.MessageText, []byte(huge))
		_ = c.Write(req.Context(), websocket.MessageText, jsonv.Marshal([]any{"EOSE", id}))
		time.Sleep(time.Second)
	}))
	if answers := p.QueryRelays([]string{"wss://big.example"}, Filter{}, authKey, QueryOptions{Needed: 1, Keep: keepAll}); len(answers) != 0 || !p.Failed("wss://big.example") {
		t.Errorf("a message above 1 MiB read: %v", answers)
	}
}

// relay-reads.json: what a complete read keeps of one relay's pages, and asks next.
func TestRelayReadVectors(t *testing.T) {
	file := tv.Load(t, "relay-reads.json")
	if tv.Int(t, file, "limit") != proto.CompleteReadLimit || tv.Int(t, file, "pages") != proto.MaxPages {
		t.Error("limit and pages")
	}
	cases := tv.Objs(t, file, "cases")
	for _, c := range cases {
		pages := tv.Arr(t, c, "pages")
		seen := map[string]bool{}
		kept := 0
		untils := []any{nil}
		answered, complete := false, false
		for page := 1; ; page++ {
			answer := pages[page-1]
			if answer == "fails" {
				answered = page > 1
				break
			}
			var entries []Entry
			for _, item := range answer.([]any) {
				pair := item.([]any)
				entries = append(entries, EntryOf(jsonv.O("id", pair[0], "created_at", pair[1]).Object(), keepAll, nil))
			}
			next := AfterCompletePage(entries, seen, func(any) { kept++ }, page)
			if next.Done {
				answered, complete = true, next.Complete
				break
			}
			untils = append(untils, next.Until)
		}
		got := jsonv.O("untils", untils, "answered", answered, "complete", complete, "kept", kept)
		if g, w := string(jsonv.Marshal(got)), string(jsonv.Marshal(tv.Get(c, "expected"))); g != w {
			t.Errorf("%s: got %s, want %s", tv.Str(t, c, "name"), g, w)
		}
	}
	// Each relay's end as QueryComplete records it (one that failed is not), and the decision its
	// callers read.
	lists := tv.Objs(t, file, "lists")
	for _, c := range lists {
		relays := tv.Arr(t, c, "relays")
		var read CompleteAnswers
		for i, state := range relays {
			relay := fmt.Sprintf("wss://%d.example", i)
			switch state {
			case "complete":
				read.ended(relay, nil, true)
			case "incomplete":
				read.ended(relay, nil, false)
			case "failed":
			default:
				t.Fatalf("%v", state)
			}
		}
		if got := read.MajorityComplete(len(relays)); got != (tv.Get(c, "expected") == true) {
			t.Errorf("%s: %v", tv.Str(t, c, "name"), got)
		}
	}
	t.Logf("%d cases, %d lists", len(cases), len(lists))
}

// relay-reads.json end to end: a relay answering each page the vector gives, read by the pool;
// the untils it asks, and what it keeps.
func TestRelayReadVectorsOverARelay(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "relay-reads.json"), "cases")
	for _, c := range cases {
		name := tv.Str(t, c, "name")
		pages := tv.Arr(t, c, "pages")
		n := testkit.NewNet()
		var mu sync.Mutex
		var untils []any
		page := 0
		n.Add("wss://a.example", testkit.Behaviour{Raw: func(message []any, send func(string)) bool {
			if message[0] != "REQ" {
				return false
			}
			filter, _ := jsonv.AsObject(message[2])
			until, _ := filter.Get("until")
			mu.Lock()
			untils = append(untils, until)
			answer := pages[page]
			page++
			mu.Unlock()
			if answer == "fails" {
				send(string(jsonv.Marshal([]any{"CLOSED", message[1], "error: failed"})))
				return true
			}
			for _, item := range answer.([]any) {
				pair := item.([]any)
				send(string(jsonv.Marshal([]any{"EVENT", message[1], jsonv.O("id", pair[0], "created_at", pair[1])})))
			}
			send(string(jsonv.Marshal([]any{"EOSE", message[1]})))
			return true
		}})
		p := newPool(t, context.Background(), n, 5*time.Second)
		read := p.QueryComplete([]string{"wss://a.example"}, Filter{Kinds: []int64{proto.RecordKind}}, authKey, 1, keepAll, false)
		expected := tv.Obj(t, c, "expected")
		kept := 0
		for _, a := range read.Answers {
			kept += len(a.Events)
		}
		got := jsonv.O("untils", untils, "answered", len(read.Answers) == 1, "complete", read.Complete == 1, "kept", kept)
		if g, w := string(jsonv.Marshal(got)), string(jsonv.Marshal(expected)); g != w {
			t.Errorf("%s:\ngot  %s\nwant %s", name, g, w)
		}
		p.Close()
		n.Close()
	}
}

// entriesOf is a vector's page as a read takes it: each [id, created_at] an event holding them.
func entriesOf(answer any) []Entry {
	var entries []Entry
	for _, item := range answer.([]any) {
		pair := item.([]any)
		// Every event kept: the vector counts each id once, whatever opens.
		entries = append(entries, EntryOf(jsonv.O("id", pair[0], "created_at", pair[1]).Object(), keepAll, nil))
	}
	return entries
}

// relay-reads.json, lists, end to end (PORTING.md, step 4): local relays whose complete read ends as
// each one says (complete: a page that is not full; incomplete: a page of more events than asked;
// failed: its first page refused), read by the pool's complete read; whether it is complete, as
// the pool's callers read it.
func TestRelayReadListVectorsOverRelays(t *testing.T) {
	lists := tv.Objs(t, tv.Load(t, "relay-reads.json"), "lists")
	for _, c := range lists {
		states := tv.Arr(t, c, "relays")
		n := testkit.NewNet()
		var relays []string
		for i, state := range states {
			url := fmt.Sprintf("wss://%d.example", i)
			relays = append(relays, url)
			n.Add(url, testkit.Behaviour{Raw: func(message []any, send func(string)) bool {
				if message[0] != "REQ" {
					return message[0] == "CLOSE"
				}
				events := 0
				switch state {
				case "complete":
					events = 1
				case "incomplete":
					events = proto.CompleteReadLimit + 1
				case "failed":
					send(string(jsonv.Marshal([]any{"CLOSED", message[1], "error: failed"})))
					return true
				default:
					t.Errorf("%v", state)
				}
				for e := 0; e < events; e++ {
					send(string(jsonv.Marshal([]any{"EVENT", message[1], jsonv.O("id", fmt.Sprintf("%064x", e), "created_at", 1000)})))
				}
				send(string(jsonv.Marshal([]any{"EOSE", message[1]})))
				return true
			}})
		}
		p := newPool(t, context.Background(), n, 5*time.Second)
		read := p.QueryComplete(relays, Filter{Kinds: []int64{proto.RecordKind}}, authKey, Majority(len(relays)), keepAll, false)
		if got := read.MajorityComplete(len(relays)); got != (tv.Get(c, "expected") == true) {
			t.Errorf("%s: complete %v", tv.Str(t, c, "name"), got)
		}
		p.Close()
		n.Close()
	}
	t.Logf("%d lists", len(lists))
}

// relay-reads.json, paged: what the pool's paged read takes of one relay's pages, each page
// through AfterPagedPage, at most MaxPages; a page that fails ends the read with what it read, the
// first one the relay's failure.
func TestPagedReadVectors(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "relay-reads.json"), "paged")
	for _, c := range cases {
		pages := tv.Arr(t, c, "pages")
		seen := map[string]bool{}
		kept := 0
		untils := []any{nil}
		answered := true
		for page := 0; page < proto.MaxPages; page++ {
			answer := pages[page]
			if answer == "fails" {
				answered = page > 0
				break
			}
			next, more := AfterPagedPage(entriesOf(answer), seen, func(any) { kept++ })
			if !more {
				break
			}
			if page+1 < proto.MaxPages {
				untils = append(untils, next)
			}
		}
		got := jsonv.O("untils", untils, "answered", answered, "kept", kept)
		if g, w := string(jsonv.Marshal(got)), string(jsonv.Marshal(tv.Get(c, "expected"))); g != w {
			t.Errorf("%s: got %s, want %s", tv.Str(t, c, "name"), g, w)
		}
	}
	t.Logf("%d cases", len(cases))
}

// pagesRelay is a relay answering the n-th query with the n-th page of pages (an empty one past
// them), "fails" with CLOSED; the untils it was asked, in order.
func pagesRelay(n *testkit.Net, url string, pages []any) func() []any {
	var mu sync.Mutex
	var untils []any
	n.Add(url, testkit.Behaviour{Raw: func(message []any, send func(string)) bool {
		if message[0] != "REQ" {
			return message[0] == "CLOSE"
		}
		filter, _ := jsonv.AsObject(message[2])
		until, _ := filter.Get("until")
		mu.Lock()
		var answer any = []any{}
		if len(untils) < len(pages) {
			answer = pages[len(untils)]
		}
		untils = append(untils, until)
		mu.Unlock()
		if answer == "fails" {
			send(string(jsonv.Marshal([]any{"CLOSED", message[1], "error: failed"})))
			return true
		}
		for _, item := range answer.([]any) {
			pair := item.([]any)
			send(string(jsonv.Marshal([]any{"EVENT", message[1], jsonv.O("id", pair[0], "created_at", pair[1])})))
		}
		send(string(jsonv.Marshal([]any{"EOSE", message[1]})))
		return true
	}})
	return func() []any {
		mu.Lock()
		defer mu.Unlock()
		return append([]any(nil), untils...)
	}
}

// relay-reads.json, paged, end to end: a relay answering each page the vector gives, read by the
// pool's paged read; the untils it asks, whether the relay answered, and what the read keeps.
func TestPagedReadVectorsOverARelay(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "relay-reads.json"), "paged")
	for _, c := range cases {
		n := testkit.NewNet()
		untils := pagesRelay(n, "wss://a.example", tv.Arr(t, c, "pages"))
		p := newPool(t, context.Background(), n, 5*time.Second)
		answers := p.QueryRelays([]string{"wss://a.example"}, Filter{Kinds: []int64{proto.RecordKind}}, authKey, QueryOptions{Needed: 1, Paged: true, Keep: keepAll})
		kept := 0
		for _, a := range answers {
			kept += len(a.Events)
		}
		got := jsonv.O("untils", untils(), "answered", MajorityAnswered(answers, 1), "kept", kept)
		if g, w := string(jsonv.Marshal(got)), string(jsonv.Marshal(tv.Get(c, "expected"))); g != w {
			t.Errorf("%s:\ngot  %s\nwant %s", tv.Str(t, c, "name"), g, w)
		}
		p.Close()
		n.Close()
	}
}

// relay-reads.json, onEveryRelay: of a read on every relay, only the relays whose read ended
// answered, never one still reading, whatever it sent; complete when a majority read completely.
// Each relay's end as the pool records it, and the pool's decisions.
func TestReadsOnEveryRelayVectors(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "relay-reads.json"), "onEveryRelay")
	for _, c := range cases {
		states := tv.Arr(t, c, "relays")
		var read CompleteAnswers
		for i, state := range states {
			relay := fmt.Sprintf("wss://%d.example", i)
			events := []any{jsonv.O("id", strconv.Itoa(i))}
			switch state {
			case "complete":
				read.ended(relay, events, true)
			case "incomplete":
				read.ended(relay, events, false)
			case "reading":
				read.stillReading(relay, events)
			case "failed":
			default:
				t.Fatalf("%v", state)
			}
		}
		got := jsonv.O("answered", read.MajorityAnswered(len(states)), "complete", read.MajorityComplete(len(states)))
		// The pages of a relay still reading are kept all the same.
		if len(read.Answers) != len(states)-strings.Count(fmt.Sprint(states), "failed") {
			t.Errorf("%s: %d answers kept", tv.Str(t, c, "name"), len(read.Answers))
		}
		if g, w := string(jsonv.Marshal(got)), string(jsonv.Marshal(tv.Get(c, "expected"))); g != w {
			t.Errorf("%s: got %s, want %s", tv.Str(t, c, "name"), g, w)
		}
	}
	t.Logf("%d cases", len(cases))
}

// relay-reads.json, onEveryRelay, end to end (PORTING.md, steps 4 and 5): each case's relays made
// by local relays that read as its states say, and read by the pool's complete read on every relay
// (a lease's confirmation), the pool telling apart a relay that read completely, one whose read
// ended incomplete, one that failed and one still reading. TestReadsOnEveryRelayVectors runs the
// same cases through the functions the pool decides with. What each relay does, by its state (in
// turn, so that a state is made more than one way):
//   - complete: a few events (one page, not full); 250 at distinct seconds (it pages: 100, 100,
//     then 52); or 250 capped at 40 an answer (a page that is not full: complete).
//   - incomplete: 150 events of one second (a full page, then the same page: nothing new); or 2 100
//     at distinct seconds (a 20th page that is full).
//   - failed: no relay at its name (the connection refused).
//   - reading: still reading when the read goes on: 2 100 events at distinct seconds, each page
//     answered a quarter of the timeout late (its read would last five timeouts); or a mute relay,
//     when the read goes on before its first page runs out of time.
//
// Of seven cases, fewer than a majority read completely while a majority still can, relays still
// reading: the pool never goes on then (SPEC.md §13), it waits for them, so no local relay is still
// reading when it does. They run with mute relays in place of those still reading, which fail at
// their timeout, and give what the case expects, as relay-reads.json says: a relay still reading
// has neither answered nor read completely.
func TestReadsOnEveryRelayVectorsOverRelays(t *testing.T) {
	const timeout = 2 * time.Second
	const now = 1_791_378_000
	d := strings.Repeat("d", 64)
	since := int64(now - 7200)
	serial := 0
	// events are count events at the address d, anybody's, the newest at now; of one second each,
	// or all of now.
	events := func(count int, oneSecond bool) []*jsonv.Object {
		list := make([]*jsonv.Object, count)
		for i := range list {
			createdAt := now - i
			if oneSecond {
				createdAt = now
			}
			list[i] = jsonv.O("id", fmt.Sprintf("%064x", serial), "pubkey", strings.Repeat("ab", 32), "created_at", createdAt,
				"kind", proto.RecordKind, "tags", [][]string{{"d", d}}, "content", "", "sig", strings.Repeat("00", 64)).Object()
			serial++
		}
		return list
	}
	hold := func(n *testkit.Net, url string, held []*jsonv.Object, b testkit.Behaviour) *testkit.Relay {
		r := n.Add(url, b)
		for _, e := range held {
			r.Store(e)
		}
		return r
	}
	makers := map[string]func(n *testkit.Net, url string) *testkit.Relay{
		"few": func(n *testkit.Net, url string) *testkit.Relay {
			return hold(n, url, events(3, false), testkit.Behaviour{})
		},
		"pages": func(n *testkit.Net, url string) *testkit.Relay {
			return hold(n, url, events(250, false), testkit.Behaviour{})
		},
		"caps": func(n *testkit.Net, url string) *testkit.Relay {
			return hold(n, url, events(250, false), testkit.Behaviour{Cap: 40})
		},
		"oneSecond": func(n *testkit.Net, url string) *testkit.Relay {
			return hold(n, url, events(150, true), testkit.Behaviour{})
		},
		"twentyPages": func(n *testkit.Net, url string) *testkit.Relay {
			return hold(n, url, events(2100, false), testkit.Behaviour{})
		},
		"refused": func(*testkit.Net, string) *testkit.Relay { return nil },
		"mute":    func(n *testkit.Net, url string) *testkit.Relay { return n.Add(url, testkit.Behaviour{Mute: true}) },
		"slowPager": func(n *testkit.Net, url string) *testkit.Relay {
			return hold(n, url, events(2100, false), testkit.Behaviour{QueryDelay: timeout / 4})
		},
	}
	turns, used := map[string]int{}, map[string]bool{}
	next := func(names ...string) string {
		key := strings.Join(names, ",")
		name := names[turns[key]%len(names)]
		turns[key]++
		used[name] = true
		return name
	}
	// goesOnWith is when the pool goes on with a complete read on every relay whose relays' reads
	// are states: once none is still reading ("settled"); with some still reading, a timeout after
	// a majority read completely ("grace"), or as soon as a majority can no longer read completely
	// ("at once"); "" when it never does, waiting for the relays still reading.
	goesOnWith := func(states []any) string {
		count := func(state string) int {
			n := 0
			for _, s := range states {
				if s == state {
					n++
				}
			}
			return n
		}
		needed := Majority(len(states))
		switch {
		case count("reading") == 0:
			return "settled"
		case count("complete") >= needed:
			return "grace"
		case count("complete")+count("reading") < needed:
			return "at once"
		}
		return ""
	}
	cases := tv.Objs(t, tv.Load(t, "relay-reads.json"), "onEveryRelay")
	var producible, waited []*jsonv.Object
	for _, c := range cases {
		if goesOnWith(tv.Arr(t, c, "relays")) == "" {
			waited = append(waited, c)
		} else {
			producible = append(producible, c)
		}
	}
	kinds := map[string]bool{}
	run := func(c *jsonv.Object) {
		states := tv.Arr(t, c, "relays")
		when := goesOnWith(states)
		kinds[when] = true
		var names []string
		for _, state := range states {
			switch {
			case state == "complete":
				names = append(names, next("few", "pages", "caps"))
			case state == "incomplete":
				names = append(names, next("oneSecond", "twentyPages"))
			case state == "failed":
				names = append(names, next("refused"))
			case state == "reading" && when == "grace":
				names = append(names, next("slowPager"))
			case state == "reading":
				names = append(names, next("mute"))
			default:
				t.Fatalf("%v", state)
			}
		}
		t.Run(tv.Str(t, c, "name"), func(t *testing.T) {
			n := testkit.NewNet()
			defer n.Close()
			var urls []string
			made := map[string]*testkit.Relay{}
			for i, name := range names {
				url := fmt.Sprintf("wss://%s-%d.example", strings.ToLower(name), i)
				urls = append(urls, url)
				made[url] = makers[name](n, url)
			}
			p := newPool(t, context.Background(), n, timeout)
			started := time.Now()
			read := p.QueryComplete(urls, Filter{Kinds: []int64{proto.RecordKind}, D: []string{d}, Since: &since}, authKey, Majority(len(urls)), keepAll, true)
			elapsed := time.Since(started)
			got := jsonv.O("answered", read.MajorityAnswered(len(urls)), "complete", read.MajorityComplete(len(urls)))
			if g, w := string(jsonv.Marshal(got)), string(jsonv.Marshal(tv.Get(c, "expected"))); g != w {
				t.Errorf("got %s, want %s (%v)", g, w, names)
			}
			switch when {
			case "grace":
				// The relays still reading were waited for a timeout after the majority, not until
				// they end.
				if elapsed < timeout-50*time.Millisecond {
					t.Errorf("went on after %v, before a timeout", elapsed)
				}
				for i, state := range states {
					if state == "reading" && len(made[urls[i]].Queries()) >= proto.MaxPages {
						t.Errorf("%s read to its end", urls[i])
					}
				}
			case "at once":
				// Before a mute relay's first page could run out of time: it was still reading.
				if elapsed >= timeout-100*time.Millisecond {
					t.Errorf("went on after %v, not at once", elapsed)
				}
			case "":
				// Waited for until the relays in place of those still reading failed.
				if elapsed < timeout-50*time.Millisecond {
					t.Errorf("went on after %v, before they failed", elapsed)
				}
			}
		})
	}
	for _, c := range producible {
		run(c)
	}
	for _, c := range waited {
		run(c)
	}
	for _, kind := range []string{"settled", "grace", "at once", ""} {
		if !kinds[kind] {
			t.Errorf("no case that goes on %q", kind)
		}
	}
	for name := range makers {
		if !used[name] {
			t.Errorf("no relay made %s", name)
		}
	}
	t.Logf("%d cases, %d of which waited for", len(cases), len(waited))
}

// heavyReadTimeout bounds each request of the reads of 18 MB below: what they test is what is kept,
// not how fast, and with the race detector on a busy machine one page of 300 events of 60 kB each
// took more than 5 s.
const heavyReadTimeout = 60 * time.Second

// Of what a relay sends, a read keeps only what Keep keeps, and still pages through the rest: a
// relay cannot make the server's memory grow (SPEC.md §13, "Limits").
func TestAReadKeepsOnlyWhatItKeepsAndPagesThroughTheRest(t *testing.T) {
	n := network(t)
	r := n.Add("wss://a.example", testkit.Behaviour{})
	filler := strings.Repeat("x", 60_000)
	for i := 0; i < 300; i++ {
		// As a relay holds it: JSON, read back.
		r.Store(parsed(jsonv.O(
			"id", fmt.Sprintf("%064x", i),
			"pubkey", strings.Repeat("ab", 32),
			"created_at", 1_000_000-i,
			"kind", 30078,
			"tags", [][]string{{"d", strings.Repeat("cd", 32)}},
			"content", filler,
			"sig", strings.Repeat("00", 64),
		)))
	}
	wanted := strings.Repeat("0", 62) + "ff"
	keep := func(v any) (any, bool) {
		if id, _ := jsonv.Field(v, "id"); id == wanted {
			return v, true
		}
		return nil, false
	}
	for _, paged := range []bool{false, true} {
		p := newPool(t, context.Background(), n, heavyReadTimeout)
		answers := p.QueryRelays([]string{"wss://a.example"}, Filter{Kinds: []int64{30078}}, authKey, QueryOptions{Needed: 1, Paged: paged, Keep: keep})
		if len(answers) != 1 || len(answers[0].Events) != 1 {
			t.Fatalf("paged %v: %d answers", paged, len(answers))
		}
		if content, _ := jsonv.Field(answers[0].Events[0], "content"); content != filler {
			t.Errorf("paged %v: the event kept is not whole", paged)
		}
	}
	// A complete read: 300 events in pages of 100, one kept; the read complete.
	p := newPool(t, context.Background(), n, heavyReadTimeout)
	complete := p.QueryComplete([]string{"wss://a.example"}, Filter{Kinds: []int64{30078}}, authKey, 1, keep, false)
	if complete.Complete != 1 || len(complete.Answers) != 1 || len(complete.Answers[0].Events) != 1 {
		t.Errorf("complete %d, %d answers", complete.Complete, len(complete.Answers))
	}
	limited := 0
	for _, q := range r.Queries() {
		if limit, _ := q.Get("limit"); limit == float64(proto.CompleteReadLimit) {
			limited++
		}
	}
	if limited != 4 {
		t.Errorf("%d pages of 100", limited)
	}
}

func parsed(v any) any {
	out, err := jsonv.Parse(jsonv.Marshal(v))
	if err != nil {
		panic(err)
	}
	return out
}

// SPEC.md §13, "Paged reads": the next page goes on from what the last page held, as it came: an
// event with a string id and a safe integer created_at, whether it checks out or not. A relay that
// sends one event per page, the newest first: one that does not check out, then one that does.
func TestAPagedReadGoesOnFromEventsThatDoNotCheckOut(t *testing.T) {
	n := network(t)
	r := n.Add("wss://a.example", testkit.Behaviour{Cap: 1})
	valid := event(t, 100, "valid")
	forged := testkit.EventValue(event(t, 200, "forged")).(*jsonv.Object)
	forged.Set("sig", strings.Repeat("00", 64))
	r.StoreEvent(valid)
	r.Store(forged)
	p := newPool(t, context.Background(), n, 2*time.Second)
	onlyValid := func(v any) (any, bool) {
		e, ok := nostr.Valid(v)
		return e, ok
	}
	answers := p.QueryRelays([]string{"wss://a.example"}, Filter{Kinds: []int64{1}}, authKey, QueryOptions{Needed: 1, Paged: true, Keep: onlyValid})
	if len(answers) != 1 || len(answers[0].Events) != 1 || answers[0].Events[0].(*nostr.Event).ID != valid.ID {
		t.Fatalf("%+v", answers)
	}
	// until: the forged event's second, then the one before (nothing new), then the valid one's.
	var untils []any
	for _, q := range r.Queries() {
		until, _ := q.Get("until")
		untils = append(untils, until)
	}
	same := fmt.Sprint(untils) == fmt.Sprint([]any{nil, float64(200), float64(199), float64(100), float64(99)})
	if !same {
		t.Errorf("untils %v", untils)
	}
}

// L3 (node's onchain-security-review.test.ts): a complete read, a full page whose only new event
// has no usable date, asks the page again once (an event not seen before, dated or not), then stops
// incomplete.
func TestACompleteReadAsksAFullPageWithAnUndatedNewEventOnceThenStops(t *testing.T) {
	seen := map[string]bool{}
	kept := 0
	page := func(events [][2]any) []Entry {
		var out []Entry
		for _, e := range events {
			out = append(out, EntryOf(jsonv.O("id", e[0], "created_at", e[1]).Object(), keepAll, nil))
		}
		return out
	}
	var first [][2]any
	for i := 0; i < 100; i++ {
		first = append(first, [2]any{"a" + strconv.Itoa(i), float64(1000)})
	}
	keep := func(any) { kept++ }
	if got := AfterCompletePage(page(first), seen, keep, 1); got != (PageOutcome{Until: 1000}) {
		t.Errorf("first page: %+v", got)
	}
	// SPEC.md §13: a new event, undated (2^60 is no safe integer), lets the read go on from the
	// dated ones.
	second := append(append([][2]any(nil), first[:99]...), [2]any{"new", math.Pow(2, 60)})
	if got := AfterCompletePage(page(second), seen, keep, 2); got != (PageOutcome{Until: 1000}) {
		t.Errorf("second page: %+v", got)
	}
	// The same page again brings nothing new: incomplete, never a loop.
	if got := AfterCompletePage(page(second), seen, keep, 3); got != (PageOutcome{Done: true}) {
		t.Errorf("third page: %+v", got)
	}
	// Kept all the same, once.
	if kept != 101 {
		t.Errorf("kept %d", kept)
	}
}
