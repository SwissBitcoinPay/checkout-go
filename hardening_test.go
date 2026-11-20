package checkout_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/relay"
	"github.com/sattleme/checkout-go/internal/testkit"
)

// What the package refuses or bounds beyond the conformance vectors: notes other languages hold
// differently, a channel whose reading key is off the curve, the fan-out of payment checks. HTTP
// answers, the relays' handshakes and reads, and a custom rate source's failures are tested in
// internal/httpx, internal/relay and internal/rates; the witnesses of an invoice in
// internal/onchain.

func TestANoteHoldingALoneSurrogateIsRefused(t *testing.T) {
	m := newMerchant(t)
	// A JSON escape makes one ("\ud800"), which a Go string holds as its WTF-8 bytes.
	_, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "N-1", Sats: 1, Note: "Order \xed\xa0\x80"})
	wantCode(t, err, checkout.CodeInvalidOrder, "note")
}

func TestANoteIsCountedInCodePoints(t *testing.T) {
	m := newMerchant(t)
	emojis := strings.Repeat("\xf0\x9f\x98\x80", 100)
	invoice, err := clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "N-2", Sats: 1, Note: emojis})
	must(t, err)
	if invoice.Note != emojis {
		t.Errorf("note %q", invoice.Note)
	}
	// 400 bytes in UTF-8: more than breez.tips's 255, so not sent as the comment.
	if made := m.Invoices()[0]; made.HasComment {
		t.Errorf("comment %q", made.Comment)
	}
	_, err = clientOf(t, m).CreateInvoice(ctx, checkout.Order{Ref: "N-3", Sats: 1, Note: emojis + "\xf0\x9f\x98\x80"})
	wantCode(t, err, checkout.CodeInvalidOrder, "note")
}

func TestAChannelWhoseReadingKeyIsOffTheCurveIsIgnored(t *testing.T) {
	m := newMerchant(t)
	m.PublishChannel(m.ChannelValue(200, jsonv.Member{Name: "registryReader", Value: strings.Repeat("00", 32)}), 200)
	// The newest valid copy stands: the one of 100.
	found, err := clientOf(t, m).Merchant(ctx)
	must(t, err)
	if found.BusinessName != "Sunrise Bakery" {
		t.Errorf("%+v", found)
	}
	other := testkit.NewMerchant()
	other.Start(nil, nil)
	t.Cleanup(other.Stop)
	other.PublishChannel(other.ChannelValue(100, jsonv.Member{Name: "registryReader", Value: strings.Repeat("00", 32)}), 100)
	_, err = clientOf(t, other).Merchant(ctx)
	wantCode(t, err, checkout.CodeChannelNotFound)
}

// counting counts the payment checks of breez.tips running at once.
type counting struct {
	next    http.RoundTripper
	mu      sync.Mutex
	running int
	most    int
}

func (c *counting) RoundTrip(req *http.Request) (*http.Response, error) {
	verify := strings.Contains(req.URL.Path, "/verify/")
	if verify {
		c.mu.Lock()
		c.running++
		c.most = max(c.most, c.running)
		c.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		defer func() {
			c.mu.Lock()
			c.running--
			c.mu.Unlock()
		}()
	}
	return c.next.RoundTrip(req)
}

func TestTheEarlierInvoicesOfAnOrderAreCheckedFourAtATime(t *testing.T) {
	m := newMerchant(t)
	c := clientOf(t, m)
	for i := 0; i < 6; i++ {
		_, err := c.CreateInvoice(ctx, checkout.Order{Ref: "E-1", Sats: 1, DurationSeconds: 60})
		must(t, err)
		m.SetNow(m.Now() + 60 + 120)
	}
	transport := &counting{next: m.HTTP}
	_, err := clientOf(t, m, checkout.WithHTTPTransport(transport)).CreateInvoice(ctx, checkout.Order{Ref: "E-1", Sats: 1})
	must(t, err)
	if transport.most != 4 {
		t.Errorf("%d checks at once", transport.most)
	}
	if n := len(m.Invoices()); n != 7 {
		t.Errorf("%d invoices", n)
	}
}

