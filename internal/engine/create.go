package engine

import (
	"context"
	"encoding/binary"
	"io"
	"math/big"
	"sync"

	"github.com/sattleme/checkout-go/internal/address"
	"github.com/sattleme/checkout-go/internal/bitcoinnode"
	"github.com/sattleme/checkout-go/internal/bolt11"
	"github.com/sattleme/checkout-go/internal/checkoutlink"
	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/lnurl"
	"github.com/sattleme/checkout-go/internal/model"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/onchain"
	"github.com/sattleme/checkout-go/internal/order"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/rates"
	"github.com/sattleme/checkout-go/internal/relay"
	"github.com/sattleme/checkout-go/internal/sales"
)

// CreateInvoice makes the invoice of an order, checked (SPEC.md §17): a Lightning invoice asked
// to the merchant's Lightning address and checked, when its amount is within the address's
// bounds; a deposit address of the merchant's wallet leased from the integration's lot, from 1 000
// sats (or both). Then the invoice is declared in the merchant's registry on a majority of its
// relays. Only then may it be shown. With Checkout, its checkout link is published on a majority
// of the merchant's relays before the invoice is declared.
//
// One order, one invoice at a time (SPEC.md §21): invoice_pending (with that invoice) while an
// earlier invoice of the same ref may still be paid (or its on-chain payment is being confirmed),
// order_paid when one was paid; in this process, one call at a time per order.
func (e *Engine) CreateInvoice(ctx context.Context, o *order.Checked) (*model.Invoice, error) {
	// Two calls for one ref would both pass the check of its earlier invoices.
	unlock, err := e.orders.lock(ctx, o.Ref)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var invoice *model.Invoice
	err = e.withRelays(ctx, func(pool *relay.Pool) error {
		var err error
		invoice, err = e.createInvoice(ctx, pool, o)
		return err
	})
	if err != nil {
		return nil, err
	}
	return invoice, nil
}

type quoteResult struct {
	rate rates.Rate
	err  error
}

