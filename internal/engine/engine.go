// Package engine is the client of SPEC.md §4, as the public package checkout drives it: a shop's
// server for one sattle.me integration. It makes the invoice of each order (a Lightning invoice,
// a deposit address of the merchant's wallet, or both), declares it in the merchant's registry,
// checks its payment. Made for a server that lives for one call: nothing is kept between calls,
// each call reads the merchant (a removal stops the next invoice) and closes its connections.
package engine

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/sattleme/checkout-go/internal/bitcoinnode"
	"github.com/sattleme/checkout-go/internal/channel"
	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/httpx"
	"github.com/sattleme/checkout-go/internal/key"
	"github.com/sattleme/checkout-go/internal/lnurl"
	"github.com/sattleme/checkout-go/internal/logx"
	"github.com/sattleme/checkout-go/internal/model"
	"github.com/sattleme/checkout-go/internal/onchain"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/rates"
	"github.com/sattleme/checkout-go/internal/relay"
	"github.com/sattleme/checkout-go/internal/sales"
)

// Options is what an engine is made with, checked by the caller.
type Options struct {
	Key *key.IntegrationKey
	// RateSources are the client's: an order in a currency without its own uses them.
	RateSources     []rates.Source
	CoinGeckoAPIKey string
	// Transport carries every HTTP request.
	Transport http.RoundTripper
	// Dialer opens every WebSocket: the relays' and the explorer's.
	Dialer       relay.Dialer
	RelayTimeout time.Duration
	HTTPTimeout  time.Duration
	Logger       logx.Logger
	// Now is the clock.
	Now func() time.Time
	// Random is the CSPRNG.
	Random io.Reader
}

// Engine is the client.
type Engine struct {
	key         *key.IntegrationKey
	rateSources []rates.Source
	// coinGeckoAPIKey is behind a pointer: a value of the client that fmt prints with a verb its
	// fields do not take (%s of an integrator's struct holding it) shows the engine's own fields,
	// never what they point to.
	coinGeckoAPIKey *string
	http            *httpx.Client
	dialer          relay.Dialer
	header          http.Header
	relayTimeout    time.Duration
	logger          logx.Logger
	now             func() time.Time
	random          io.Reader
	// orders holds one invoice at a time per order, in this process.
	orders keyedLocks
	// restAsks holds one invoice of the rest at a time per invoice (ref/address), in this
	// process.
	restAsks keyedLocks
}

// New returns the engine of the options.
func New(o Options) *Engine {
	return &Engine{
		key:             o.Key,
		rateSources:     o.RateSources,
		coinGeckoAPIKey: &o.CoinGeckoAPIKey,
		http:            httpx.New(o.Transport, o.HTTPTimeout),
		dialer:          o.Dialer,
		header:          http.Header{"User-Agent": {proto.UserAgent}},
		relayTimeout:    o.RelayTimeout,
		logger:          o.Logger,
		now:             o.Now,
		random:          o.Random,
	}
}

// RateSources are the client's rate sources.
func (e *Engine) RateSources() []rates.Source { return e.rateSources }

// seconds is the server's clock, in Unix seconds rounded down.
func (e *Engine) seconds() int64 {
	return e.now().Unix()
}

// merchantChannel is what the server needs of the merchant, read with the key alone (the
// integration's channel).
type merchantChannel struct {
	businessName string
	// lightningAddress is never given to the integrator.
	lightningAddress string
	registry         derive.Registry
	// relays are the account's relays: where declarations, leases, witnesses and checkout links
	// go.
	relays []string
	// checkoutOrigin is the merchant's web app for checkout links, "" when its app gives none.
	checkoutOrigin string
	// tolerance is the merchant's tolerance of on-chain underpayments (basis points): a new
	// order's lease keeps it. Never in what the integrator or the customer gets.
	tolerance int64
	// node is the merchant's Bitcoin node (SPEC.md §31), nil for mempool.space: the pages of
	// checkout links read it too when its Payers is true.
	node *bitcoinnode.Node
	// explorer is the explorer the call reads: the node with its fallback, else mempool.space.
	explorer *onchain.Explorer
}

// callExplorer is what the reads of the explorer of one call share (SPEC.md §31, "in the same
// call"): when the node failed, and, for WaitForPayment, who hears the node each channel read
// names (its WebSocket follows it). nil: a call of its own, which no one hears.
type callExplorer struct {
	failures *onchain.NodeFailures
	onNode   func(node *bitcoinnode.Node)
}

// nodeFailures are the call's node failures; nil (a new call's own) without a call.
func (c *callExplorer) nodeFailures() *onchain.NodeFailures {
	if c == nil {
		return nil
	}
	return c.failures
}

// heard tells the call the node a channel read names.
func (c *callExplorer) heard(node *bitcoinnode.Node) {
	if c != nil && c.onNode != nil {
		c.onNode(node)
	}
}

