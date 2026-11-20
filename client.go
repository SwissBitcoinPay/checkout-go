package checkout

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"time"

	"github.com/sattleme/checkout-go/internal/engine"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/key"
	"github.com/sattleme/checkout-go/internal/model"
	"github.com/sattleme/checkout-go/internal/order"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/rates"
	transportpkg "github.com/sattleme/checkout-go/internal/transport"
)

// Client is a shop's server for one sattle.me integration (SPEC.md §4): it makes the invoice of
// each order (a Lightning invoice, a deposit address of the merchant's wallet, or both), declares
// it in the merchant's registry, and checks its payment. Made for a server that lives for one call
// (it starts, makes an invoice, stops): nothing is kept between calls, each call reads the merchant
// (a removal stops the next invoice) and closes its connections; there is nothing to close. Safe
// for concurrent use. Its string, its debug writing and its JSON never hold the key.
type Client struct {
	team   string
	relays []string
	e      *engine.Engine
}

// Option is an option of New.
type Option func(*settings)

type settings struct {
	rateSources     []RateSource
	hasRateSources  bool
	coinGeckoAPIKey string
	hasCoinGecko    bool
	transport       http.RoundTripper
	hasTransport    bool
	dialer          WebSocketDialer
	hasDialer       bool
	relayTimeout    time.Duration
	hasRelayTimeout bool
	httpTimeout     time.Duration
	hasHTTPTimeout  bool
	logger          Logger
	hasLogger       bool
	now             func() time.Time
	hasNow          bool
	random          io.Reader
	hasRandom       bool
}

// WithRateSource is where invoices priced in a currency get their rate: a source, or a list of
// them asked in order, the next one only when the previous one gives no price (at least one, no
// name twice). DefaultRateSources by default. Each order may give its own.
func WithRateSource(sources ...RateSource) Option {
	return func(s *settings) { s.rateSources, s.hasRateSources = sources, true }
}

// WithCoinGeckoAPIKey is CoinGecko's Demo API key, sent to CoinGecko only: 1 or more visible ASCII
// characters. Without it, its keyless API.
func WithCoinGeckoAPIKey(apiKey string) Option {
	return func(s *settings) { s.coinGeckoAPIKey, s.hasCoinGecko = apiKey, true }
}

// WithHTTPTransport carries every HTTP request (the Lightning address, the rate sources, the
// explorer, BlockCypher): for tests and proxies. A merchant's Bitcoin node of a local network is
// asked in clear (an http:// URL, SPEC.md §31): the transport sends it as it is, an http.Transport
// over its DialContext and its Proxy; plain http goes to no other URL. By default,
// http.DefaultTransport's settings, each connection dialed (its name resolved) within its request's
// context. The package follows no redirect, bounds each request and reads 1 MiB of an answer at
// most, whatever the transport: a request whose time ran out, or whose call was cancelled, is let
// go even when the transport does not heed its context (its answer, when it comes, is closed
// unread). Called from several goroutines at once: an http.RoundTripper must be safe for concurrent
// use.
func WithHTTPTransport(transport http.RoundTripper) Option {
	return func(s *settings) { s.transport, s.hasTransport = transport, true }
}

// WithWebSocketDialer opens every WebSocket (the relays', the explorer's, a merchant's Bitcoin
// node's ws:// for one of a local network): for tests and proxies. NewWebSocketDialer(nil) by
// default; NewWebSocketDialer(transport) sends them over your own transport. Called from several
// goroutines at once (WebSocketDialer); the connections of a publication's relays past the majority
// are still used once its call returned, for the relay timeout at most.
func WithWebSocketDialer(d WebSocketDialer) Option {
	return func(s *settings) { s.dialer, s.hasDialer = d, true }
}

// WithRelayTimeout bounds each relay request, its connection included: a whole number of
// milliseconds, 100 ms to 60 s; 10 s by default.
func WithRelayTimeout(d time.Duration) Option {
	return func(s *settings) { s.relayTimeout, s.hasRelayTimeout = d, true }
}

// WithHTTPTimeout bounds each HTTP request (Lightning address, rate sources, explorer,
// BlockCypher), its body included: a whole number of milliseconds, 100 ms to 60 s; 10 s by default.
func WithHTTPTimeout(d time.Duration) Option {
	return func(s *settings) { s.httpTimeout, s.hasHTTPTimeout = d, true }
}