func (e *Engine) createInvoice(ctx context.Context, pool *relay.Pool, o *order.Checked) (*model.Invoice, error) {
	m, err := e.channel(ctx, pool, e.logger, nil)
	if err != nil {
		return nil, err
	}
	if o.Checkout && m.checkoutOrigin == "" {
		return nil, errs.NewCheckoutUnavailable()
	}
	sale, err := derive.SaleOf(e.key.Key, m.registry, o.Ref)
	if err != nil {
		return nil, err
	}
	// Asked while the registry is read: the Lightning address and the rate need nothing of it.
	askCtx, cancelAsks := context.WithCancel(ctx)
	defer cancelAsks()
	payRequest := start(func() payResult {
		pay, err := lnurl.FetchPayRequest(askCtx, e.http, m.lightningAddress)
		return payResult{pay, err}
	})
	var quote *future[quoteResult]
	if o.Fiat != nil && o.RateSources != nil {
		quote = start(func() quoteResult {
			rate, err := rates.FetchFirstRate(askCtx, o.RateSources, o.Fiat.Currency,
				rates.Context{HTTP: e.http, CoinGeckoAPIKey: *e.coinGeckoAPIKey}, e.seconds, e.logger, nil)
			return quoteResult{rate, err}
		})
	}
	// Both asks end before this call returns: a failure below cancels them.
	defer func() {
		cancelAsks()
		payRequest.get()
		if quote != nil {
			quote.get()
		}
	}()
	if err := e.checkEarlierInvoices(ctx, pool, sale, m, o); err != nil {
		return nil, err
	}
	pay := payRequest.get()
	if pay.err != nil {
		return nil, pay.err
	}

	var sats int64
	var rate *model.RateInfo
	param := "sats"
	if o.Fiat != nil {
		q := quote.get()
		if q.err != nil {
			return nil, q.err
		}
		converted := rates.MinorToSats(o.Fiat.Minor, o.Fiat.Digits, q.rate.Price)
		if converted.Cmp(big.NewInt(proto.MaxSats)) > 0 {
			return nil, errs.NewInvalidOrder("fiat", converted.String()+" sats is more than 21 million bitcoins")
		}
		sats = converted.Int64()
		info := q.rate.Info
		rate = &info
		param = "fiat"
	} else {
		sats = o.Sats
	}
	min, max := lnurl.SendableSats(pay.pay)
	methods, err := onchain.PaymentMethods(sats, min, max, param)
	if err != nil {
		return nil, err
	}

	var answer *lnurl.InvoiceAnswer
	var decoded *bolt11.Invoice
	if methods.Lightning {
		a, err := lnurl.RequestInvoice(ctx, e.http, pay.pay, sats, o.DurationSeconds, o.Label)
		if err != nil {
			return nil, err
		}
		decoded, err = lnurl.CheckInvoice(a, pay.pay, sats, o.DurationSeconds, e.seconds())
		if err != nil {
			return nil, err
		}
		answer = &a
	}
	leasedAt := e.seconds()
	createdAt, expiresAt := leasedAt, leasedAt+int64(o.DurationSeconds)
	if decoded != nil {
		createdAt, expiresAt = decoded.Timestamp, decoded.Timestamp+decoded.ExpirySeconds
	}
	price := declaration.Price{Minor: sats, Digits: 0, Currency: proto.SatsCurrency}
	if o.Fiat != nil {
		price = declaration.Price{Minor: o.Fiat.Minor, Digits: o.Fiat.Digits, Currency: o.Fiat.Currency}
	}
	bolt11Text, verifyURL := "", ""
	if answer != nil {
		bolt11Text, verifyURL = answer.PR, answer.Verify
	}

	var event *nostr.Event
	if answer != nil {
		event, err = declaration.Event(declaration.Input{
			Sale:        sale,
			Invoice:     answer.PR,
			VerifyURL:   answer.Verify,
			PaymentHash: decoded.PaymentHash,
			Price:       price,
			Note:        o.Label,
			CreatedAt:   e.seconds(),
			Random:      e.random,
		})
		if err != nil {
			// A valid invoice too long for a NIP-44 plaintext (65 535 bytes) cannot be declared.
			return nil, errs.NewInvalidInvoice("The invoice is too large to be declared", err)
		}
	}

	// The address before the link and the declaration: when the lease fails, nothing is published.
	onchainAddress := ""
	if methods.Onchain {
		terms := onchain.LeaseTerms{
			Sats:      sats,
			Invoice:   bolt11Text,
			VerifyURL: verifyURL,
			CreatedAt: createdAt,
			ExpiresAt: expiresAt,
			Price:     price,
			Note:      o.Label,
			// The order's for good: every check judges its payment with it.
			Tolerance: m.tolerance,
		}
		// The lease holds the invoice too, sealed twice as the declaration: an invoice it cannot
		// hold fails now, before anything is published.
		if err := onchain.CheckLeaseSize(sale, terms); err != nil {
			return nil, errs.NewInvalidInvoice("The invoice is too large for the lease of an address", err)
		}
		// Without an invoice, the lease's date is the Invoice's.
		leased, err := e.leaseAddress(ctx, pool, m, sale, terms, leasedAt, decoded == nil)
		if err != nil {
			return nil, err
		}
		if _, err := onchain.MethodsWith(methods, leased.outcome); err != nil {
			return nil, err
		}
		switch leased.outcome.Status {
		case onchain.Leased:
			onchainAddress = leased.address
		case onchain.ExplorerFailed:
			e.logger.Warn("github.com/sattleme/checkout-go: no deposit address leased, the explorer did not answer (" +
				leased.outcome.Err.Error() + "): Lightning only")
		}
	}

	// On a failure below, the lease stays: it holds its address until its end (a withdrawal on
	// some relays only would leave them disagreeing), and was confirmed: §21 may hand it out.
	checkoutURL := ""
	// The link before the declaration: when the link fails, nothing is declared and the order gets
	// a new invoice at once; when the declaration fails, the link's URL was never given and its
	// keys are gone, so nobody can open it.
	if o.Checkout {
		var fiat *declaration.Price
		if o.Fiat != nil {
			fiat = &price
		}
		checkoutURL, err = e.publishCheckout(ctx, pool, m, checkoutlink.Value(checkoutlink.Input{
			BusinessName:   m.businessName,
			Invoice:        bolt11Text,
			VerifyURL:      verifyURL,
			OnchainAddress: onchainAddress,
			Sats:           sats,
			CreatedAt:      createdAt,
			ExpiresAt:      expiresAt,
			Fiat:           fiat,
			Note:           o.Label,
			RedirectURL:    o.RedirectURL,
			Rest:           e.linkRest(o.Ref, onchainAddress, expiresAt),
			BitcoinNode:    bitcoinnode.ForRecord(m.node),
		}), expiresAt)
		if err != nil {
			return nil, err
		}
	}
	if event != nil {
		if err := e.declare(ctx, pool, event, sale, m); err != nil {
			return nil, err
		}
	}

	invoice := &model.Invoice{
		Ref:            o.Ref,
		Bolt11:         bolt11Text,
		VerifyURL:      verifyURL,
		OnchainAddress: onchainAddress,
		PaymentURI:     address.PaymentURI(onchainAddress, sats, bolt11Text),
		Sats:           sats,
		Fiat:           o.Fiat,
		Rate:           rate,
		Note:           o.Label,
		CreatedAt:      createdAt,
		ExpiresAt:      expiresAt,
		CheckoutURL:    checkoutURL,
	}
	if decoded != nil {
		invoice.PaymentHash = decoded.PaymentHash
	}
	if o.Checkout {
		invoice.RedirectURL = o.RedirectURL
	}
	return invoice, nil
}

