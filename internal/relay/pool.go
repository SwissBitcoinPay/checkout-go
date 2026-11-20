// Package relay is the Nostr relay client (NIP-01, NIP-42; SPEC.md §13), as the merchant's app
// reads and writes relays. An integration's server lives for one call: a Pool opens one
// connection per relay for that call, every read and write of the call goes over it, and it is
// closed once the call is done. Every relay of a list at once; a read goes on as soon as a
// majority of them answered (any two majorities of one list share a relay). Subscription ids are
// random: what a relay receives names nothing of the integration.
package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/secp"
	"github.com/sattleme/checkout-go/internal/wsdial"
)

// Conn is a WebSocket connection to a relay.
type Conn = wsdial.Conn

// Dialer opens WebSocket connections to relays, the handshake carrying header and bounded by ctx.
type Dialer interface {
	Dial(ctx context.Context, url string, header http.Header) (Conn, error)
}

// Filter is a NIP-01 subscription filter, as far as the package uses it.
type Filter struct {
	Kinds   []int64
	Authors []string
	// D is "#d"; O is "#o", a witness's order tag (SPEC.md §31).
	D     []string
	O     []string
	Since *int64
	Until *int64
	Limit *int64
}

// JSON writes the filter as a relay takes it.
func (f Filter) JSON() jsonv.Members {
	var m jsonv.Members
	if f.Kinds != nil {
		kinds := make([]any, len(f.Kinds))
		for i, k := range f.Kinds {
			kinds[i] = k
		}
		m = append(m, jsonv.Member{Name: "kinds", Value: kinds})
	}
	if f.Authors != nil {
		m = append(m, jsonv.Member{Name: "authors", Value: f.Authors})
	}
	if f.D != nil {
		m = append(m, jsonv.Member{Name: "#d", Value: f.D})
	}
	if f.O != nil {
		m = append(m, jsonv.Member{Name: "#o", Value: f.O})
	}
	if f.Since != nil {
		m = append(m, jsonv.Member{Name: "since", Value: *f.Since})
	}
	if f.Until != nil {
		m = append(m, jsonv.Member{Name: "until", Value: *f.Until})
	}
	if f.Limit != nil {
		m = append(m, jsonv.Member{Name: "limit", Value: *f.Limit})
	}
	if m == nil {
		m = jsonv.Members{}
	}
	return m
}

// Keep is what a read keeps of an event a relay sent (SPEC.md §13), as it comes: what it opens
// to, or the event itself, when it opens; false drops it. Its id and its date are kept anyway, to
// page the read and count each event once; so what a relay sends never piles up. It runs on the
// connection's reader: it must not block, and may be called for several relays at once.
type Keep func(v any) (any, bool)

// Entry is what a read keeps of an event, as it comes: its id and its date, and what Keep gave.
type Entry struct {
	ID        string
	HasID     bool
	CreatedAt int64
	HasDate   bool
	Value     any
	Kept      bool
}

// EntryOf is the entry of v: its id when it is a string (a long one by its SHA-256: an id is 64
// characters), its created_at when it is a safe integer, and what keep keeps of it, if any. read,
// when not nil, holds the ids of the query so far: an event of one of them again (a copy, or a
// forgery: an id is its event's hash) only counts, never kept nor checked again (SPEC.md §13,
// "Limits"); the id of an event read is added to it.
func EntryOf(v any, keep Keep, read map[string]bool) Entry {
	var e Entry
	if o, ok := jsonv.AsObject(v); ok {
		if id, ok := o.Get("id"); ok {
			if s, ok := id.(string); ok {
				e.HasID = true
				if len(s) <= 64 {
					e.ID = s
				} else {
					sum := sha256.Sum256([]byte(s))
					e.ID = "#" + hex.EncodeToString(sum[:])
				}
			}
		}
		if createdAt, ok := o.Get("created_at"); ok {
			if n, ok := jsonv.AsSafeInteger(createdAt); ok {
				e.CreatedAt, e.HasDate = n, true
			}
		}
	}
	if e.HasID && read != nil {
		if read[e.ID] {
			return e
		}
		read[e.ID] = true
	}
	if keep != nil {
		if kept, ok := keep(v); ok {
			e.Value, e.Kept = kept, true
		}
	}
	return e
}

// Majority is more than half of n relays: any two majorities of one list share a relay.
func Majority(n int) int { return n/2 + 1 }

// Answer is the events a relay answered, as Keep kept them. Partial is a relay still reading when
// a read on every relay went on: the pages it sent are kept as events, and that is all, for it
// did not answer (SPEC.md §13).
type Answer struct {
	Relay   string
	Events  []any
	Partial bool
}

// Answered is how many relays of answers answered: those whose read ended, complete or not.
func Answered(answers []Answer) int {
	n := 0
	for _, a := range answers {
		if !a.Partial {
			n++
		}
	}
	return n
}