// WithLogger is where warnings go: slog's default logger by default. Never given a secret. Called
// from several goroutines at once: it must be safe for concurrent use.
func WithLogger(l Logger) Option {
	return func(s *settings) { s.logger, s.hasLogger = l, true }
}

// WithClock is the clock: time.Now by default. For tests only. Called from several goroutines at
// once, and by a publication's relays past the majority for their one authentication (NIP-42) up
// to the relay timeout once its call returned: it must be safe for concurrent use.
func WithClock(now func() time.Time) Option {
	return func(s *settings) { s.now, s.hasNow = now, true }
}

// WithRandom is where random bytes come from: crypto/rand by default. For reproducible tests
// only: anything but a CSPRNG makes the keys of checkout links guessable. When the reader fails,
// the call fails with invalid_option (random). Read from several goroutines at once, and by a
// publication's relays past the majority for their one authentication (NIP-42) up to the relay
// timeout once its call returned: it must be safe for concurrent use, as crypto/rand's is.
func WithRandom(r io.Reader) Option {
	return func(s *settings) { s.random, s.hasRandom = r, true }
}

func invalidOption(param, message string) error {
	return &Error{Code: CodeInvalidOption, Param: param, Message: param + ": " + message}
}

// isNil tells whether v, the value of an option of an interface type, holds nothing: nil, or a
// nil pointer, function, map, slice or channel (a typed nil: a nil *http.Transport given as the
// http.RoundTripper is no nil interface). Taken, it would panic at the first request, in one of
// the call's goroutines, where no recover of the caller reaches: SPEC.md §4 has New refuse it.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	switch value := reflect.ValueOf(v); value.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan, reflect.Interface, reflect.UnsafePointer:
		return value.IsNil()
	}
	return false
}

// milliseconds checks a duration of an option: a whole number of milliseconds within its bounds.
func milliseconds(d time.Duration, given bool, fallback, low, high int64, param string) (time.Duration, error) {
	if !given {
		return time.Duration(fallback) * time.Millisecond, nil
	}
	if d%time.Millisecond != 0 || d < time.Duration(low)*time.Millisecond || d > time.Duration(high)*time.Millisecond {
		return 0, invalidOption(param, fmt.Sprintf("a whole number of milliseconds, %d to %d", low, high))
	}
	return d, nil
}

// New is the client of the integration key (sattleme_ck_…, "Integrations" in the merchant's
// app: from the environment or a vault, never in a repository). invalid_key when it is not one;
// invalid_option for an invalid option, before any request.
func New(integrationKey string, opts ...Option) (*Client, error) {
	k, problem := key.Parse(integrationKey)
	if problem != "" {
		return nil, publicError(errs.NewInvalidKey(string(problem)))
	}
	var s settings
	for _, opt := range opts {
		if opt == nil {
			return nil, invalidOption("options", "an option of this package, not nil")
		}
		opt(&s)
	}
	sources := rates.Default()
	if s.hasRateSources {
		list, ok := rates.SourcesOf(sourcesOf(s.rateSources))
		if !ok {
			return nil, invalidOption("rateSource", order.RateSourceRule)
		}
		sources = list
	}
	// Visible ASCII: its HTTP header carries it as it is.
	if s.hasCoinGecko && !isVisibleASCII(s.coinGeckoAPIKey) {
		return nil, invalidOption("coingeckoApiKey", "1 or more visible ASCII characters")
	}
	var transport http.RoundTripper = transportpkg.Default
	if s.hasTransport {
		if isNil(s.transport) {
			return nil, invalidOption("httpTransport", "an http.RoundTripper, not nil")
		}
		transport = s.transport
	}
	var dial WebSocketDialer = NewWebSocketDialer(nil)
	if s.hasDialer {
		if isNil(s.dialer) {
			return nil, invalidOption("webSocketDialer", "a WebSocketDialer, not nil")
		}
		if d, ok := s.dialer.(dialer); ok && d.d == nil {
			return nil, invalidOption("webSocketDialer", "NewWebSocketDialer of an http.RoundTripper, not of a nil one")
		}
		dial = s.dialer
	}
	relayTimeout, err := milliseconds(s.relayTimeout, s.hasRelayTimeout, proto.DefaultTimeoutMs, proto.MinTimeoutMs, proto.MaxTimeoutMs, "relayTimeoutMs")
	if err != nil {
		return nil, err
	}
	httpTimeout, err := milliseconds(s.httpTimeout, s.hasHTTPTimeout, proto.DefaultTimeoutMs, proto.MinTimeoutMs, proto.MaxTimeoutMs, "httpTimeoutMs")
	if err != nil {
		return nil, err
	}
	var logger Logger = defaultLogger{}
	if s.hasLogger {
		if isNil(s.logger) {
			return nil, invalidOption("logger", "a Logger, not nil")
		}
		logger = s.logger
	}
	now := time.Now
	if s.hasNow {
		if s.now == nil {
			return nil, invalidOption("now", "a function, not nil")
		}
		now = s.now
	}
	var random io.Reader = rand.Reader
	if s.hasRandom {
		if isNil(s.random) {
			return nil, invalidOption("random", "an io.Reader, not nil")
		}
		random = randomSource{s.random}
	}
	return &Client{
		team:   k.TeamPublicKey,
		relays: append([]string(nil), k.Relays...),
		e: engine.New(engine.Options{
			Key:             k,
			RateSources:     sources,
			CoinGeckoAPIKey: s.coinGeckoAPIKey,
			Transport:       transport,
			Dialer:          engineDialer{dial},
			RelayTimeout:    relayTimeout,
			HTTPTimeout:     httpTimeout,
			Logger:          logger,
			Now:             now,
			Random:          random,
		}),
	}, nil
}