// linkRest is what a checkout link to the invoice of ref with address (none: "") and the end
// expiresAt carries of its rest keys: nil without an address.
func (e *Engine) linkRest(ref, onchainAddress string, expiresAt int64) *checkoutlink.Rest {
	if onchainAddress == "" {
		return nil
	}
	keys := derive.RestKeys(e.key.Key, ref, onchainAddress, expiresAt)
	return &checkoutlink.Rest{Author: keys.Author, Key: keys.Key}
}

// leased is what leasing an address gave: the outcome, and when leased, the address and its
// lease (marked).
type leased struct {
	outcome onchain.LeaseOutcome
	address string
	event   *nostr.Event
}

// leaseAddress leases a deposit address of the integration's lot for sale (SPEC.md §31, "Leasing
// an address"): the lot and its leases read; of its free addresses, in a random order, at most 3
// tried, one at a time: one the explorer shows paid already is skipped (its witness none
// published, so that the merchant's app takes it out of the lot); else the lease is published on a
// majority, the address's leases read again on every relay, and the lease confirmed then marked,
// or withdrawn when the read is not complete or another author's leases it too (the later of two
// servers sees both). A lease once confirmed is never withdrawn. ownDate: the lease's date is
// leasedAt (the Invoice's, of an order paid on-chain only). network when the relays fail.
func (e *Engine) leaseAddress(ctx context.Context, pool *relay.Pool, m *merchantChannel, sale *derive.Sale, terms onchain.LeaseTerms, leasedAt int64, ownDate bool) (leased, error) {
	lot, err := e.lot(ctx, pool, m)
	if err != nil {
		return leased{}, err
	}
	if len(lot) == 0 {
		return leased{outcome: onchain.LeaseOutcome{Status: onchain.NoAddress}}, nil
	}
	relays := m.relays
	seen := onchain.SeenOf(e.key.Key)
	// The leases, complete or not (the confirmation decides), and the addresses a witness names.
	type namedResult struct {
		named map[string]bool
		err   error
	}
	namedRead := start(func() namedResult {
		named, err := onchain.ReadNamedAddresses(ctx, pool, seen, lot, relays, e.random)
		return namedResult{named, err}
	})
	leases, err := onchain.ReadLeases(ctx, pool, m.registry.Key, lot, relays, e.seconds(), e.random, false)
	named := namedRead.get()
	if err != nil {
		return leased{}, err
	}
	if named.err != nil {
		return leased{}, named.err
	}
	if !leases.Answered {
		return leased{}, errs.NewNetwork("Fewer than a majority of the merchant's relays answered", nil)
	}
	if hasNewerLease(leases.Records) {
		e.logger.Warn("github.com/sattleme/checkout-go: a lease of the integration's addresses has a newer format than this version reads; " +
			"update the package")
	}
	now := e.seconds()
	// Not where the read shows a record of the sale (its signature checked): an earlier call of
	// the order tried it, and a new lease could be of the second of its withdrawal, and either
	// replace the other.
	var free []string
	for _, a := range lot {
		records := leases.Records[a]
		if named.named[a] || onchain.IsLeased(records, now) || hasSignedRecordOf(records, sale.Author, "") {
			continue
		}
		free = append(free, a)
	}
	free, err = shuffle(free, e.random)
	if err != nil {
		return leased{}, err
	}
	if len(free) > proto.MaxAddressTries {
		free = free[:proto.MaxAddressTries]
	}
	for _, a := range free {
		// The explorer of the call: the node with its fallback, else mempool.space. With "none" and
		// the node down, no address (SPEC.md §31).
		txs, err := onchain.ReadTransactions(ctx, m.explorer, a)
		if err != nil {
			if ctx.Err() != nil {
				return leased{}, ctx.Err()
			}
			if !errs.Is(err, errs.Network) {
				return leased{}, err
			}
			return leased{outcome: onchain.LeaseOutcome{Status: onchain.ExplorerFailed, Err: err}}, nil
		}
		var paid []*onchain.ExplorerTx
		for _, tx := range txs {
			if onchain.PaysTo(tx, a) > 0 {
				paid = append(paid, tx)
			}
		}
		if len(paid) > 0 {
			// Paid on-chain already: of no order, for good.
			if err := e.publishWitnessesOfNone(ctx, pool, m, seen, a, paid); err != nil {
				return leased{}, err
			}
			continue
		}
		// Published unconfirmed: it holds the address, never gives the invoice one.
		createdAt := e.seconds()
		if ownDate {
			createdAt = leasedAt
		}
		unconfirmed := terms
		unconfirmed.Address, unconfirmed.Confirmed, unconfirmed.HasConfirmed = a, false, true
		event, err := onchain.LeaseEventOf(sale, unconfirmed, createdAt, e.random)
		if err != nil {
			return leased{}, err
		}
		err = e.publishLeaseRecord(ctx, pool, relays, event, sale)
		var again *onchain.LeaseRead
		if err == nil {
			// On every relay, each until its timeout: a relay that hides a lease cannot hide it
			// while one that holds it answers in time (SPEC.md §13, §27).
			again, err = onchain.ReadLeases(ctx, pool, m.registry.Key, []string{a}, relays, e.seconds(), e.random, true)
		}
		if err != nil {
			// On some relays only (or the call cancelled): withdrawn, never the order's.
			e.withdraw(ctx, pool, relays, sale, a, event)
			return leased{}, err
		}
		records := again.Records[a]
		own := onchain.LeaseEvent{ID: event.ID, PubKey: event.PubKey, CreatedAt: event.CreatedAt, Sig: event.Sig}
		if !again.Confirms(a, own, e.seconds()) {
			// Another server leased it at once (the later reader sees both), the read could not be
			// complete, or fewer than a majority answered: withdrawn (safe on some relays only: the
			// address was never the order's), unless the read shows another record of the sale
			// there whose signature checks out, even from fewer than a majority: another call of
			// the order made it at once (its lock failed, SPEC.md §17), and it is not this call's to
			// end.
			if !hasSignedRecordOf(records, sale.Author, event.ID) {
				e.withdraw(ctx, pool, relays, sale, a, event)
			}
			if !again.Answered {
				return leased{}, errs.NewNetwork("Fewer than a majority of the merchant's relays answered", nil)
			}
			// The next one.
			continue
		}
		// Confirmed: never withdrawn from now on, whatever fails (a withdrawal on some relays only
		// would leave them disagreeing on an address that may be the order's). Marked confirmed:
		// from now on the address is the order's, its invoice's in §21 too.
		mark := terms
		mark.Address, mark.Confirmed, mark.HasConfirmed = a, true, true
		markedAt := e.seconds()
		if markedAt < event.CreatedAt+1 {
			markedAt = event.CreatedAt + 1
		}
		marked, err := onchain.LeaseEventOf(sale, mark, markedAt, e.random)
		if err != nil {
			return leased{}, err
		}
		if err := e.publishLeaseRecord(ctx, pool, relays, marked, sale); err != nil {
			return leased{}, err
		}
		return leased{outcome: onchain.LeaseOutcome{Status: onchain.Leased}, address: a, event: marked}, nil
	}
	return leased{outcome: onchain.LeaseOutcome{Status: onchain.NoAddress}}, nil
}