// SPEC.md §13: each request to a relay, the wait for the connection included, is bounded by the
// relay timeout: a relay whose name never resolves fails in time.
func TestARelayWhoseNameNeverResolvesFailsWithinTheRelayTimeout(t *testing.T) {
	m := newMerchant(t)
	var asked atomic.Int32
	stalled := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		asked.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	rt := &http.Transport{DialContext: (&net.Dialer{Resolver: stalled}).DialContext}
	c, err := checkout.New(m.Key(), checkout.WithWebSocketDialer(checkout.NewWebSocketDialer(rt)), checkout.WithHTTPTransport(rt),
		checkout.WithRelayTimeout(300*time.Millisecond), checkout.WithLogger(quiet()))
	must(t, err)
	started := time.Now()
	_, err = c.CreateInvoice(ctx, checkout.Order{Ref: "D-1", Sats: 1})
	wantCode(t, err, checkout.CodeNetwork)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("took %v", elapsed)
	}
	if asked.Load() == 0 {
		t.Error("the names were not resolved")
	}
}

// SPEC.md §13, §15: every WebSocket handshake of a call, to each relay it reads or writes, carries
// the User-Agent of every request (relays behind a bot protection refuse a handshake without one)
// and offers no compression.
func TestEveryRelayHandshakeCarriesTheUserAgentAndOffersNoCompression(t *testing.T) {
	m := newMerchant(t)
	// Room for a busy machine: the declaration goes to every relay, and waits for its connection.
	c := clientOf(t, m, checkout.WithRelayTimeout(10*time.Second))
	_, err := c.CreateInvoice(ctx, checkout.Order{Ref: "UA-1", Sats: 2100})
	must(t, err)
	// The call may return before the last relay's handshake: its reads and its declaration go on
	// once a majority answered or took it, the other relays' requests after it.
	for _, url := range m.KeyRelays {
		testkit.Eventually(t, func() bool { return len(m.Net.Get(url).Handshakes()) > 0 }, url+": a handshake")
	}
	for _, url := range m.KeyRelays {
		for _, h := range m.Net.Get(url).Handshakes() {
			if h.Get("User-Agent") != "sattleme-Checkout/1" || h.Get("Sec-WebSocket-Extensions") != "" {
				t.Errorf("%s: handshake headers %v", url, h)
			}
		}
	}
}

// failingRandom gives crypto/rand's bytes to its first reads, then fails; it counts its reads.
type failingRandom struct {
	mu    sync.Mutex
	left  int
	reads int
}

func (f *failingRandom) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.left == 0 {
		return 0, errors.New("the random source failed")
	}
	f.left--
	return rand.Read(p)
}

// count is how many reads it had: a publication's relays past the majority may still read it, for
// their one authentication, once the call returned.
func (f *failingRandom) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// SPEC.md §5: every error a call fails with, but for a cancellation, is one of the package's.
// Wherever the client's random source fails (after each count of the reads a call makes), every
// method fails with an Error of the package (invalid_option, random, or network when a read of the
// relays lost its requests), never with what failed below as it is.
func TestEveryErrorOfACallIsAnErrorOfThePackage(t *testing.T) {
	s := newRestSetup(t)
	// Free addresses for new orders; the order R-1 underpaid: its check asks an invoice of the rest,
	// declares it and publishes its records.
	s.m.PublishLot(lot, 91)
	s.pay(20_000, "part")
	// An invoice with an address paid in Lightning: a wait ends at its first status, once its full
	// check (the on-chain part, read on the relays) ended.
	paid, err := s.c.CreateInvoice(ctx, checkout.Order{Ref: "E-PAID", Sats: 49_000})
	must(t, err)
	if paid.OnchainAddress == "" {
		t.Fatal("no address")
	}
	s.m.PayInvoice(paid.PaymentHash)
	orders := 0
	for _, c := range []struct {
		name string
		call func(c *checkout.Client) error
	}{
		{"Merchant", func(c *checkout.Client) error { _, err := c.Merchant(ctx); return err }},
		{"CreateInvoice", func(c *checkout.Client) error {
			orders++
			_, err := c.CreateInvoice(ctx, checkout.Order{Ref: "E-" + strconv.Itoa(orders), Sats: 49_000, Checkout: true})
			return err
		}},
		{"GetStatus", func(c *checkout.Client) error { _, err := c.GetStatus(ctx, s.invoice); return err }},
		{"WaitForPayment", func(c *checkout.Client) error { _, err := c.WaitForPayment(ctx, paid); return err }},
		{"FindInvoices", func(c *checkout.Client) error { _, err := c.FindInvoices(ctx, "R-1"); return err }},
	} {
		// The reads the call makes when its source does not fail, then a failure after each count.
		counted := &failingRandom{left: -1}
		must(t, c.call(s.client(t, checkout.WithRandom(counted))))
		reads := counted.count()
		if reads == 0 {
			t.Errorf("%s reads no random bytes", c.name)
		}
		for left := 0; left < reads; left++ {
			err := c.call(s.client(t, checkout.WithRandom(&failingRandom{left: left})))
			if err == nil {
				continue
			}
			var e *checkout.Error
			if !errors.As(err, &e) {
				t.Errorf("%s, the random source failing after %d reads: %T %v", c.name, left, err, err)
			}
		}
	}
}