// MajorityAnswered is whether a majority of n relays answered, of answers (SPEC.md §13): their
// read ended, complete or not; a relay still reading never did, whatever it sent.
func MajorityAnswered(answers []Answer, n int) bool { return Answered(answers) >= Majority(n) }

// Options is what a pool is made with.
type Options struct {
	Dialer Dialer
	// Timeout bounds each request (a page of a read, a write), the wait for the connection
	// included, and each connection's handshake.
	Timeout time.Duration
	// Now is the clock, in Unix seconds: the date of NIP-42 authentication events.
	Now func() int64
	// Random is the CSPRNG: subscription ids, signatures' auxiliary randomness.
	Random io.Reader
	// Header is what each handshake carries: the User-Agent of SPEC.md §15.
	Header http.Header
}

// Pool is the relays of one call: one connection per relay, opened at its first request, kept for
// the call's next requests, closed by Close or Release. Requests are bounded by Timeout each.
type Pool struct {
	opts Options
	// ctx is the call's: a cancelled call stops every request.
	ctx context.Context
	// life ends the connections: Close, or the call's cancellation.
	life     context.Context
	stopLife context.CancelFunc

	mu    sync.Mutex
	conns map[string]*conn
	// failed holds the relays a request to which ran out of time or lost its connection in this
	// call: a read on every relay counts them as failed at once (SPEC.md §13).
	failed map[string]bool
	// closed: nothing new may be asked (Release, Close); shut: the connections are closed (Close).
	closed  bool
	shut    bool
	pending sync.WaitGroup
}

// NewPool returns the pool of the call ctx.
func NewPool(ctx context.Context, opts Options) *Pool {
	life, stop := context.WithCancel(ctx)
	return &Pool{opts: opts, ctx: ctx, life: life, stopLife: stop, conns: map[string]*conn{}, failed: map[string]bool{}}
}

// Failed tells whether a request to relay ran out of time or lost its connection in this call.
func (p *Pool) Failed(relay string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failed[relay]
}

func (p *Pool) markFailed(relay string) {
	p.mu.Lock()
	p.failed[relay] = true
	p.mu.Unlock()
}

// Close closes every connection now: their requests fail.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed, p.shut = true, true
	conns := p.conns
	p.conns = map[string]*conn{}
	p.mu.Unlock()
	for _, c := range conns {
		c.die(errClosed)
	}
	p.stopLife()
}

// Release closes every connection once the requests still running are over (a write to the
// relays past the quorum), each bounded by its timeout. Nothing new may be asked of the pool.
func (p *Pool) Release() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	go func() {
		p.pending.Wait()
		// What each connection still has to send first (the CLOSE of a subscription a read went
		// on without), within the timeout.
		p.mu.Lock()
		conns := make([]*conn, 0, len(p.conns))
		for _, c := range p.conns {
			conns = append(conns, c)
		}
		p.mu.Unlock()
		deadline := time.NewTimer(p.opts.Timeout)
		defer deadline.Stop()
		for _, c := range conns {
			select {
			case <-c.drain():
			case <-deadline.C:
			}
		}
		p.Close()
	}()
}

var (
	errClosed  = errors.New("connection closed")
	errTimeout = errors.New("no answer in time")
)

// RelayError is a request to a relay that failed.
type RelayError struct {
	Relay  string
	Reason string
	// Refused is a relay's refusal of an event (NIP-01 OK false) or of a query (CLOSED).
	Refused bool
}

func (e *RelayError) Error() string { return e.Relay + ": " + e.Reason }

// connection is the pool's connection to url, opened at its first request; false once the pool
// is shut.
func (p *Pool) connection(url string) (*conn, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shut {
		return nil, false
	}
	c, ok := p.conns[url]
	if !ok {
		c = newConn(url)
		p.conns[url] = c
		go c.run(p)
	}
	return c, true
}

// request runs op over the connection to url once it is open, within the timeout. It fails when
// the connection ends, the time runs out, or parent is done; a relay whose request ran out of time
// or lost its connection is failed for the call. Once parent is done (the read went on without the
// relay, or the call was cancelled), it starts nothing: no connection, no query.
func (p *Pool) request(parent context.Context, url string, op func(ctx context.Context, c *conn) error) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if !p.begin() {
		p.markFailed(url)
		return errPoolClosed(url)
	}
	return p.begun(parent, url, op)
}

func errPoolClosed(url string) error { return &RelayError{Relay: url, Reason: "the pool is closed"} }

// begin counts a request that Release waits for before it closes the connections: false once
// nothing new may be asked of the pool.
func (p *Pool) begin() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.pending.Add(1)
	return true
}