// hasSignedRecordOf tells whether records hold a record of author, other than the event except
// (an id, or ""), whose signature checks out.
func hasSignedRecordOf(records []*onchain.LeaseRecord, author, except string) bool {
	for _, r := range records {
		if r.Event.PubKey == author && r.Event.ID != except && r.Signed() {
			return true
		}
	}
	return false
}

// withdraw withdraws lease, the lease of address by sale that was not confirmed: its withdrawal,
// a second after it (so that it replaces the lease, and no later record of the sale), published
// on the merchant's relays; a failure only logged (the lease then holds its address until its
// end, and never gives its invoice one).
func (e *Engine) withdraw(ctx context.Context, pool *relay.Pool, relays []string, sale *derive.Sale, address string, lease *nostr.Event) {
	if ctx.Err() != nil {
		return
	}
	event, err := onchain.WithdrawalEvent(sale, address, lease.CreatedAt+1, e.random)
	if err == nil {
		err = pool.PublishToQuorum(relays, event, sale.SecretKey, relay.Majority(len(relays)))
	}
	if err != nil && ctx.Err() == nil {
		e.logger.Warn("github.com/sattleme/checkout-go: a lease that was not confirmed could not be withdrawn; it " +
			"holds its deposit address until its end")
	}
}

// publishLeaseRecord publishes a lease record on a majority of the merchant's relays, else
// network.
func (e *Engine) publishLeaseRecord(ctx context.Context, pool *relay.Pool, relays []string, event *nostr.Event, sale *derive.Sale) error {
	if err := pool.PublishToQuorum(relays, event, sale.SecretKey, relay.Majority(len(relays))); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errs.NewNetwork("Fewer than a majority of the merchant's relays took the lease of a deposit address: "+
			"try again", err)
	}
	return nil
}