// SPEC.md §13 (Limits): of one relay's answer to a query, the first event of each id is read; a
// later one of that id (a copy, or a forgery: an id is its event's hash) only counts toward the
// query's 1000 events, nothing of it kept, opened nor checked. Of an event kept, only its NIP-01
// members: a relay may add any to an event that checks out, its id and its signature covering none.
// A relay sends 50 copies of the channel, each with 100 000 characters added: each read (not paged,
// paged, complete) keeps the channel once, as its NIP-01 members, and opens one copy per query that
// sent them.
func TestARelaysCopiesOfOneEventAreReadOnceWithoutWhatItAdds(t *testing.T) {
	m := newMerchant(t)
	const url = "wss://copies.test"
	r := m.Net.Add(url, testkit.Behaviour{})
	channel := m.ChannelEvent(m.ChannelValue(100), 100, nil)
	padded := testkit.EventValue(channel).(*jsonv.Object)
	padded.Set("padding", strings.Repeat("x", 100_000))
	for i := 0; i < 50; i++ {
		r.Store(padded)
	}
	_, encryptionKey := derive.ChannelKeys(m.IntegrationKey)
	open := nostr.RecordsThatOpen(func(string, string) []byte { return encryptionKey }, nostr.NewSigMemo())
	var opened atomic.Int64
	keep := func(v any) (any, bool) {
		opened.Add(1)
		return open(v)
	}
	filter := relay.Filter{Kinds: []int64{proto.RecordKind}}
	authKey := make([]byte, 32)
	for _, c := range []struct {
		name string
		read func(p *relay.Pool) []relay.Answer
	}{
		{"not paged", func(p *relay.Pool) []relay.Answer {
			return p.QueryRelays([]string{url}, filter, authKey, relay.QueryOptions{Needed: 1, Keep: keep})
		}},
		{"paged", func(p *relay.Pool) []relay.Answer {
			return p.QueryRelays([]string{url}, filter, authKey, relay.QueryOptions{Needed: 1, Paged: true, Keep: keep})
		}},
		{"complete", func(p *relay.Pool) []relay.Answer {
			return p.QueryComplete([]string{url}, filter, authKey, 1, keep, false).Answers
		}},
	} {
		opened.Store(0)
		asked := len(r.Queries())
		pool := relay.NewPool(ctx, relay.Options{Dialer: m.Net.Dialer(), Timeout: 10 * time.Second, Now: m.Now, Random: rand.Reader})
		answers := c.read(pool)
		pool.Close()
		if len(answers) != 1 {
			t.Fatalf("%s: %d answers", c.name, len(answers))
		}
		same(t, c.name, answers[0].Events, []any{testkit.EventValue(channel)})
		// One opening per query that sent the copies (a paged read asks their second again).
		sent := 0
		for _, f := range r.Queries()[asked:] {
			until, bounded := f.Get("until")
			if n, _ := until.(float64); !bounded || n >= float64(channel.CreatedAt) {
				sent++
			}
		}
		if n := opened.Load(); n != int64(sent) {
			t.Errorf("%s: %d copies opened by %d queries that sent them", c.name, n, sent)
		}
	}
}
