package engine

import (
	"context"
	"sync"

	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/lnurl"
	"github.com/sattleme/checkout-go/internal/logx"
	"github.com/sattleme/checkout-go/internal/model"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/onchain"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/relay"
	"github.com/sattleme/checkout-go/internal/sales"
)

// Stored is a stored invoice, checked (order.CheckStored): what its payment check asks.
type Stored struct {
	// Ref is its order's reference, with an address ("" without one).
	Ref string
	// Lightning is its Lightning invoice, nil for an order paid on-chain only.
	Lightning      *lnurl.Invoice
	OnchainAddress string
	Sats           int64
	ExpiresAt      int64
}

// parts is the last result of each part of an invoice (nil: it has none).
type parts struct {
	lightning *onchain.LightningResult
	onchain   *onchain.OnchainResult
	rests     *onchain.RestPart
}

// scope is the relays a check reads over: the call's pool, the merchant read on it, and the
// sale's records when they were read already.
type scope struct {
	pool    *relay.Pool
	m       *merchantChannel
	records *sales.Records
}

// statusOf is the status of stored from its parts, at now (Unix seconds); a part's error.
func statusOf(stored *Stored, p *parts, now int64) (*model.PaymentStatus, error) {
	return onchain.CombineParts(onchain.InvoiceTerms{Sats: stored.Sats, ExpiresAt: stored.ExpiresAt, Address: stored.OnchainAddress},
		now, p.lightning, p.onchain, p.rests)
}

// storedOf is what a payment check asks of a declared invoice.
func storedOf(invoice *model.DeclaredInvoice) *Stored {
	s := &Stored{Ref: invoice.Ref, OnchainAddress: invoice.OnchainAddress, Sats: invoice.Sats, ExpiresAt: invoice.ExpiresAt}
	if invoice.Bolt11 != "" {
		s.Lightning = &lnurl.Invoice{Bolt11: invoice.Bolt11, PaymentHash: invoice.PaymentHash, VerifyURL: invoice.VerifyURL, ExpiresAt: invoice.ExpiresAt}
	}
	return s
}

// lightningOf is what the Lightning part of a payment check needs of a rest invoice.
func lightningOf(invoice onchain.RestInvoice) lnurl.Invoice {
	return lnurl.Invoice{Bolt11: invoice.Bolt11, PaymentHash: invoice.PaymentHash, VerifyURL: invoice.VerifyURL, ExpiresAt: invoice.ExpiresAt}
}

// isFinal tells whether a Lightning part is final: paid, or expired for good.
func isFinal(r onchain.LightningResult) bool {
	return r.Err == nil && r.Part.State != lnurl.Open
}

func isPaid(r onchain.LightningResult) bool {
	return r.Err == nil && r.Part.State == lnurl.Paid
}

// verify is the Lightning part of inv, checked now: its part, or the error it failed with.
func (e *Engine) verify(ctx context.Context, inv lnurl.Invoice) onchain.LightningResult {
	part, err := lnurl.VerifyPayment(ctx, e.http, inv, e.seconds())
	return onchain.LightningResult{Part: part, Err: err}
}

// verifyAll checks the Lightning part of each invoice at once.
func (e *Engine) verifyAll(ctx context.Context, invoices []lnurl.Invoice) []onchain.LightningResult {
	results := make([]onchain.LightningResult, len(invoices))
	var wg sync.WaitGroup
	for i, inv := range invoices {
		wg.Add(1)
		go func(i int, inv lnurl.Invoice) {
			defer wg.Done()
			results[i] = e.verify(ctx, inv)
		}(i, inv)
	}
	wg.Wait()
	return results
}