// hasNewerLease tells whether a record of the leases is of a newer format: it holds its address
// (never written over, SPEC.md §12). Its signature is not checked (that of a record that holds
// never is): only a holder of the registry key seals one.
func hasNewerLease(records map[string][]*onchain.LeaseRecord) bool {
	for _, list := range records {
		for _, r := range list {
			if r.Outside.Kind == onchain.OutsideNewer {
				return true
			}
		}
	}
	return false
}

// publishWitnessesOfNone publishes a witness none of each transaction of txs paying address,
// failures logged.
func (e *Engine) publishWitnessesOfNone(ctx context.Context, pool *relay.Pool, m *merchantChannel, seen *onchain.SeenKeys, address string, txs []*onchain.ExplorerTx) error {
	var wg sync.WaitGroup
	failures := make([]error, len(txs))
	for i, tx := range txs {
		wg.Add(1)
		go func(i int, tx *onchain.ExplorerTx) {
			defer wg.Done()
			inputs := tx.Coins
			if len(inputs) > proto.MaxWitnessInputs {
				inputs = inputs[:proto.MaxWitnessInputs]
			}
			err := onchain.PublishWitness(ctx, pool, seen, onchain.Witness{
				Address: address,
				Txid:    tx.Txid,
				Sats:    onchain.PaysTo(tx, address),
				Inputs:  inputs,
			}, m.relays, e.seconds(), e.random)
			if err == nil {
				return
			}
			if errs.Is(err, errs.Network) {
				e.logger.Warn("github.com/sattleme/checkout-go: the witness of a paid address was not published: " + err.Error())
				return
			}
			failures[i] = err
		}(i, tx)
	}
	wg.Wait()
	for _, err := range failures {
		if err != nil {
			return err
		}
	}
	return nil
}

// publishCheckout publishes a new checkout link holding value on a majority of the merchant's
// relays, and gives its URL. The author's secret answers the relays' authentication, then is
// dropped with the link: nobody can change it afterwards. network when fewer than a majority took
// it.
func (e *Engine) publishCheckout(ctx context.Context, pool *relay.Pool, m *merchantChannel, value jsonv.Members, expiresAt int64) (string, error) {
	link, err := checkoutlink.New(m.checkoutOrigin, m.relays, value, expiresAt, e.seconds(), e.random)
	if err != nil {
		return "", errs.NewInvalidInvoice("The invoice is too large for a checkout link", err)
	}
	if err := pool.PublishToQuorum(m.relays, link.Event, link.SecretKey, relay.Majority(len(m.relays))); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errs.NewNetwork("Fewer than a majority of the merchant's relays took the checkout link: try again", err)
	}
	return link.URL, nil
}

