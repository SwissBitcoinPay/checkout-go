package testkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/wsdial"
)

// Behaviour is what a local relay does besides NIP-01 and NIP-42.
type Behaviour struct {
	// Refuse refuses every event with this reason.
	Refuse string
	// RefuseEvent refuses the events it gives a reason for.
	RefuseEvent func(e *nostr.Event) string
	// Mute accepts connections and never sends anything.
	Mute bool
	// RequireAuth asks every connection to authenticate (NIP-42) before reading, and as an event's
	// author before writing it; a connection keeps every key it authenticated. Whatever it is, an
	// authentication sent as an EVENT is refused.
	RequireAuth bool
	// Cap sends at most this many events per query, the newest first (strfry: 500); 0 for none.
	Cap int
	// MuteQuery never answers the queries it gives true for (no event, no EOSE).
	MuteQuery func(filter *jsonv.Object) bool
	// QueryDelay answers each query this late (writes at once).
	QueryDelay time.Duration
	// OldestFirstWithoutLimit answers a filter without limit oldest first, as nostr-rs-relay does.
	OldestFirstWithoutLimit bool
	// Hide leaves out of its answers the events it gives true for: a hostile relay.
	Hide func(event any) bool
	// OnEvent hears each event the relay took, once it keeps it.
	OnEvent func(e *nostr.Event, r *Relay)
	// Raw, when set, answers each message itself (a hostile relay): what it returns is sent, as it
	// is; the relay's own handling is skipped when handled is true.
	Raw func(message []any, send func(text string)) (handled bool)
}

// OnFirst is an OnEvent, for one or several relays, that runs f when a relay keeps the first event
// match gives true for, before that relay answers it; a relay that keeps another such event
// meanwhile waits for f to end before it answers too. So whatever f writes is on the relays before
// any of them took the event: what single-threaded relays would do, f running at once.
func OnFirst(match func(e *nostr.Event) bool, f func()) func(e *nostr.Event, r *Relay) {
	var once sync.Once
	return func(e *nostr.Event, _ *Relay) {
		if match(e) {
			once.Do(f)
		}
	}
}

// Relay is a local Nostr relay (NIP-01, NIP-42): it keeps the newest event of each address, as
// strfry does, and behaves as its Behaviour says.
type Relay struct {
	URL string

	mu        sync.Mutex
	behaviour Behaviour
	events    []any
	received  []*nostr.Event
	queries   []*jsonv.Object
	authed    []string
	closedIDs []string
	reqIDs    []string
	headers   []http.Header
	conns     []*websocket.Conn
	open      int
}

// SetBehaviour changes what the relay does.
func (r *Relay) SetBehaviour(b Behaviour) {
	r.mu.Lock()
	r.behaviour = b
	r.mu.Unlock()
}

// Behaviour is what the relay does.
func (r *Relay) Behaviour() Behaviour {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.behaviour
}

// Store keeps v as it is (an invalid event too: a relay is never trusted), as JSON read back:
// what a relay holds and sends.
func (r *Relay) Store(v any) {
	value, err := jsonv.Parse(jsonv.Marshal(v))
	if err != nil {
		panic(err)
	}
	r.mu.Lock()
	r.events = append(r.events, value)
	r.mu.Unlock()
}

// StoreEvent keeps an event of the package as it is.
func (r *Relay) StoreEvent(e *nostr.Event) { r.Store(EventValue(e)) }

// Take takes event as a client's write: the newest of its address is kept.
func (r *Relay) Take(e *nostr.Event) {
	r.mu.Lock()
	r.keep(e)
	r.mu.Unlock()
}

// Events are the events the relay holds.
func (r *Relay) Events() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]any(nil), r.events...)
}

// SetEvents replaces the events the relay holds.
func (r *Relay) SetEvents(events []any) {
	r.mu.Lock()
	r.events = append([]any(nil), events...)
	r.mu.Unlock()
}

// Received are the valid events clients sent, accepted or not.
func (r *Relay) Received() []*nostr.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*nostr.Event(nil), r.received...)
}