// GetStatus checks where the payment of an invoice is (SPEC.md §22): its Lightning part from the
// merchant's Lightning address (LUD-21), its on-chain part from the explorer and the witnesses of
// its order, and the Lightning invoices of the rest of an on-chain underpayment, all at once.
// Underpaid on-chain, it asks the merchant's Lightning address for an invoice of the rest,
// declares it and publishes it for the order's checkout links (a failure is logged, the next
// check asks again). It never opens the explorer's WebSocket.
func (e *Engine) GetStatus(ctx context.Context, stored *Stored) (*model.PaymentStatus, error) {
	p, err := e.check(ctx, stored, nil, e.logger, nil)
	if err != nil {
		return nil, err
	}
	return statusOf(stored, p, e.seconds())
}

// check checks every part of stored at once (SPEC.md §31, "The on-chain payment"): its Lightning
// part, and with an address its on-chain part and its rest part, read on the merchant's relays
// (over sc when given, with the sale's records when already read; else over the connections of a
// call of its own, the channel read with the explorer of call: its node's failures, and who hears
// the node it names); an invoice of the rest asked when the status calls for one. A part that
// fails is its error, never returned; a cancellation is.
func (e *Engine) check(ctx context.Context, stored *Stored, sc *scope, logger logx.Logger, call *callExplorer) (*parts, error) {
	lightning := ready[*onchain.LightningResult](nil)
	if stored.Lightning != nil {
		inv := *stored.Lightning
		lightning = start(func() *onchain.LightningResult {
			r := e.verify(ctx, inv)
			return &r
		})
	}
	if stored.OnchainAddress == "" {
		p := &parts{lightning: lightning.get()}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return p, nil
	}
	var p *parts
	var err error
	if sc != nil {
		p, err = e.addressParts(ctx, stored, sc, lightning, logger)
	} else {
		err = e.withRelays(ctx, func(pool *relay.Pool) error {
			m, err := e.channel(ctx, pool, logger, call.nodeFailures())
			if err != nil {
				return err
			}
			call.heard(m.node)
			p, err = e.addressParts(ctx, stored, &scope{pool: pool, m: m}, lightning, logger)
			return err
		})
	}
	if err != nil {
		l := lightning.get()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The relays or the channel failed: the address's parts cannot be told.
		return &parts{lightning: l, onchain: &onchain.OnchainResult{Err: err}}, nil
	}
	return p, nil
}

type recordsResult struct {
	records *sales.Records
	err     error
}

type restResult struct {
	part *onchain.RestPart
	err  error
}