// randomSource is the reader of WithRandom as the package reads it: when it fails, wherever that
// is, the call fails with invalid_option (random), an error of the package (SPEC.md §5), what it
// said as the cause. crypto/rand's never fails.
type randomSource struct{ r io.Reader }

func (s randomSource) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil {
		return n, &errs.Error{Code: errs.InvalidOption, Param: "random", Message: "random: the random source failed", Cause: errs.Sanitize(err)}
	}
	return n, nil
}

func isVisibleASCII(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// String is the integration key's team and relays: no secret.
func (c *Client) String() string {
	return fmt.Sprintf("checkout.Client{team: %s, relays: %v}", c.team, c.relays)
}

// GoString is String: no secret.
func (c *Client) GoString() string { return c.String() }

// Format writes String whatever the verb: no secret.
func (c *Client) Format(f fmt.State, _ rune) { fmt.Fprint(f, c.String()) }

// MarshalJSON is the integration key's team and relays: no secret.
func (c *Client) MarshalJSON() ([]byte, error) {
	return jsonv.Marshal(jsonv.O("teamPublicKey", c.team, "relays", c.relays)), nil
}

// done is what a call that ended gives: ctx's error once ctx is done, whatever its steps gave
// (SPEC.md §4: a cancelled call fails with the platform's cancellation, even when a step that
// does not heed it, a custom rate source, delayed it); else err as an Error of the package, or
// nil.
func done(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return nil
	}
	return publicError(err)
}

func orBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// Merchant reads the merchant: its business's name, the bounds of its Lightning address in sats,
// and how many deposit addresses the integration's lot holds. revoked when the merchant removed
// the integration. Never its Lightning address.
func (c *Client) Merchant(ctx context.Context) (*Merchant, error) {
	ctx = orBackground(ctx)
	m, err := c.e.Merchant(ctx)
	if err := done(ctx, err); err != nil {
		return nil, err
	}
	return merchantOf(m), nil
}

// Price is a price in a currency, as an order gives it: {Amount: "24.50", Currency: "CHF"}, a
// string, never a float.
type Price struct {
	// Amount is a string of digits with at most the currency's decimals: "24.50".
	Amount string
	// Currency is an ISO 4217 code in upper case: "CHF".
	Currency string
}

// Order is an order: its reference, and its amount in sats or its price in a currency.
type Order struct {
	// Ref is the order's reference, 1 to 64 characters A–Z a–z 0–9 . _ : -: one order, one sale.
	Ref string
	// Sats is the amount in sats, 1 to 21 million bitcoins; or Fiat.
	Sats int64
	// Fiat is the price in a currency, converted at the rate of RateSources.
	Fiat *Price
	// RateSources is the rate source for this invoice (Fiat only), or a list of them asked in
	// order; the client's when nil.
	RateSources []RateSource
	// DurationSeconds is how long the invoice can be paid: 60 to 3600; 900 when 0.
	DurationSeconds int
	// Note is what the merchant's app shows, 100 characters at most; the reference when "".
	Note string
	// Checkout asks for a checkout link: a URL of the merchant's web app where the customer pays
	// the invoice (Invoice.CheckoutURL).
	Checkout bool
	// RedirectURL is, with Checkout only, where the checkout link sends the customer once the
	// invoice is paid: an https URL or an app's link (myshop://order/42). Not a proof of payment.
	RedirectURL string
}