// checkEarlierInvoices refuses a new invoice for the sale while an earlier one may still be paid
// (until its end plus 120 s, or while its on-chain payment is being confirmed or undecided), or
// when one was paid: read in the registry, so that it holds even when the shop lost what it stored
// (SPEC.md §21).
func (e *Engine) checkEarlierInvoices(ctx context.Context, pool *relay.Pool, sale *derive.Sale, m *merchantChannel, o *order.Checked) error {
	records, err := e.declared(ctx, pool, sale, m, false, e.logger)
	if err != nil {
		return err
	}
	earlier := records.Invoices
	// A few at a time; a paid invoice wins over a service that did not answer about another one.
	check := func(list []sales.Declaration) []sales.Checked {
		return settleEach(list, proto.MaxParallelPaymentChecks, func(d sales.Declaration) sales.Checked {
			stored := storedOf(d.Invoice)
			p, err := e.check(ctx, stored, &scope{pool: pool, m: m, records: records}, e.logger, nil)
			if err != nil {
				return sales.Checked{Err: err}
			}
			status, err := statusOf(stored, p, e.seconds())
			return sales.Checked{Status: status, Err: err}
		})
	}
	invoices := make([]*model.DeclaredInvoice, len(earlier))
	for i, d := range earlier {
		invoices[i] = d.Invoice
	}
	var mayBePaid []sales.Declaration
	for _, i := range sales.MayStillBePaid(invoices, e.seconds()) {
		mayBePaid = append(mayBePaid, earlier[i])
	}
	if len(mayBePaid) > 0 {
		// Their payments checked, even of one alone: an invoice already paid is never handed out
		// again. Several: an earlier createInvoice failed once a minority of the relays took its
		// last record, and a later one made another invoice and showed it; the sale's records are
		// read on every relay at the same time, and the one shown is the one a majority holds.
		type everywhereResult struct {
			records *sales.Records
			err     error
		}
		var everywhere *future[everywhereResult]
		if len(mayBePaid) > 1 {
			everywhere = start(func() everywhereResult {
				r, err := e.declared(ctx, pool, sale, m, true, e.logger)
				return everywhereResult{r, err}
			})
		}
		statuses := check(mayBePaid)
		var read everywhereResult
		if everywhere != nil {
			read = everywhere.get()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var held []bool
		if everywhere != nil && read.err == nil {
			held = sales.Held(read.records, mayBePaid, len(m.relays))
		}
		outcome := sales.PendingOutcome(statuses, held)
		switch outcome.Kind {
		case sales.OutcomePaid:
			return errs.NewOrderPaid(mayBePaid[outcome.Index].Invoice, statuses[outcome.Index].Status)
		case sales.OutcomeFailed:
			return statuses[outcome.Index].Err
		case sales.OutcomeUnread:
			// Fewer than a majority answered the read again: after a payment and an error.
			if read.err != nil {
				return read.err
			}
			return errs.NewNetwork("Fewer than a majority of the merchant's relays answered", nil)
		}
		open := mayBePaid[outcome.Index]
		// Its createInvoice may have failed with fewer than a majority of the relays holding its
		// declaration or its lease ("do not show this invoice"): published again before it is
		// handed out. Its lease, with an address, was confirmed (SPEC.md §31, "Leasing an
		// address"): the address is the order's.
		if open.Event != nil {
			if err := e.declare(ctx, pool, open.Event, sale, m); err != nil {
				return err
			}
		}
		if open.Lease != nil {
			if err := e.publishLease(ctx, pool, open.Lease, sale, m); err != nil {
				return err
			}
		}
		invoice := open.Invoice
		// A new link to it, for a shop that lost the one it stored (or never got it): links only
		// show the invoice, several of them change nothing.
		checkoutURL := ""
		if o.Checkout {
			var fiat *declaration.Price
			if invoice.Fiat != nil {
				fiat = &declaration.Price{Minor: invoice.Fiat.Minor, Digits: invoice.Fiat.Digits, Currency: invoice.Fiat.Currency}
			}
			checkoutURL, err = e.publishCheckout(ctx, pool, m, checkoutlink.Value(checkoutlink.Input{
				BusinessName:   m.businessName,
				Invoice:        invoice.Bolt11,
				VerifyURL:      invoice.VerifyURL,
				OnchainAddress: invoice.OnchainAddress,
				Sats:           invoice.Sats,
				CreatedAt:      invoice.CreatedAt,
				ExpiresAt:      invoice.ExpiresAt,
				Fiat:           fiat,
				Note:           invoice.Note,
				RedirectURL:    o.RedirectURL,
				Rest:           e.linkRest(o.Ref, invoice.OnchainAddress, invoice.ExpiresAt),
				BitcoinNode:    bitcoinnode.ForRecord(m.node),
			}), invoice.ExpiresAt)
			if err != nil {
				return err
			}
		}
		return errs.NewInvoicePending(invoice, checkoutURL)
	}
	statuses := check(earlier)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for i, s := range statuses {
		if s.Err == nil && s.Status.State == model.Paid {
			return errs.NewOrderPaid(earlier[i].Invoice, s.Status)
		}
	}
	// An on-chain payment being confirmed, or a transaction undecided: its time to be paid is
	// over, nothing is published again, and no new link (the shop keeps the one it got, whose page
	// goes on following the payment).
	for i, s := range statuses {
		if s.Err == nil && (s.Status.State == model.Unconfirmed || s.Status.State == model.Open) {
			return errs.NewInvoicePending(earlier[i].Invoice, "")
		}
	}
	for _, s := range statuses {
		if s.Err != nil {
			return s.Err
		}
	}
	return nil
}

// declare publishes a declaration on a majority of the merchant's relays, else network.
func (e *Engine) declare(ctx context.Context, pool *relay.Pool, event *nostr.Event, sale *derive.Sale, m *merchantChannel) error {
	if err := pool.PublishToQuorum(m.relays, event, sale.SecretKey, relay.Majority(len(m.relays))); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errs.NewNetwork("Fewer than a majority of the merchant's relays took the invoice's declaration: do not "+
			"show this invoice, try again", err)
	}
	return nil
}