// channel reads the integration's channel on the connections of pool, with the explorer of the
// call: its node's failures in failures (a new call's own when nil).
func (e *Engine) channel(ctx context.Context, pool *relay.Pool, logger logx.Logger, failures *onchain.NodeFailures) (*merchantChannel, error) {
	read, err := channel.ReadChannel(ctx, pool, e.key, channel.Options{Random: e.random, Logger: logger})
	if err != nil {
		return nil, err
	}
	c := read.Channel
	return &merchantChannel{
		businessName:     c.BusinessName,
		lightningAddress: c.LightningAddress,
		registry:         declaration.RegistryOf(c.RegistryKey, c.RegistryReader),
		relays:           channel.MerchantRelays(c, e.key),
		checkoutOrigin:   c.CheckoutOrigin,
		tolerance:        c.Tolerance,
		node:             c.BitcoinNode,
		explorer:         onchain.NewExplorer(e.http, c.BitcoinNode, failures, e.now),
	}, nil
}

// withRelays runs run with the relays of one call: one connection per relay, closed once the
// call is done (a write still running past the quorum first ends, bounded by its timeout).
func (e *Engine) withRelays(ctx context.Context, run func(pool *relay.Pool) error) error {
	pool := relay.NewPool(ctx, relay.Options{
		Dialer:  e.dialer,
		Timeout: e.relayTimeout,
		Now:     e.seconds,
		Random:  e.random,
		Header:  e.header,
	})
	defer pool.Release()
	return run(pool)
}

// lot is the integration's lot of deposit addresses, read on the merchant's relays.
func (e *Engine) lot(ctx context.Context, pool *relay.Pool, m *merchantChannel) ([]string, error) {
	return onchain.ReadLot(ctx, pool, e.key, m.relays, e.random, e.logger)
}

// declared reads the sale's records on the merchant's relays.
func (e *Engine) declared(ctx context.Context, pool *relay.Pool, sale *derive.Sale, m *merchantChannel, everyRelay bool, logger logx.Logger) (*sales.Records, error) {
	return sales.Read(ctx, pool, sale, m.relays, logger, everyRelay)
}

// Merchant reads the merchant: its business's name, the bounds of its Lightning address in sats,
// and how many deposit addresses the integration's lot holds. revoked when the merchant removed
// the integration. Never its Lightning address.
func (e *Engine) Merchant(ctx context.Context) (*model.Merchant, error) {
	var merchant *model.Merchant
	err := e.withRelays(ctx, func(pool *relay.Pool) error {
		m, err := e.channel(ctx, pool, e.logger, nil)
		if err != nil {
			return err
		}
		payCtx, cancelPay := context.WithCancel(ctx)
		defer cancelPay()
		payRequest := start(func() payResult {
			pay, err := lnurl.FetchPayRequest(payCtx, e.http, m.lightningAddress)
			return payResult{pay, err}
		})
		lot, err := e.lot(ctx, pool, m)
		if err != nil {
			cancelPay()
			payRequest.get()
			return err
		}
		pay := payRequest.get()
		if pay.err != nil {
			return pay.err
		}
		min, max := lnurl.SendableSats(pay.pay)
		merchant = &model.Merchant{BusinessName: m.businessName, MinSats: min, MaxSats: max, OnchainAddresses: len(lot)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return merchant, nil
}

// FindInvoices reads the invoices declared for the order ref (checked) in the merchant's
// registry, oldest first: to find an order's invoice again after a failure. Read on a majority of
// the merchant's relays (any declaration and lease was published on one); network when fewer
// answered.
func (e *Engine) FindInvoices(ctx context.Context, ref string) ([]*model.DeclaredInvoice, error) {
	var invoices []*model.DeclaredInvoice
	err := e.withRelays(ctx, func(pool *relay.Pool) error {
		m, err := e.channel(ctx, pool, e.logger, nil)
		if err != nil {
			return err
		}
		sale, err := derive.SaleOf(e.key.Key, m.registry, ref)
		if err != nil {
			return err
		}
		records, err := e.declared(ctx, pool, sale, m, false, e.logger)
		if err != nil {
			return err
		}
		invoices = make([]*model.DeclaredInvoice, len(records.Invoices))
		for i, d := range records.Invoices {
			invoices[i] = d.Invoice
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return invoices, nil
}

type payResult struct {
	pay *lnurl.PayRequest
	err error
}

// future is the outcome of a function run in its own goroutine, read any number of times.
type future[T any] struct {
	done  chan struct{}
	value T
}

// start runs f in its own goroutine.
func start[T any](f func() T) *future[T] {
	fu := &future[T]{done: make(chan struct{})}
	go func() {
		defer close(fu.done)
		fu.value = f()
	}()
	return fu
}

// ready is a future whose outcome is known.
func ready[T any](value T) *future[T] {
	fu := &future[T]{done: make(chan struct{}), value: value}
	close(fu.done)
	return fu
}

// get waits for the outcome.
func (f *future[T]) get() T {
	<-f.done
	return f.value
}

// keyedLocks holds one call at a time per key, in this process.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	ch    chan struct{}
	users int
}

// lock waits until key is free (or ctx is done: its error), then holds it until the function it
// returns is called.
func (k *keyedLocks) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	l := k.locks[key]
	if l == nil {
		l = &keyedLock{ch: make(chan struct{}, 1)}
		k.locks[key] = l
	}
	l.users++
	k.mu.Unlock()
	leave := func() {
		k.mu.Lock()
		l.users--
		if l.users == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
	select {
	case l.ch <- struct{}{}:
		return func() {
			<-l.ch
			leave()
		}, nil
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
}