// addressParts checks the on-chain part and the rest part of stored (it has an address and a
// reference), at once: the sale's records (its rest invoices, its lease) read while the explorer
// is asked; then, when the status is open with a rest and no invoice of it, one asked (askRest);
// and the records of its rest invoices published again where no relay holds them.
func (e *Engine) addressParts(ctx context.Context, stored *Stored, sc *scope, lightningCheck *future[*onchain.LightningResult], logger logx.Logger) (*parts, error) {
	pool, m := sc.pool, sc.m
	address := stored.OnchainAddress
	sale, err := derive.SaleOf(e.key.Key, m.registry, stored.Ref)
	if err != nil {
		lightningCheck.get()
		return nil, err
	}
	read := ready(recordsResult{records: sc.records})
	if sc.records == nil {
		read = start(func() recordsResult {
			r, err := e.declared(ctx, pool, sale, m, false, logger)
			return recordsResult{r, err}
		})
	}
	restCheck := start(func() restResult {
		found := read.get()
		if found.err != nil {
			return restResult{err: found.err}
		}
		// This invoice's own: another invoice of the order may have leased the address before.
		invoices := found.records.Rests[sales.RestKeyOf(address, stored.ExpiresAt)]
		// One whose declaration may be on a minority of the relays only (its publication failed): on
		// a majority before it counts, is shown or recorded, else the rest part fails (network).
		for _, inv := range invoices {
			if event := found.records.UnsureRests[inv.PaymentHash]; event != nil {
				if err := e.declare(ctx, pool, event, sale, m); err != nil {
					return restResult{err: err}
				}
			}
		}
		checks := make([]lnurl.Invoice, len(invoices))
		for i, inv := range invoices {
			checks[i] = lightningOf(inv)
		}
		return restResult{part: &onchain.RestPart{Invoices: invoices, Results: e.verifyAll(ctx, checks)}}
	})
	// The order's tolerance, its lease's (the invoice's own terms): without the records, or another
	// lease of the address, none.
	tolerance := func() int64 {
		found := read.get()
		if found.err != nil {
			return 0
		}
		lease := found.records.Leases[address]
		if lease != nil && lease.Terms.Sats == stored.Sats && lease.Terms.ExpiresAt == stored.ExpiresAt {
			return lease.Terms.Tolerance
		}
		return 0
	}
	seen := onchain.SeenOf(e.key.Key)
	// The merchant's acceptance of this invoice: the team's record, read on every relay.
	accepted := onchain.MerchantAcceptanceKeysOf(e.key.Key, e.key.TeamPublicKey, sale.Author, address, stored.ExpiresAt)
	checkOnchain := func(paidByRests func() int64) *onchain.OnchainResult {
		part, err := onchain.CheckOnchainPart(ctx, onchain.OnchainCheck{
			Address:   address,
			Sats:      stored.Sats,
			ExpiresAt: stored.ExpiresAt,
			Sale:      sale.Author,
			Clock:     e.seconds,
			HTTP:      e.http,
			Explorer:  m.explorer,
			ReadWitnesses: func(everyRelay bool) (*onchain.OrderWitnesses, error) {
				return onchain.ReadOrderWitnesses(ctx, pool, seen, address, sale.Author, stored.ExpiresAt, m.relays, e.random, everyRelay)
			},
			WitnessAddress: func(txid, of string) string {
				return onchain.WitnessAddress(seen.AddressKey, address, txid, of)
			},
			Publish: func(w onchain.Witness) error {
				return onchain.PublishWitness(ctx, pool, seen, w, m.relays, e.seconds(), e.random)
			},
			Logger:      logger,
			PaidByRests: paidByRests,
			Tolerance:   tolerance,
			ReadMerchantAcceptance: func() (onchain.MerchantAcceptance, error) {
				return onchain.ReadMerchantAcceptance(ctx, pool, accepted, onchain.AcceptedInvoice{Sats: stored.Sats, ExpiresAt: stored.ExpiresAt}, m.relays, e.random)
			},
		})
		return &onchain.OnchainResult{Part: part, Err: err}
	}
	onchainCheck := start(func() *onchain.OnchainResult {
		// A rest invoice that failed counts as not paid: the on-chain part then needs more.
		return checkOnchain(func() int64 {
			r := restCheck.get()
			if r.err != nil {
				return 0
			}
			return onchain.SummaryOf(r.part).Paid
		})
	})
	lightning := lightningCheck.get()
	onchainResult := onchainCheck.get()
	rest := restCheck.get()
	found := read.get()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The sale's records unread: whether a rest invoice was paid cannot be told.
	if rest.err != nil {
		return &parts{lightning: lightning, onchain: onchainResult, rests: &onchain.RestPart{Err: rest.err}}, nil
	}
	rests := rest.part
	now := e.seconds()
	// A part failed: the status is its error, nothing is asked.
	status, _ := statusOf(stored, &parts{lightning: lightning, onchain: onchainResult, rests: rests}, now)
	var lease *onchain.LeaseTerms
	if l := found.records.Leases[address]; l != nil {
		lease = &l.Terms
	}
	// The rest invoices whose rest record a majority took in this check.
	recorded := map[string]bool{}
	if status != nil && status.State == model.Open && lease != nil {
		// Decided again once its turn came, with the clock read then.
		if _, asking := onchain.RestToAsk(status.Rest, stored.Sats, stored.ExpiresAt, now, &onchain.LeaseOfInvoice{Sats: lease.Sats, ExpiresAt: lease.ExpiresAt}); asking {
			asked, err := e.askRest(ctx, stored, status.Rest, *lease, rests.Invoices, pool, m, sale, logger)
			if err != nil {
				return nil, err
			}
			if asked != nil {
				if asked.recorded {
					recorded[asked.invoice.PaymentHash] = true
				}
				rests = &onchain.RestPart{
					Invoices: append(append([]onchain.RestInvoice(nil), rests.Invoices...), asked.invoice),
					Results:  append(append([]onchain.LightningResult(nil), rests.Results...), asked.result),
				}
				// Asked meanwhile by another check and paid already: less is due on-chain.
				if isPaid(asked.result) {
					paid := onchain.SummaryOf(rests).Paid
					onchainResult = checkOnchain(func() int64 { return paid })
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if len(rests.Invoices) > 0 && now < stored.ExpiresAt+proto.GraceSeconds {
		unrecorded, err := e.publishMissingRests(ctx, stored, rests.Invoices, recorded, pool, m, logger)
		if err != nil {
			return nil, err
		}
		rests = &onchain.RestPart{Invoices: rests.Invoices, Results: rests.Results, Unrecorded: unrecorded}
	}
	p := &parts{lightning: lightning, onchain: onchainResult, rests: rests}
	if err := e.publishAcceptance(ctx, stored, p, pool, m, logger); err != nil {
		return nil, err
	}
	return p, nil
}

// publishAcceptance has the pages of the order's checkout links know when the order's tolerance or
// the merchant's acceptance accepted less than its amount (the status p gives is being confirmed,
// or paid on-chain; SPEC.md §31, "Acceptance records"): unless a majority of the merchant's relays
// hold what it received, or less, publishes an acceptance record of it. A failure is logged, never
// the check's: the next check publishes it again. Never the tolerance itself.
func (e *Engine) publishAcceptance(ctx context.Context, stored *Stored, p *parts, pool *relay.Pool, m *merchantChannel, logger logx.Logger) error {
	status, err := statusOf(stored, p, e.seconds())
	if err != nil {
		// A part failed: nothing is accepted.
		return nil
	}
	received, ok := onchain.AcceptanceToPublish(status, stored.Sats, stored.ExpiresAt, e.seconds())
	if !ok {
		return nil
	}
	keys := derive.RestKeys(e.key.Key, stored.Ref, stored.OnchainAddress, stored.ExpiresAt)
	known, err := onchain.AcceptanceKnown(ctx, pool, onchain.ReaderOf(keys), stored.Sats, received, m.relays, e.random, logger)
	if err == nil && known {
		return nil
	}
	if err == nil {
		var event *nostr.Event
		event, err = onchain.AcceptanceRecordEvent(keys, received, stored.ExpiresAt, e.seconds(), e.random)
		if err == nil {
			err = onchain.PublishAcceptanceRecord(ctx, pool, event, keys, m.relays)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !errs.Is(err, errs.Network) {
			return err
		}
		logger.Warn("github.com/sattleme/checkout-go: the acceptance record of a payment was not published (" + err.Error() +
			"); published again at the next check")
	}
	return nil
}

// askedRest is an invoice of the rest a check asked for (or found), its Lightning part, and
// whether a majority took its rest record in this call.
type askedRest struct {
	invoice  onchain.RestInvoice
	result   onchain.LightningResult
	recorded bool
}

// askRest asks the merchant's Lightning address for an invoice of the status's rest, declares it
// with rest (the price and note of the sale's lease of the address) on a majority of the
// merchant's relays, then publishes its rest record (SPEC.md §31, "Asking for a rest invoice").
// One at a time per invoice in this process, each after the rest invoices were read again: another
// check's invoice of the same rest, declared meanwhile, is taken instead. The check decides with
// the clock read once its turn came, the rest invoices read again and the pay request asked, never
// earlier (a check that waited for another one has less time left): the decision, the expiry
// asked, the invoice's checks and its records take that time. nil when none is asked (less than a
// minute left), none could be made (outside the address's sendable sats), or on a failure, logged
// as a warning (the next check asks again).
func (e *Engine) askRest(ctx context.Context, stored *Stored, rest *model.Rest, lease onchain.LeaseTerms, known []onchain.RestInvoice,
	pool *relay.Pool, m *merchantChannel, sale *derive.Sale, logger logx.Logger) (*askedRest, error) {
	unlock, err := e.restAsks.lock(ctx, sale.Ref+"/"+stored.OnchainAddress)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	asked, err := e.askRestInTurn(ctx, stored, rest, lease, known, pool, m, sale, logger)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if _, ours := errs.Of(err); !ours {
			return nil, err
		}
		logger.Warn("github.com/sattleme/checkout-go: no Lightning invoice of the rest of an underpayment (" + err.Error() +
			"); asked again at the next check")
		return nil, nil
	}
	return asked, nil
}

// askRestInTurn is askRest once its turn came, inside its critical section.
func (e *Engine) askRestInTurn(ctx context.Context, stored *Stored, rest *model.Rest, lease onchain.LeaseTerms, known []onchain.RestInvoice,
	pool *relay.Pool, m *merchantChannel, sale *derive.Sale, logger logx.Logger) (*askedRest, error) {
	address := stored.OnchainAddress
	// Another check may have asked for it meanwhile.
	again, err := e.declared(ctx, pool, sale, m, false, e.logger)
	if err != nil {
		return nil, err
	}
	before := map[string]bool{}
	for _, inv := range known {
		before[inv.PaymentHash] = true
	}
	now := e.seconds()
	for _, inv := range again.Rests[sales.RestKeyOf(address, stored.ExpiresAt)] {
		if before[inv.PaymentHash] || inv.Sats != rest.Sats || now >= inv.ExpiresAt {
			continue
		}
		// On a majority, as any rest invoice found (a failure is logged by askRest).
		if event := again.UnsureRests[inv.PaymentHash]; event != nil {
			if err := e.declare(ctx, pool, event, sale, m); err != nil {
				return nil, err
			}
		}
		// Checked as any rest invoice: it may have been paid already.
		return &askedRest{invoice: inv, result: e.verify(ctx, lightningOf(inv))}, nil
	}
	pay, err := lnurl.FetchPayRequest(ctx, e.http, m.lightningAddress)
	if err != nil {
		return nil, err
	}
	// The check decides now: its turn came, the rest invoices read again, the pay request asked.
	now = e.seconds()
	ask, asking := onchain.RestToAsk(rest, stored.Sats, stored.ExpiresAt, now, &onchain.LeaseOfInvoice{Sats: lease.Sats, ExpiresAt: lease.ExpiresAt})
	min, max := lnurl.SendableSats(pay)
	if !asking || !onchain.Sendable(ask.Sats, min, max) {
		return nil, nil
	}
	answer, err := lnurl.RequestInvoice(ctx, e.http, pay, ask.Sats, int(ask.ExpirySeconds), lease.Note)
	if err != nil {
		return nil, err
	}
	decoded, err := lnurl.CheckInvoice(answer, pay, ask.Sats, int(ask.ExpirySeconds), now)
	if err != nil {
		return nil, err
	}
	event, err := declaration.Event(declaration.Input{
		Sale:        sale,
		Invoice:     answer.PR,
		VerifyURL:   answer.Verify,
		PaymentHash: decoded.PaymentHash,
		Price:       lease.Price,
		Note:        lease.Note,
		CreatedAt:   now,
		Rest:        &declaration.RestOf{Address: address, ExpiresAt: stored.ExpiresAt},
		Random:      e.random,
	})
	if err != nil {
		return nil, errs.NewInvalidInvoice("The invoice is too large to be declared", err)
	}
	if err := e.declare(ctx, pool, event, sale, m); err != nil {
		return nil, err
	}
	invoice := onchain.RestInvoice{
		Bolt11:      answer.PR,
		PaymentHash: decoded.PaymentHash,
		VerifyURL:   answer.Verify,
		Sats:        ask.Sats,
		CreatedAt:   decoded.Timestamp,
		ExpiresAt:   decoded.Timestamp + decoded.ExpirySeconds,
	}
	// Declared: it may be shown. Its record, for the checkout links' pages.
	recorded, err := e.publishRest(ctx, stored, invoice, now, pool, m, logger)
	if err != nil {
		return nil, err
	}
	return &askedRest{invoice: invoice, result: onchain.LightningResult{Part: lnurl.Part{State: lnurl.Open}}, recorded: recorded}, nil
}

// publishRest publishes the rest record of invoice, signed at createdAt, on a majority; a failure
// only logged.
func (e *Engine) publishRest(ctx context.Context, stored *Stored, invoice onchain.RestInvoice, createdAt int64, pool *relay.Pool, m *merchantChannel, logger logx.Logger) (bool, error) {
	keys := derive.RestKeys(e.key.Key, stored.Ref, stored.OnchainAddress, stored.ExpiresAt)
	event, err := onchain.RestRecordEvent(keys, invoice, stored.ExpiresAt, createdAt, e.random)
	if err == nil {
		err = onchain.PublishRestRecord(ctx, pool, event, keys, m.relays)
	}
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if !errs.Is(err, errs.Network) {
			return false, err
		}
		logger.Warn("github.com/sattleme/checkout-go: the record of an invoice of the rest was not published (" + err.Error() +
			"); published again at the next check")
		return false, nil
	}
	return true, nil
}

// publishMissingRests publishes again the rest records of invoices that fewer than a majority of
// the relays sent (their first publication may have failed), so that the checkout links' pages see
// each of them. The payment hashes of those still not known on a majority: neither sent by a
// majority nor taken by one in this check (recorded, or published again here); when fewer than a
// majority answered, all of them but recorded. Those are never the status's bolt11: the next
// check tries again.
func (e *Engine) publishMissingRests(ctx context.Context, stored *Stored, invoices []onchain.RestInvoice, recorded map[string]bool,
	pool *relay.Pool, m *merchantChannel, logger logx.Logger) (map[string]bool, error) {
	keys := derive.RestKeys(e.key.Key, stored.Ref, stored.OnchainAddress, stored.ExpiresAt)
	read, err := onchain.ReadRestRecords(ctx, pool, onchain.ReaderOf(keys),
		onchain.RestLinkTerms{Sats: stored.Sats, CreatedAt: -jsonv.MaxSafeInteger, ExpiresAt: stored.ExpiresAt}, m.relays, e.random)
	if err != nil {
		return nil, err
	}
	var unknown []onchain.RestInvoice
	for _, inv := range invoices {
		if !recorded[inv.PaymentHash] {
			unknown = append(unknown, inv)
		}
	}
	unrecorded := map[string]bool{}
	// Not read: whether the page can see the others is not known.
	if !read.Answered {
		for _, inv := range unknown {
			unrecorded[inv.PaymentHash] = true
		}
		return unrecorded, nil
	}
	if len(read.Newer) > 0 {
		logger.Warn("github.com/sattleme/checkout-go: a record of an invoice of the rest has a newer format than this version reads; " +
			"update the package")
	}
	for _, inv := range unknown {
		if read.Held[inv.PaymentHash] {
			continue
		}
		// A record of a newer format at its address: never written over (SPEC.md §12).
		if read.Newer[derive.RestRecordAddress(keys.Key, inv.PaymentHash)] {
			continue
		}
		ok, err := e.publishRest(ctx, stored, inv, e.seconds(), pool, m, logger)
		if err != nil {
			return nil, err
		}
		if !ok {
			unrecorded[inv.PaymentHash] = true
		}
	}
	return unrecorded, nil
}