// Queries are the filters clients asked.
func (r *Relay) Queries() []*jsonv.Object {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*jsonv.Object(nil), r.queries...)
}

// Authenticated are the public keys that authenticated.
func (r *Relay) Authenticated() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.authed...)
}

// Subscriptions are the subscriptions clients opened (NIP-01 REQ).
func (r *Relay) Subscriptions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.reqIDs...)
}

// AllClosed tells whether clients closed every subscription they opened.
func (r *Relay) AllClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	closed := map[string]bool{}
	for _, id := range r.closedIDs {
		closed[id] = true
	}
	for _, id := range r.reqIDs {
		if !closed[id] {
			return false
		}
	}
	return true
}

// ClosedSubscriptions are the subscriptions clients closed (NIP-01 CLOSE).
func (r *Relay) ClosedSubscriptions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.closedIDs...)
}

// Handshakes are the headers of each connection's opening handshake.
func (r *Relay) Handshakes() []http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]http.Header(nil), r.headers...)
}

// EventValue is an event as a relay holds it: JSON, read back.
func EventValue(e *nostr.Event) any {
	v, err := jsonv.Parse(jsonv.Marshal(e.JSON()))
	if err != nil {
		panic(err)
	}
	return v
}

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	c, err := websocket.Accept(w, req, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled, InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 24)
	r.mu.Lock()
	r.headers = append(r.headers, req.Header.Clone())
	r.conns = append(r.conns, c)
	r.open++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.open--
		r.mu.Unlock()
	}()
	r.serve(req.Context(), c)
}

// Open is how many connections the relay has open now.
func (r *Relay) Open() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.open
}

func (r *Relay) serve(ctx context.Context, c *websocket.Conn) {
	defer c.CloseNow()
	var writeMu sync.Mutex
	send := func(text string) {
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.Write(wctx, websocket.MessageText, []byte(text))
	}
	sendJSON := func(message []any) { send(string(jsonv.Marshal(message))) }
	challenge := randomHex(16)
	// The keys this connection authenticated (NIP-42): one authentication per key and challenge,
	// every key kept, as SPEC.md §13 has a client authenticate.
	authed := map[string]bool{}
	if b := r.Behaviour(); b.RequireAuth && !b.Mute {
		sendJSON([]any{"AUTH", challenge})
	}
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		b := r.Behaviour()
		if b.Mute {
			continue
		}
		value, err := jsonv.Parse(data)
		if err != nil {
			continue
		}
		message, ok := value.([]any)
		if !ok || len(message) == 0 {
			continue
		}
		if b.Raw != nil && b.Raw(message, send) {
			continue
		}
		var rest []any
		if len(message) > 1 {
			rest = message[1:]
		}
		switch message[0] {
		case "AUTH":
			var authValue any
			if len(rest) > 0 {
				authValue = rest[0]
			}
			e, valid := nostr.Valid(authValue)
			ok := valid && e.Kind == 22242 && hasTag(e.Tags, "challenge", challenge)
			if ok {
				authed[e.PubKey] = true
				r.mu.Lock()
				r.authed = append(r.authed, e.PubKey)
				r.mu.Unlock()
			}
			id, _ := jsonv.Field(authValue, "id")
			reason := "invalid: bad auth"
			if ok {
				reason = ""
			}
			sendJSON([]any{"OK", id, ok, reason})
		case "EVENT":
			var eventValue any
			if len(rest) > 0 {
				eventValue = rest[0]
			}
			id, _ := jsonv.Field(eventValue, "id")
			pubkey, _ := jsonv.Field(eventValue, "pubkey")
			e, valid := nostr.Valid(eventValue)
			if valid {
				r.mu.Lock()
				r.received = append(r.received, e)
				r.mu.Unlock()
			}
			switch {
			case valid && e.Kind == 22242:
				// NIP-42: an authentication is an AUTH message, never an EVENT (SPEC.md §13): it is
				// neither kept nor taken as one.
				sendJSON([]any{"OK", id, false, "invalid: an authentication is sent as AUTH"})
			case b.RequireAuth && !authed[fmt.Sprint(pubkey)]:
				sendJSON([]any{"OK", id, false, "auth-required: sign in first"})
			case b.Refuse != "":
				sendJSON([]any{"OK", id, false, b.Refuse})
			case valid && b.RefuseEvent != nil && b.RefuseEvent(e) != "":
				sendJSON([]any{"OK", id, false, b.RefuseEvent(e)})
			case !valid:
				sendJSON([]any{"OK", id, false, "invalid: bad event"})
			default:
				r.mu.Lock()
				r.keep(e)
				r.mu.Unlock()
				if b.OnEvent != nil {
					b.OnEvent(e, r)
				}
				sendJSON([]any{"OK", e.ID, true, ""})
			}
		case "REQ":
			if len(rest) == 0 {
				continue
			}
			subscription := rest[0]
			if id, ok := subscription.(string); ok {
				r.mu.Lock()
				r.reqIDs = append(r.reqIDs, id)
				r.mu.Unlock()
			}
			var filters []*jsonv.Object
			for _, f := range rest[1:] {
				if o, ok := jsonv.AsObject(f); ok {
					filters = append(filters, o)
				}
			}
			r.mu.Lock()
			r.queries = append(r.queries, filters...)
			r.mu.Unlock()
			if b.RequireAuth && len(authed) == 0 {
				sendJSON([]any{"CLOSED", subscription, "auth-required: sign in first"})
				continue
			}
			muted := false
			for _, f := range filters {
				if b.MuteQuery != nil && b.MuteQuery(f) {
					muted = true
				}
			}
			if muted {
				continue
			}
			answer := func() {
				for _, e := range r.match(filters, b) {
					sendJSON([]any{"EVENT", subscription, e})
				}
				sendJSON([]any{"EOSE", subscription})
			}
			if b.QueryDelay > 0 {
				time.AfterFunc(b.QueryDelay, answer)
			} else {
				answer()
			}
		case "CLOSE":
			if len(rest) > 0 {
				id, _ := rest[0].(string)
				r.mu.Lock()
				r.closedIDs = append(r.closedIDs, id)
				r.mu.Unlock()
			}
		}
	}
}