// publishLease publishes a lease again on a majority of the merchant's relays, else network.
func (e *Engine) publishLease(ctx context.Context, pool *relay.Pool, event *nostr.Event, sale *derive.Sale, m *merchantChannel) error {
	if err := pool.PublishToQuorum(m.relays, event, sale.SecretKey, relay.Majority(len(m.relays))); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errs.NewNetwork("Fewer than a majority of the merchant's relays took the invoice's lease: do not show "+
			"this invoice, try again", err)
	}
	return nil
}

// settleEach runs run on every item, limit at a time: the outcome of each, in order.
func settleEach(items []sales.Declaration, limit int, run func(sales.Declaration) sales.Checked) []sales.Checked {
	results := make([]sales.Checked, len(items))
	var mu sync.Mutex
	next := 0
	var wg sync.WaitGroup
	workers := min(limit, len(items))
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				index := next
				next++
				mu.Unlock()
				if index >= len(items) {
					return
				}
				results[index] = run(items[index])
			}
		}()
	}
	wg.Wait()
	return results
}

// shuffle is items in a random order, from the CSPRNG (so that two servers rarely try the same
// address first): Fisher–Yates, each draw without bias.
func shuffle(items []string, random io.Reader) ([]string, error) {
	shuffled := append([]string(nil), items...)
	for i := len(shuffled) - 1; i > 0; i-- {
		bound := uint64(i + 1)
		// The largest multiple of bound within 2^32: draws at or above it are drawn again.
		limit := uint64(1<<32) - uint64(1<<32)%bound
		var draw uint64
		for {
			var b [4]byte
			if _, err := io.ReadFull(random, b[:]); err != nil {
				return nil, err
			}
			draw = uint64(binary.BigEndian.Uint32(b[:]))
			if draw < limit {
				break
			}
		}
		j := draw % bound
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	return shuffled, nil
}