func (o Order) internal() order.Order {
	internal := order.Order{
		Ref:             o.Ref,
		Sats:            o.Sats,
		HasSats:         o.Sats != 0,
		DurationSeconds: int64(o.DurationSeconds),
		HasDuration:     o.DurationSeconds != 0,
		Note:            o.Note,
		HasNote:         o.Note != "",
		Checkout:        o.Checkout,
		RedirectURL:     o.RedirectURL,
		HasRedirectURL:  o.RedirectURL != "",
	}
	if o.Fiat != nil {
		internal.Fiat = &order.Price{Amount: o.Fiat.Amount, Currency: o.Fiat.Currency}
	}
	if o.RateSources != nil {
		internal.RateSources, internal.HasRateSources = sourcesOf(o.RateSources), true
	}
	return internal
}

// CreateInvoice makes the invoice of an order (SPEC.md §17): a Lightning invoice asked to the
// merchant's Lightning address and checked, when its amount is within the address's bounds; a
// deposit address of the merchant's wallet leased from the integration's lot, from 1 000 sats (or
// both). Then the invoice is declared in the merchant's registry on a majority of its relays. Only
// then may it be shown. Store the result with the order.
//
// One order, one invoice at a time: invoice_pending (with that invoice) while an earlier invoice of
// the same Ref may still be paid (or its on-chain payment is being confirmed), order_paid when one
// was paid. onchain_unavailable when the order can only be paid on-chain and no address could be
// leased; network when, for such an order, the explorer did not answer (mempool.space, or the
// merchant's Bitcoin node with its fallback, SPEC.md §31). With an address and Checkout, the link's
// record names the merchant's Bitcoin node when its app lets the payers read it.
//
// With Checkout, the invoice also gets a checkout link (CheckoutURL): a page of the merchant's web
// app that shows it to the customer, checks its payment and, once it is paid, sends the customer to
// RedirectURL. It is published on a majority of the merchant's relays before the invoice is
// declared. checkout_unavailable when the merchant's app gives no web app for it.
func (c *Client) CreateInvoice(ctx context.Context, o Order) (*Invoice, error) {
	ctx = orBackground(ctx)
	checked, err := order.Check(o.internal(), c.e.RateSources())
	if err != nil {
		return nil, publicError(err)
	}
	invoice, err := c.e.CreateInvoice(ctx, checked)
	if err := done(ctx, err); err != nil {
		return nil, err
	}
	return invoiceOf(invoice), nil
}

// stored is the engine's stored invoice of invoice, checked (SPEC.md §22): invalid_order
// (invoice) when it is not one.
func stored(invoice Checkable) (*engine.Stored, error) {
	switch v := invoice.(type) {
	case nil:
		return nil, errNotInvoice()
	case *Invoice:
		if v == nil {
			return nil, errNotInvoice()
		}
	case *DeclaredInvoice:
		if v == nil {
			return nil, errNotInvoice()
		}
	case *InvoiceToCheck:
		if v == nil {
			return nil, errNotInvoice()
		}
	}
	c := invoice.toCheck()
	checked, lightning, err := order.CheckStored(order.Stored{
		Ref:            c.Ref,
		Bolt11:         c.Bolt11,
		PaymentHash:    c.PaymentHash,
		VerifyURL:      c.VerifyURL,
		OnchainAddress: c.OnchainAddress,
		Sats:           c.Sats,
		ExpiresAt:      c.ExpiresAt,
	})
	if err != nil {
		return nil, publicError(err)
	}
	return &engine.Stored{Ref: checked.Ref, Lightning: lightning, OnchainAddress: checked.OnchainAddress, Sats: checked.Sats, ExpiresAt: checked.ExpiresAt}, nil
}