func hasTag(tags [][]string, name, value string) bool {
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == name && tag[1] == value {
			return true
		}
	}
	return false
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// keep keeps the newest event of an address (kind 30000 to 39999), as relays do (NIP-01).
func (r *Relay) keep(e *nostr.Event) {
	if e.Kind >= 30000 && e.Kind < 40000 {
		d, _ := e.DTag()
		for i, other := range r.events {
			if kind, _ := number(field(other, "kind")); kind != e.Kind || field(other, "pubkey") != e.PubKey || firstD(other) != d {
				continue
			}
			createdAt, _ := number(field(other, "created_at"))
			id, _ := field(other, "id").(string)
			// The newest kept; of one second, the lowest id (NIP-01).
			if createdAt > float64(e.CreatedAt) || createdAt == float64(e.CreatedAt) && id <= e.ID {
				return
			}
			r.events = append(r.events[:i], r.events[i+1:]...)
			break
		}
	}
	r.events = append(r.events, EventValue(e))
}

// firstD is the value of the first d tag of a stored event, "" without one.
func firstD(v any) string {
	tags, _ := field(v, "tags").([]any)
	for _, t := range tags {
		if tag, ok := t.([]any); ok && len(tag) >= 2 && tag[0] == "d" {
			d, _ := tag[1].(string)
			return d
		}
	}
	return ""
}