// mayDial tells whether c may be dialed now: while the pool takes new requests; once it was
// released (its call returned), only for a publication's relay past the majority, whose event waits
// in c's queue (nothing else is queued before a connection opens); never once the pool is closed
// or its call cancelled.
func (p *Pool) mayDial(c *conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shut || p.ctx.Err() != nil {
		return false
	}
	if !p.closed {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue) > 0
}

// begun is request once begin counted it, even if Release came meanwhile (a publication's tail
// counted before its call returned).
func (p *Pool) begun(parent context.Context, url string, op func(ctx context.Context, c *conn) error) error {
	defer p.pending.Done()
	deadline := time.Now().Add(p.opts.Timeout)
	c, ok := p.connection(url)
	if !ok {
		p.markFailed(url)
		return errPoolClosed(url)
	}
	return p.over(parent, url, c, deadline, op)
}

// over runs op over c, once it is open, until deadline: nil, the relay's failure for the call
// (its time ran out, its connection failed), or the reason it was dropped.
func (p *Pool) over(parent context.Context, url string, c *conn, deadline time.Time, op func(ctx context.Context, c *conn) error) error {
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	err := func() error {
		select {
		case <-c.opened:
		case <-c.dead:
			return errClosed
		case <-ctx.Done():
			return ctx.Err()
		}
		return op(ctx, c)
	}()
	switch {
	case err == nil:
		return nil
	case p.ctx.Err() != nil:
		// The call was cancelled: no relay's failure.
		return p.ctx.Err()
	case isTimeout(err) && !time.Now().Before(deadline):
		// Its own time ran out, whatever ended meanwhile (a read that went on without it).
		p.markFailed(url)
		return &RelayError{Relay: url, Reason: errTimeout.Error()}
	case parent.Err() != nil:
		// The read went on without it, in time: dropped, not failed.
		return parent.Err()
	case errors.Is(err, errClosed), c.isDead():
		p.markFailed(url)
		return &RelayError{Relay: url, Reason: "connection failed"}
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, errTimeout):
		p.markFailed(url)
		return &RelayError{Relay: url, Reason: errTimeout.Error()}
	}
	return err
}

func isTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, errTimeout)
}