// GetStatus checks where the payment of an invoice is (SPEC.md §22): its Lightning part from the
// merchant's Lightning address (LUD-21), its on-chain part from the explorer and the witnesses of
// its order, and the Lightning invoices of the rest of an on-chain underpayment, all at once.
// Underpaid on-chain, it asks the merchant's Lightning address for an invoice of the rest, declares
// it and publishes it for the order's checkout links (Rest.Bolt11; a failure is logged, the next
// check asks again). It never opens the explorer's WebSocket. network (retry) when a service does
// not answer, invalid_invoice when an answer is not about this invoice or its preimage is wrong,
// invalid_order (invoice) when invoice is not one.
func (c *Client) GetStatus(ctx context.Context, invoice Checkable) (*PaymentStatus, error) {
	ctx = orBackground(ctx)
	s, err := stored(invoice)
	if err != nil {
		return nil, err
	}
	status, err := c.e.GetStatus(ctx, s)
	if err := done(ctx, err); err != nil {
		return nil, err
	}
	return statusOf(status), nil
}

// WaitOption is an option of WaitForPayment.
type WaitOption func(*waitSettings)

type waitSettings struct {
	interval    time.Duration
	hasInterval bool
	onStatus    func(*PaymentStatus)
	hasOnStatus bool
}

// WithInterval is the time between two checks: a whole number of milliseconds, 250 ms to 60 s;
// 2 s by default.
func WithInterval(d time.Duration) WaitOption {
	return func(s *waitSettings) { s.interval, s.hasInterval = d, true }
}

// WithOnStatus hears each status, from WaitForPayment's own goroutine.
func WithOnStatus(f func(*PaymentStatus)) WaitOption {
	return func(s *waitSettings) { s.onStatus, s.hasOnStatus = f, true }
}

// WaitForPayment checks the payment until it is paid or expired (SPEC.md §23): its Lightning part,
// and the invoices of the rest of an on-chain underpayment, every interval; its on-chain part (with
// an address) each time the explorer's WebSocket (mempool.space's, or the merchant's Bitcoin node's
// with its fallback, opened once a check read the channel) says something changed for the address,
// every 60 s at least, and at once when an invoice of the rest is paid. Network errors are retried
// (one that each check meets again logged once, and again only after it stopped). ctx's error when
// it is done, and any error that is not a network one. Whatever it returns, every check it started
// has stopped by then: none asks nor publishes anything once it returned. An on-chain payment takes
// about 30 minutes to be paid once unconfirmed: a server that cannot wait that long checks again
// later with GetStatus.
func (c *Client) WaitForPayment(ctx context.Context, invoice Checkable, opts ...WaitOption) (*PaymentStatus, error) {
	ctx = orBackground(ctx)
	var w waitSettings
	for _, opt := range opts {
		if opt == nil {
			return nil, invalidOption("options", "an option of this package, not nil")
		}
		opt(&w)
	}
	interval, err := milliseconds(w.interval, w.hasInterval, proto.DefaultIntervalMs, proto.MinIntervalMs, proto.MaxIntervalMs, "intervalMs")
	if err != nil {
		return nil, err
	}
	if w.hasOnStatus && w.onStatus == nil {
		return nil, invalidOption("onStatus", "a function, not nil")
	}
	s, err := stored(invoice)
	if err != nil {
		return nil, err
	}
	var onStatus func(*model.PaymentStatus)
	if w.onStatus != nil {
		f := w.onStatus
		onStatus = func(status *model.PaymentStatus) { f(statusOf(status)) }
	}
	status, err := c.e.WaitForPayment(ctx, s, interval, onStatus)
	if err := done(ctx, err); err != nil {
		return nil, err
	}
	return statusOf(status), nil
}

// FindInvoices reads the invoices declared for the order ref in the merchant's registry, oldest
// first: to find an order's invoice again after a failure. Read on a majority of the merchant's
// relays (any declaration and lease was published on one); network when fewer answered.
func (c *Client) FindInvoices(ctx context.Context, ref string) ([]DeclaredInvoice, error) {
	ctx = orBackground(ctx)
	if err := order.CheckRef(ref); err != nil {
		return nil, publicError(err)
	}
	invoices, err := c.e.FindInvoices(ctx, ref)
	if err := done(ctx, err); err != nil {
		return nil, err
	}
	out := make([]DeclaredInvoice, len(invoices))
	for i, d := range invoices {
		out[i] = *declaredOf(d)
	}
	return out, nil
}