func number(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func field(v any, name string) any {
	f, _ := jsonv.Field(v, name)
	return f
}

func contains(list any, v any) bool {
	items, _ := list.([]any)
	for _, item := range items {
		if item == v {
			return true
		}
	}
	return false
}

// match is the events that match one of the filters, newest first (on a tie, the lowest id),
// capped.
func (r *Relay) match(filters []*jsonv.Object, b Behaviour) []any {
	r.mu.Lock()
	events := append([]any(nil), r.events...)
	r.mu.Unlock()
	var found []any
	for _, e := range events {
		if b.Hide != nil && b.Hide(e) {
			continue
		}
		for _, f := range filters {
			if matches(e, f) {
				found = append(found, e)
				break
			}
		}
	}
	createdAt := func(e any) float64 { n, _ := number(field(e, "created_at")); return n }
	sort.SliceStable(found, func(i, j int) bool {
		a, b := createdAt(found[i]), createdAt(found[j])
		if a != b {
			return a > b
		}
		ai, _ := field(found[i], "id").(string)
		bi, _ := field(found[j], "id").(string)
		return ai < bi
	})
	noLimit := true
	limit := -1
	for _, f := range filters {
		if v, ok := f.Get("limit"); ok {
			noLimit = false
			if n, ok := number(v); ok && (limit < 0 || int(n) < limit) {
				limit = int(n)
			}
		}
	}
	if b.OldestFirstWithoutLimit && noLimit {
		for i, j := 0, len(found)-1; i < j; i, j = i+1, j-1 {
			found[i], found[j] = found[j], found[i]
		}
	}
	if b.Cap > 0 && (limit < 0 || b.Cap < limit) {
		limit = b.Cap
	}
	if limit >= 0 && limit < len(found) {
		found = found[:limit]
	}
	return found
}

func matches(e any, f *jsonv.Object) bool {
	if ids, ok := f.Get("ids"); ok && !contains(ids, field(e, "id")) {
		return false
	}
	if kinds, ok := f.Get("kinds"); ok && !contains(kinds, field(e, "kind")) {
		return false
	}
	if authors, ok := f.Get("authors"); ok && !contains(authors, field(e, "pubkey")) {
		return false
	}
	for _, name := range []string{"d", "o"} {
		values, ok := f.Get("#" + name)
		if !ok {
			continue
		}
		tags, _ := field(e, "tags").([]any)
		found := false
		for _, t := range tags {
			tag, ok := t.([]any)
			if ok && len(tag) >= 2 && tag[0] == name && contains(values, tag[1]) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	createdAt, _ := number(field(e, "created_at"))
	if since, ok := f.Get("since"); ok {
		if n, ok := number(since); ok && createdAt < n {
			return false
		}
	}
	if until, ok := f.Get("until"); ok {
		if n, ok := number(until); ok && createdAt > n {
			return false
		}
	}
	return true
}

// closeAll ends every connection of the relay.
func (r *Relay) closeAll() {
	r.mu.Lock()
	conns := r.conns
	r.conns = nil
	r.mu.Unlock()
	for _, c := range conns {
		c.CloseNow()
	}
}

// Net is the relays and WebSocket servers of a test, by their wss:// host names, on loopback
// listeners that the injected dialer reaches: wss://relay.example names the local relay of that
// name, its TLS stood in for (the connection is plain), never the network.
type Net struct {
	mu         sync.Mutex
	relays     map[string]*Relay
	handlers   map[string]http.Handler
	listeners  map[string]net.Listener
	down       map[string]bool
	blackholes map[string]bool
	// plain are the addresses served in clear (ws://, http://: a merchant's Bitcoin node of a
	// local network); every other one is reached as if over TLS.
	plain       map[string]bool
	connections map[string]int
	held        []net.Conn
	servers     []*http.Server
	transport   *http.Transport
}

// NewNet returns an empty network.
func NewNet() *Net {
	n := &Net{
		relays:      map[string]*Relay{},
		handlers:    map[string]http.Handler{},
		listeners:   map[string]net.Listener{},
		down:        map[string]bool{},
		blackholes:  map[string]bool{},
		plain:       map[string]bool{},
		connections: map[string]int{},
	}
	n.transport = &http.Transport{
		// In clear, only to what a test serves in clear.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			n.mu.Lock()
			plain := n.plain[addr]
			if !plain {
				n.connections[addr]++
			}
			n.mu.Unlock()
			if !plain {
				return nil, errors.New("testkit: no plain connection")
			}
			return n.dial(ctx, network, addr)
		},
		DialTLSContext:    n.dial,
		DisableKeepAlives: true,
	}
	return n
}

// hostOf is the name a URL is reached at: its host and port, 443 when absent (80 for ws:// and
// http://).
func hostOf(rawURL string) string {
	rest, port := rawURL, ":443"
	if isPlain(rawURL) {
		port = ":80"
	}
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if !strings.Contains(rest, ":") {
		rest += port
	}
	return rest
}

// isPlain tells whether a URL is reached in clear: ws:// or http://.
func isPlain(rawURL string) bool {
	return strings.HasPrefix(rawURL, "ws://") || strings.HasPrefix(rawURL, "http://")
}

func (n *Net) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	n.mu.Lock()
	n.connections[addr]++
	listener, ok := n.listeners[addr]
	hole := n.blackholes[addr]
	down := n.down[addr]
	n.mu.Unlock()
	if hole {
		// Takes the connection and never answers the handshake.
		client, server := net.Pipe()
		n.mu.Lock()
		n.held = append(n.held, server)
		n.mu.Unlock()
		return client, nil
	}
	if !ok || down {
		return nil, errors.New("testkit: connection refused: " + addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", listener.Addr().String())
}

func (n *Net) serve(rawURL string, h http.Handler) error {
	addr := hostOf(rawURL)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	server := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	n.mu.Lock()
	if old, ok := n.listeners[addr]; ok {
		old.Close()
	}
	n.listeners[addr] = listener
	n.handlers[addr] = h
	n.plain[addr] = isPlain(rawURL)
	n.servers = append(n.servers, server)
	delete(n.down, addr)
	n.mu.Unlock()
	go server.Serve(listener)
	return nil
}

// Add starts a local relay at url.
func (n *Net) Add(url string, b Behaviour) *Relay {
	r := &Relay{URL: url, behaviour: b}
	if err := n.serve(url, r); err != nil {
		panic(err)
	}
	n.mu.Lock()
	n.relays[hostOf(url)] = r
	n.mu.Unlock()
	return r
}

// Get returns the relay at url.
func (n *Net) Get(url string) *Relay {
	n.mu.Lock()
	defer n.mu.Unlock()
	r, ok := n.relays[hostOf(url)]
	if !ok {
		panic("testkit: no relay " + url)
	}
	return r
}

// AddServer serves url with a handler of the test (a WebSocket server of its own, a redirect…).
func (n *Net) AddServer(url string, h http.Handler) {
	if err := n.serve(url, h); err != nil {
		panic(err)
	}
}

// AddBlackhole makes url take connections and never answer the WebSocket handshake.
func (n *Net) AddBlackhole(url string) {
	n.mu.Lock()
	n.blackholes[hostOf(url)] = true
	n.mu.Unlock()
}

// Down makes url refuse connections from now on (nothing listens): its relay stops.
func (n *Net) Down(url string) {
	addr := hostOf(url)
	n.mu.Lock()
	n.down[addr] = true
	r := n.relays[addr]
	n.mu.Unlock()
	if r != nil {
		r.closeAll()
	}
}

// Connections is how many connections url got.
func (n *Net) Connections(url string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.connections[hostOf(url)]
}

// Transport is the HTTP transport WebSocket handshakes go through.
func (n *Net) Transport() *http.Transport { return n.transport }

// Dialer is the package's own WebSocket client (wsdial) over the network's transport.
func (n *Net) Dialer() *wsdial.Dialer { return wsdial.New(n.transport) }

// Close stops every server and connection of the network.
func (n *Net) Close() {
	n.mu.Lock()
	relays := make([]*Relay, 0, len(n.relays))
	for _, r := range n.relays {
		relays = append(relays, r)
	}
	servers := n.servers
	held := n.held
	n.mu.Unlock()
	for _, r := range relays {
		r.closeAll()
	}
	for _, s := range servers {
		s.Close()
	}
	for _, c := range held {
		c.Close()
	}
	n.transport.CloseIdleConnections()
}