// wait waits for done, the connection's end or ctx.
func wait(ctx context.Context, c *conn, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-c.dead:
		return errClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AuthRequired is the NIP-42 prefix of a refusal that authentication may lift.
const AuthRequired = "auth-required:"

type queryResult struct {
	entries []Entry
	closed  bool
	reason  string
}

// query is the stored events of one relay that match filter, as it sent them: the entry of each
// (EntryOf: the event itself only when keep takes it), unchecked, the caller verifies them. At
// most 1000. Authenticates when the relay asks (NIP-42).
func (p *Pool) query(parent context.Context, url string, filter Filter, authKey []byte, keep Keep) ([]Entry, error) {
	var entries []Entry
	err := p.request(parent, url, func(ctx context.Context, c *conn) error {
		result, err := p.ask(ctx, c, filter, keep)
		if err != nil {
			return err
		}
		if result.closed && strings.HasPrefix(result.reason, AuthRequired) {
			authenticated, err := c.authenticate(ctx, p, authKey)
			if err != nil {
				return err
			}
			if authenticated {
				if result, err = p.ask(ctx, c, filter, keep); err != nil {
					return err
				}
			}
		}
		if result.closed {
			return &RelayError{Relay: url, Reason: "closed the query: " + result.reason, Refused: true}
		}
		entries = result.entries
		return nil
	})
	return entries, err
}

// subscription is a query's state, written by the connection's reader.
type subscription struct {
	mu     sync.Mutex
	open   bool
	result queryResult
	done   chan struct{}
}

func (s *subscription) end(closed bool, reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open {
		return false
	}
	s.open = false
	s.result.closed, s.result.reason = closed, reason
	close(s.done)
	return true
}

func (p *Pool) subscriptionID() (string, error) {
	b := make([]byte, 8)
	if _, err := io.ReadFull(p.opts.Random, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ask sends one REQ and reads its answer: the events until EOSE (then CLOSE), the 1000th ending
// it (then CLOSE), or CLOSED. Once ctx is done (its read went on, after an authentication too),
// it draws no subscription id and sends nothing.
func (p *Pool) ask(ctx context.Context, c *conn, filter Filter, keep Keep) (queryResult, error) {
	if err := ctx.Err(); err != nil {
		return queryResult{}, err
	}
	id, err := p.subscriptionID()
	if err != nil {
		return queryResult{}, err
	}
	s := &subscription{open: true, done: make(chan struct{})}
	// The ids of this query's events so far: the first event of each is the one read. Heard on
	// the connection's reader alone.
	read := map[string]bool{}
	stop := c.listen(func(message []any) {
		if len(message) < 2 || message[1] != id {
			return
		}
		switch message[0] {
		case "EVENT":
			var event any
			if len(message) > 2 {
				event = message[2]
			}
			entry := EntryOf(event, keep, read)
			s.mu.Lock()
			if !s.open {
				s.mu.Unlock()
				return
			}
			s.result.entries = append(s.result.entries, entry)
			full := len(s.result.entries) >= proto.MaxEventsPerQuery
			s.mu.Unlock()
			if full && s.end(false, "") {
				// Over this connection, which stays open for the call's next requests.
				c.send([]any{"CLOSE", id})
			}
		case "EOSE":
			if s.end(false, "") {
				c.send([]any{"CLOSE", id})
			}
		case "CLOSED":
			reason := ""
			if len(message) > 2 {
				reason, _ = message[2].(string)
			}
			s.end(true, reason)
		}
	})
	defer stop()
	c.send([]any{"REQ", id, filter.JSON()})
	if err := wait(ctx, c, s.done); err != nil {
		if s.end(false, "") {
			c.send([]any{"CLOSE", id})
		}
		return queryResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, nil
}

// writing is an event sent to a relay, its OK awaited.
type writing struct {
	id       string
	done     chan struct{}
	once     sync.Once
	accepted bool
	reason   string
	stop     func()
}

// startWrite has the relay's OK of event heard, then queues the event: written in the order they
// are queued, once the connection is open.
func (c *conn) startWrite(event *nostr.Event) *writing {
	w := &writing{id: event.ID, done: make(chan struct{})}
	w.stop = c.listen(func(message []any) {
		if len(message) < 2 || message[0] != "OK" || message[1] != w.id {
			return
		}
		w.once.Do(func() {
			w.accepted = len(message) > 2 && message[2] == true
			if len(message) > 3 {
				w.reason, _ = message[3].(string)
			}
			close(w.done)
		})
	})
	c.send([]any{"EVENT", event.JSON()})
	return w
}

// result waits for the relay's OK: whether it took the event, and the reason it gave.
func (w *writing) result(ctx context.Context, c *conn) (bool, string, error) {
	defer w.stop()
	if err := wait(ctx, c, w.done); err != nil {
		return false, "", err
	}
	return w.accepted, w.reason, nil
}

// write sends event to a relay again (once authenticated), and waits for its OK; nothing once ctx
// is done (its call cancelled, its time run out).
func (p *Pool) write(ctx context.Context, c *conn, event *nostr.Event) (bool, string, error) {
	if err := ctx.Err(); err != nil {
		return false, "", err
	}
	return c.startWrite(event).result(ctx, c)
}

// QueryOptions is how QueryRelays reads.
type QueryOptions struct {
	// Needed is the number of relays whose answer is enough (a majority).
	Needed int
	// Paged reads page after page (SPEC.md §13, "Paged reads").
	Paged bool
	Keep  Keep
	// EveryRelay waits for every relay (SPEC.md §13, "Reads on every relay").
	EveryRelay bool
}

// sink holds the events a read kept so far: on every relay, a read cut short keeps its pages.
type sink struct {
	mu     sync.Mutex
	events []any
}

func (s *sink) add(v any) {
	s.mu.Lock()
	s.events = append(s.events, v)
	s.mu.Unlock()
}

func (s *sink) snapshot() []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]any(nil), s.events...)
}

type readResult struct {
	relay    string
	events   []any
	complete bool
	err      error
}

// QueryRelays is what every relay of relays holds that matches filter, in parallel, page after
// page when Paged. It goes on as soon as Needed relays answered, or once every relay answered or
// failed: the events of each relay that answered, in the order they answered, unchecked. The
// others' reads are dropped then, and it returns once they stopped: they start nothing more (no
// page, no subscription, no authentication), their context done, so each stops at once. On every
// relay (EveryRelay): the relays failed earlier in the call count as failed at once, the others
// are waited for one timeout more at most once Needed answered, and the pages of a relay still
// reading are kept, Partial (Answered leaves it out). It never fails; the call's cancellation
// ends it at once (the caller checks it).
func (p *Pool) QueryRelays(relays []string, filter Filter, authKey []byte, o QueryOptions) []Answer {
	ctx, stop := context.WithCancel(p.ctx)
	defer stop()
	asked := p.asked(relays, o.EveryRelay)
	failed := len(relays) - len(asked)
	sinks := map[string]*sink{}
	results := make(chan readResult, len(asked))
	var readers sync.WaitGroup
	for _, relay := range asked {
		s := &sink{}
		sinks[relay] = s
		readers.Add(1)
		go func(relay string) {
			defer readers.Done()
			var events []any
			var err error
			if o.Paged {
				events, err = p.pages(ctx, relay, filter, authKey, o.Keep, s)
			} else {
				var entries []Entry
				entries, err = p.query(ctx, relay, filter, authKey, o.Keep)
				events = keptOf(entries)
			}
			results <- readResult{relay: relay, events: events, err: err}
		}(relay)
	}
	var read CompleteAnswers
	ended := map[string]bool{}
	var grace *time.Timer
	defer func() {
		if grace != nil {
			grace.Stop()
		}
	}()
	graceC := func() <-chan time.Time {
		if grace == nil {
			return nil
		}
		return grace.C
	}
read:
	for {
		if len(read.Answers)+failed >= len(relays) {
			break
		}
		if len(read.Answers) >= o.Needed {
			if !o.EveryRelay {
				break
			}
			// On every relay, the others are waited for one timeout more at most (SPEC.md §13).
			if grace == nil {
				grace = time.NewTimer(p.opts.Timeout)
			}
		}
		select {
		case r := <-results:
			ended[r.relay] = true
			if r.err != nil {
				failed++
			} else {
				read.ended(r.relay, r.events, false)
			}
		case <-graceC():
			break read
		case <-p.ctx.Done():
			break read
		}
	}
	stop()
	// Nothing the read started runs on once it returned: its call may return next.
	readers.Wait()
	if o.EveryRelay {
		for _, relay := range asked {
			if !ended[relay] {
				read.stillReading(relay, sinks[relay].snapshot())
			}
		}
	}
	return read.Answers
}

// asked is the relays a read asks: on every relay, those that failed in this call are not.
func (p *Pool) asked(relays []string, everyRelay bool) []string {
	if !everyRelay {
		return relays
	}
	var asked []string
	for _, relay := range relays {
		if !p.Failed(relay) {
			asked = append(asked, relay)
		}
	}
	return asked
}

func keptOf(entries []Entry) []any {
	var kept []any
	for _, e := range entries {
		if e.Kept {
			kept = append(kept, e.Value)
		}
	}
	return kept
}

// CompleteAnswers is what a complete read of a list gives: each answering relay's events, and
// how many read all.
type CompleteAnswers struct {
	// Answers are the events of every relay whose read ended (its first page answered), complete
	// or not, and on every relay those a relay still reading had sent (Partial: it did not
	// answer).
	Answers []Answer
	// Complete is how many relays read completely.
	Complete int
}

// ended records a relay whose read ended: its events, as it answered, and whether it read
// completely. A relay that failed is not recorded: it did not answer.
func (a *CompleteAnswers) ended(relay string, events []any, complete bool) {
	a.Answers = append(a.Answers, Answer{Relay: relay, Events: events})
	if complete {
		a.Complete++
	}
}

// stillReading records what a relay still reading had sent when a read on every relay went on:
// its events are kept, but it did not answer.
func (a *CompleteAnswers) stillReading(relay string, events []any) {
	if len(events) > 0 {
		a.Answers = append(a.Answers, Answer{Relay: relay, Events: events, Partial: true})
	}
}

// MajorityAnswered is whether a majority of n relays answered (SPEC.md §13): their read ended,
// complete or not; a relay still reading never did.
func (a CompleteAnswers) MajorityAnswered(n int) bool { return MajorityAnswered(a.Answers, n) }

// MajorityComplete is whether a majority of n relays read completely: the read is complete.
func (a CompleteAnswers) MajorityComplete(n int) bool { return a.Complete >= Majority(n) }

// QueryComplete is a complete read of filter on every relay of relays at once (SPEC.md §13,
// "Complete reads"): pages of 100 events, the next one from the oldest second of the last, until
// a page is not full. It goes on as soon as needed relays read completely, or once every relay
// answered, stopped incomplete or failed; on every relay as QueryRelays does, and also as soon as
// needed relays can no longer read completely. The others' reads are dropped then, and it returns
// once they stopped, as QueryRelays does.
func (p *Pool) QueryComplete(relays []string, filter Filter, authKey []byte, needed int, keep Keep, everyRelay bool) CompleteAnswers {
	ctx, stop := context.WithCancel(p.ctx)
	defer stop()
	asked := p.asked(relays, everyRelay)
	settled := len(relays) - len(asked)
	sinks := map[string]*sink{}
	results := make(chan readResult, len(asked))
	var readers sync.WaitGroup
	for _, relay := range asked {
		s := &sink{}
		sinks[relay] = s
		readers.Add(1)
		go func(relay string) {
			defer readers.Done()
			events, complete, err := p.completePages(ctx, relay, filter, authKey, keep, s)
			results <- readResult{relay: relay, events: events, complete: complete, err: err}
		}(relay)
	}
	var out CompleteAnswers
	ended := map[string]bool{}
	var grace *time.Timer
	defer func() {
		if grace != nil {
			grace.Stop()
		}
	}()
	graceC := func() <-chan time.Time {
		if grace == nil {
			return nil
		}
		return grace.C
	}
read:
	for {
		if settled >= len(relays) {
			break
		}
		// On every relay, no wait once a majority can no longer read completely: not complete.
		if everyRelay && out.Complete+(len(relays)-settled) < needed {
			break
		}
		if out.Complete >= needed {
			if !everyRelay {
				break
			}
			if grace == nil {
				grace = time.NewTimer(p.opts.Timeout)
			}
		}
		select {
		case r := <-results:
			ended[r.relay] = true
			settled++
			if r.err == nil {
				out.ended(r.relay, r.events, r.complete)
			}
		case <-graceC():
			break read
		case <-p.ctx.Done():
			break read
		}
	}
	stop()
	readers.Wait()
	if everyRelay {
		for _, relay := range asked {
			if !ended[relay] {
				out.stillReading(relay, sinks[relay].snapshot())
			}
		}
	}
	return out
}

// PublishToQuorum publishes event to every relay of relays, in parallel: nil once quorum relays
// took it (the others go on until Release, each bounded by its timeout), an error as soon as they
// no longer can; the call's error when it is cancelled: it then sends the event to no more relays.
func (p *Pool) PublishToQuorum(relays []string, event *nostr.Event, authKey []byte, quorum int) error {
	results := make(chan error, len(relays))
	// Its requests end at once when it fails: only the relays past a majority may go on (SPEC.md
	// §13). Once it succeeded, ctx ends when the last of them did.
	ctx, cancel := context.WithCancel(p.ctx)
	for _, relay := range relays {
		if err := p.ctx.Err(); err != nil {
			results <- err
			continue
		}
		// Counted now, before the quorum may let the call return: Release waits for the relays
		// past it, whenever their goroutine runs. Queued now too: each relay gets the call's
		// events in the order the call publishes them (authentication asked for aside).
		if !p.begin() {
			p.markFailed(relay)
			results <- errPoolClosed(relay)
			continue
		}
		deadline := time.Now().Add(p.opts.Timeout)
		c, ok := p.connection(relay)
		if !ok {
			p.pending.Done()
			p.markFailed(relay)
			results <- errPoolClosed(relay)
			continue
		}
		w := c.startWrite(event)
		go func(relay string) {
			defer p.pending.Done()
			defer w.stop()
			results <- p.over(ctx, relay, c, deadline, func(ctx context.Context, c *conn) error {
				accepted, reason, err := w.result(ctx, c)
				if err != nil {
					return err
				}
				if !accepted && strings.HasPrefix(reason, AuthRequired) {
					authenticated, err := c.authenticate(ctx, p, authKey)
					if err != nil {
						return err
					}
					if authenticated {
						if accepted, reason, err = p.write(ctx, c, event); err != nil {
							return err
						}
					}
				}
				if !accepted {
					return &RelayError{Relay: relay, Reason: "refused the event: " + reason, Refused: true}
				}
				return nil
			})
		}(relay)
	}
	taken, failures := 0, 0
	for {
		if taken >= quorum {
			go func(left int) {
				for ; left > 0; left-- {
					<-results
				}
				cancel()
			}(len(relays) - taken - failures)
			return nil
		}
		if len(relays)-failures < quorum {
			cancel()
			// A request the call's cancellation ended is no relay's failure.
			if err := p.ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("fewer than %d relays took the event", quorum)
		}
		select {
		case err := <-results:
			if err == nil {
				taken++
			} else {
				failures++
			}
		case <-p.ctx.Done():
			cancel()
			return p.ctx.Err()
		}
	}
}

// pages reads the stored events of url that match filter, page after page (SPEC.md §13, "Paged
// reads"), as the merchant's app reads them: the events kept go to s as pages come.
func (p *Pool) pages(ctx context.Context, url string, filter Filter, authKey []byte, keep Keep, s *sink) ([]any, error) {
	seen := map[string]bool{}
	until := filter.Until
	for page := 0; page < proto.MaxPages; page++ {
		f := filter
		f.Until = until
		entries, err := p.query(ctx, url, f, authKey, keep)
		if err != nil {
			// A relay that stops answering after its first page keeps what it sent.
			if page == 0 || ctx.Err() != nil {
				return nil, err
			}
			break
		}
		next, more := AfterPagedPage(entries, seen, s.add)
		if !more {
			break
		}
		until = &next
	}
	return s.snapshot(), nil
}

// AfterPagedPage reads one page of a paged read (SPEC.md §13, "Paged reads"): the until of the
// next page, or false when the read ends there. Only an event's string id and safe integer date
// page the read, whatever else it holds; one without them is not kept. seen holds the ids of the
// read so far; the page's new ones are added to it, and the events kept of them go to kept.
func AfterPagedPage(answer []Entry, seen map[string]bool, kept func(any)) (int64, bool) {
	var oldest int64
	hasOldest, fresh := false, false
	for _, e := range answer {
		if !e.HasID || !e.HasDate {
			continue
		}
		if !hasOldest || e.CreatedAt < oldest {
			oldest, hasOldest = e.CreatedAt, true
		}
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		if e.Kept {
			kept(e.Value)
		}
		fresh = true
	}
	if !hasOldest {
		return 0, false
	}
	if !fresh {
		// Nothing new: the oldest second again would bring the same page.
		return oldest - 1, true
	}
	return oldest, true
}

// PageOutcome is what a complete read asks next after a page (Until), or how it ends (Done and
// Complete).
type PageOutcome struct {
	Until    int64
	Done     bool
	Complete bool
}

// AfterCompletePage reads one page of a complete read (SPEC.md §13, "Complete reads"): seen holds
// the ids of the read so far; the page's new ones are added to it, and the events kept of them
// to kept.
func AfterCompletePage(answer []Entry, seen map[string]bool, kept func(any), pagesRead int) PageOutcome {
	var oldest int64
	hasOldest, fresh := false, false
	for _, e := range answer {
		if !e.HasID {
			continue
		}
		if e.HasDate && (!hasOldest || e.CreatedAt < oldest) {
			oldest, hasOldest = e.CreatedAt, true
		}
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		if e.Kept {
			kept(e.Value)
		}
		// New, dated or not (SPEC.md §13: "no event not seen before"): the next page may hold
		// more.
		fresh = true
	}
	switch {
	case len(answer) > proto.CompleteReadLimit:
		// More than asked: the relay ignored the limit, and what it left out cannot be told.
		return PageOutcome{Done: true}
	case len(answer) < proto.CompleteReadLimit:
		return PageOutcome{Done: true, Complete: true}
	case !hasOldest || !fresh || pagesRead >= proto.MaxPages:
		// Full: the next page from its oldest second, unless it cannot move on.
		return PageOutcome{Done: true}
	}
	return PageOutcome{Until: oldest}
}

// completePages reads the events of url that match filter completely if it can, and whether it
// did. An error when its first page fails (the relay failed); a later failure ends the read
// incomplete.
func (p *Pool) completePages(ctx context.Context, url string, filter Filter, authKey []byte, keep Keep, s *sink) ([]any, bool, error) {
	seen := map[string]bool{}
	until := filter.Until
	limit := int64(proto.CompleteReadLimit)
	for page := 1; ; page++ {
		f := filter
		f.Limit = &limit
		f.Until = until
		entries, err := p.query(ctx, url, f, authKey, keep)
		if err != nil {
			if page == 1 || ctx.Err() != nil {
				return nil, false, err
			}
			return s.snapshot(), false, nil
		}
		next := AfterCompletePage(entries, seen, s.add, page)
		if next.Done {
			return s.snapshot(), next.Complete, nil
		}
		u := next.Until
		until = &u
	}
}

// conn is one connection to one relay, shared by every request of a pool.
type conn struct {
	url    string
	opened chan struct{}
	dead   chan struct{}

	mu           sync.Mutex
	err          error
	ws           Conn
	listeners    map[int]func([]any)
	nextListener int
	challenge    string
	hasChallenge bool
	// auths are the NIP-42 answers, by public key and challenge: one AUTH per key per challenge.
	auths map[string]*auth
	// queue is what the writer has still to send; a nil entry marks a drain, whose channel the
	// writer closes once it reaches it (drains, in their order).
	queue  [][]byte
	drains []chan struct{}
	wake   chan struct{}
}

type auth struct {
	done     chan struct{}
	accepted bool
}

func newConn(url string) *conn {
	return &conn{
		url:       url,
		opened:    make(chan struct{}),
		dead:      make(chan struct{}),
		listeners: map[int]func([]any){},
		auths:     map[string]*auth{},
		wake:      make(chan struct{}, 1),
	}
}

func (c *conn) isDead() bool {
	select {
	case <-c.dead:
		return true
	default:
		return false
	}
}

// run opens the connection, then reads it until it ends. A connection whose goroutine runs once
// its call returned is opened only for a publication's relay past the majority (mayDial): nothing
// else is asked of the dialer then.
func (c *conn) run(p *Pool) {
	if !p.mayDial(c) {
		c.die(errClosed)
		return
	}
	ctx, cancel := context.WithTimeout(p.life, p.opts.Timeout)
	ws, err := p.opts.Dialer.Dial(ctx, c.url, p.opts.Header.Clone())
	cancel()
	if err != nil {
		c.die(err)
		return
	}
	c.mu.Lock()
	if c.isDead() {
		c.mu.Unlock()
		ws.Close()
		return
	}
	c.ws = ws
	c.mu.Unlock()
	close(c.opened)
	go c.writeLoop(p.life, ws)
	for {
		data, err := ws.Read(p.life)
		if err != nil {
			c.die(err)
			return
		}
		value, err := jsonv.Parse(data)
		if err != nil {
			continue
		}
		message, ok := value.([]any)
		if !ok {
			continue
		}
		c.mu.Lock()
		if len(message) >= 2 && message[0] == "AUTH" {
			if challenge, ok := message[1].(string); ok {
				c.challenge, c.hasChallenge = challenge, true
			}
		}
		listeners := make([]func([]any), 0, len(c.listeners))
		for _, l := range c.listeners {
			listeners = append(listeners, l)
		}
		c.mu.Unlock()
		for _, l := range listeners {
			l(message)
		}
	}
}

func (c *conn) writeLoop(ctx context.Context, ws Conn) {
	for {
		select {
		case <-c.dead:
			return
		case <-c.wake:
		}
		for {
			c.mu.Lock()
			if len(c.queue) == 0 {
				c.mu.Unlock()
				break
			}
			data := c.queue[0]
			c.queue = c.queue[1:]
			if data == nil {
				// A drain: everything queued before it is written.
				if len(c.drains) > 0 {
					close(c.drains[0])
					c.drains = c.drains[1:]
				}
				c.mu.Unlock()
				continue
			}
			c.mu.Unlock()
			if err := ws.Write(ctx, data); err != nil {
				c.die(err)
				return
			}
		}
	}
}

// listen hears every message of the relay until the returned function is called.
func (c *conn) listen(l func([]any)) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextListener
	c.nextListener++
	c.listeners[id] = l
	return func() {
		c.mu.Lock()
		delete(c.listeners, id)
		c.mu.Unlock()
	}
}

// send queues a message; a connection that ended drops it.
func (c *conn) send(message []any) {
	data := jsonv.Marshal(message)
	c.mu.Lock()
	if c.isDead() {
		c.mu.Unlock()
		return
	}
	c.queue = append(c.queue, data)
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// drain is closed once the writer wrote every message queued so far, or the connection is not
// open (never opened, or ended).
func (c *conn) drain() <-chan struct{} {
	done := make(chan struct{})
	c.mu.Lock()
	if c.isDead() || c.ws == nil {
		c.mu.Unlock()
		close(done)
		return done
	}
	c.queue = append(c.queue, nil)
	c.drains = append(c.drains, done)
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return done
}

// die ends the connection; every request on it fails, every drain is over.
func (c *conn) die(err error) {
	c.mu.Lock()
	if c.isDead() {
		c.mu.Unlock()
		return
	}
	c.err = err
	c.listeners = map[int]func([]any){}
	for _, done := range c.drains {
		close(done)
	}
	c.queue, c.drains = nil, nil
	close(c.dead)
	ws := c.ws
	c.mu.Unlock()
	if ws != nil {
		ws.Close()
	}
}

// authenticate answers the relay's NIP-42 challenge with authKey: whether the relay took it. One
// AUTH per key and challenge, shared by the requests that ask for it. The wait for the relay's OK
// is part of the request's time (SPEC.md §13): ctx's error when ctx is done first (its time ran
// out, or the read went on without it), errClosed when the connection ends first; never a
// refusal, which the request would end with and the call would ask again.
func (c *conn) authenticate(ctx context.Context, p *Pool, authKey []byte) (bool, error) {
	// A request dropped meanwhile (its read went on) sends nothing more.
	if err := ctx.Err(); err != nil {
		return false, err
	}
	pub, err := secp.PublicKey(authKey)
	if err != nil {
		return false, nil
	}
	c.mu.Lock()
	if c.isDead() {
		c.mu.Unlock()
		return false, errClosed
	}
	if !c.hasChallenge {
		c.mu.Unlock()
		return false, nil
	}
	challenge := c.challenge
	id := pub + ":" + challenge
	a, ok := c.auths[id]
	if !ok {
		a = &auth{done: make(chan struct{})}
		c.auths[id] = a
	}
	c.mu.Unlock()
	if !ok {
		event, err := nostr.Sign(nostr.Template{
			CreatedAt: p.opts.Now(),
			Kind:      proto.AuthKind,
			Tags:      [][]string{{"relay", c.url}, {"challenge", challenge}},
			Content:   "",
		}, authKey, p.opts.Random)
		if err != nil {
			close(a.done)
			return false, nil
		}
		var once sync.Once
		stop := c.listen(func(message []any) {
			if len(message) < 2 || message[0] != "OK" || message[1] != event.ID {
				return
			}
			once.Do(func() {
				a.accepted = len(message) > 2 && message[2] == true
				close(a.done)
			})
		})
		go func() {
			select {
			case <-a.done:
			case <-c.dead:
			}
			stop()
		}()
		c.send([]any{"AUTH", event.JSON()})
	}
	select {
	case <-a.done:
		return a.accepted, nil
	case <-c.dead:
		return false, errClosed
	case <-ctx.Done():
		return false, ctx.Err()
	}
}
