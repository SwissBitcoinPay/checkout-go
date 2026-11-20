// Package sales reads back the invoices declared for a sale from the merchant's registry (SPEC.md
// §21, "One order, one invoice at a time"): its declarations and its leases of deposit addresses.
// What createInvoice checks before a new invoice, and findInvoices gives.
package sales

import (
	"context"
	"math/big"
	"sort"
	"strconv"

	"github.com/sattleme/checkout-go/internal/address"
	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/logx"
	"github.com/sattleme/checkout-go/internal/model"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/onchain"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/rates"
	"github.com/sattleme/checkout-go/internal/relay"
	"github.com/sattleme/checkout-go/internal/text"
)

// Declaration is a declared invoice, with its signed declaration (nil for an order paid on-chain
// only) and lease (nil without an address): published again when the invoice may still be paid.
type Declaration struct {
	Invoice *model.DeclaredInvoice
	Event   *nostr.Event
	Lease   *nostr.Event
}

// Records is what the registry holds of a sale.
type Records struct {
	// Invoices are the order's invoices, oldest first.
	Invoices []Declaration
	// Rests are the rest invoices of each invoice with an address (SPEC.md §31, "The rest of an
	// underpayment"), by RestKeyOf of that invoice (its address and end): its declarations with
	// rest, by date then payment hash.
	Rests map[string][]onchain.RestInvoice
	// UnsureRests are the declarations of rest invoices fewer than a majority of the merchant's
	// relays sent, by payment hash: their publication may have reached a minority only (it
	// failed), and a later read may miss them. Published again on a majority before they are used.
	UnsureRests map[string]*nostr.Event
	// Leases are the sale's leases still held (not withdrawn), by address: the order's terms.
	Leases map[string]*onchain.OpenedLease
	// SentBy is how many relays sent each event, by id: of a read on every relay, which relays
	// hold it.
	SentBy map[string]int
}

// Read reads the invoices of sale on relays, oldest first (by date, then payment hash, then
// address): page after page as soon as a majority of the relays answered (each declaration and
// lease is on a majority of them); on every relay with everyRelay (SPEC.md §13): which relays
// hold a record. One per payment hash, with the address of the sale's confirmed lease naming its
// invoice; and one per confirmed lease of an order paid on-chain only (InvoicesOf). A lease naming
// an invoice without a declaration is left out: that invoice was never shown. A declaration with
// rest is no invoice of the order: one of the rest invoices of the invoice it names. network
// when fewer than a majority answered.
func Read(ctx context.Context, pool *relay.Pool, sale *derive.Sale, relays []string, logger logx.Logger, everyRelay bool) (*Records, error) {
	needed := relay.Majority(len(relays))
	memo := nostr.NewSigMemo()
	answers := pool.QueryRelays(relays, relay.Filter{Kinds: []int64{proto.RecordKind}, Authors: []string{sale.Author}}, sale.SecretKey, relay.QueryOptions{
		Needed:     needed,
		Paged:      true,
		EveryRelay: everyRelay,
		// Its declarations and its records at registry addresses: sealed with the outer key of
		// their address and of the sale.
		Keep: nostr.RecordsThatOpen(func(d, author string) []byte {
			if author != sale.Author || d == "" {
				return nil
			}
			return derive.OuterKey(sale.Registry.Key, d, author)
		}, memo),
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !relay.MajorityAnswered(answers, len(relays)) {
		return nil, errs.NewNetwork("Fewer than a majority of the merchant's relays answered", nil)
	}
	declarations := map[string]*declaration.Opened{}
	var hashes []string
	// The sale's records at registry addresses of addresses.
	var records []onchain.SaleLeaseRecord
	// Event ids that checked out: another copy of one is the same event, not opened again.
	checked := map[string]bool{}
	sentBy := map[string]int{}
	newer := false
	for _, answer := range answers {
		// A relay counts for an event when it sent a copy whose id is its hash: it holds that
		// event.
		ids := map[string]bool{}
		for _, v := range answer.Events {
			if e, ok := nostr.WithValidID(v); ok {
				ids[e.ID] = true
			}
		}
		for id := range ids {
			sentBy[id]++
		}
		for _, v := range answer.Events {
			if id, ok := jsonv.Field(v, "id"); ok {
				if s, isText := id.(string); isText && checked[s] {
					continue
				}
			}
			opened, newerDeclaration, ok := declaration.Open(v, sale, memo)
			if newerDeclaration {
				newer = true
				// Authentic (its signature checked before its format), at a registry address: the
				// newest there, it leaves no lease of the sale.
				if d := nostr.DTagOf(v); d != "" {
					if e, valid := nostr.WithValidID(v); valid {
						checked[e.ID] = true
						records = append(records, onchain.SaleLeaseRecord{Kind: onchain.SaleNewer, Event: e, D: d})
					}
				}
				continue
			}
			if ok {
				checked[opened.Event.ID] = true
				hash := opened.Decoded.PaymentHash
				if _, known := declarations[hash]; !known {
					declarations[hash] = opened
					hashes = append(hashes, hash)
				}
				continue
			}
			record, ok := onchain.OpenSaleLease(v, sale, memo)
			if !ok {
				continue
			}
			if record.Kind == onchain.SaleNewer {
				newer = true
			}
			checked[record.Event.ID] = true
			records = append(records, record)
		}
	}
	if newer {
		logger.Warn("github.com/sattleme/checkout-go: a declaration or a lease of this order has a newer format " +
			"than this version reads; update the package")
	}
	newest := NewestRecords(records)
	rests := map[string][]onchain.RestInvoice{}
	unsureRests := map[string]*nostr.Event{}
	var invoices []*declaration.Opened
	for _, hash := range hashes {
		d := declarations[hash]
		if d.Rest == nil {
			invoices = append(invoices, d)
			continue
		}
		if sentBy[d.Event.ID] < needed {
			unsureRests[hash] = d.Event
		}
		key := RestKeyOf(d.Rest.Address, d.Rest.ExpiresAt)
		rests[key] = append(rests[key], onchain.RestInvoiceOf(d))
	}
	for _, list := range rests {
		onchain.SortRests(list)
	}
	leases := map[string]*onchain.OpenedLease{}
	for _, record := range newest {
		if record.Kind == onchain.SaleLease {
			leases[record.Lease.Terms.Address] = record.Lease
		}
	}
	return &Records{
		Invoices:    InvoicesOf(sale.Ref, invoices, newest),
		Rests:       rests,
		UnsureRests: unsureRests,
		Leases:      leases,
		SentBy:      sentBy,
	}, nil
}

// NewestRecords is, of the sale's records at registry addresses, the newest at each (the highest
// created_at, then the lowest id, as relays keep them): its lease there, or its withdrawal, which
// leaves none; in the order their addresses first appear.
func NewestRecords(records []onchain.SaleLeaseRecord) []onchain.SaleLeaseRecord {
	newest := map[string]int{}
	var out []onchain.SaleLeaseRecord
	for _, record := range records {
		i, known := newest[record.D]
		if !known {
			newest[record.D] = len(out)
			out = append(out, record)
			continue
		}
		e, k := record.Event, out[i].Event
		if nostr.Replaces(e.CreatedAt, e.ID, k.CreatedAt, k.ID) {
			out[i] = record
		}
	}
	return out
}

// InvoicesOf is the sale's invoices, oldest first (by date, then payment hash, none first, then
// address), from its declarations (one per payment hash; one with rest gives none) and its newest
// record at each registry address (NewestRecords). Only a confirmed lease gives an address
// (SPEC.md §31, "Leasing an address"): one that is not was left by a createInvoice that failed
// before the address was the order's. One invoice per declaration, with the address of the newest
// confirmed lease naming its invoice; and one per confirmed lease of an order paid on-chain only.
// A lease naming an invoice without a declaration is left out: that invoice was never shown.
func InvoicesOf(ref string, declarations []*declaration.Opened, newest []onchain.SaleLeaseRecord) []Declaration {
	var confirmed []*onchain.OpenedLease
	for _, record := range newest {
		if record.Kind == onchain.SaleLease && onchain.IsConfirmedLease(record.Lease.Terms) {
			confirmed = append(confirmed, record.Lease)
		}
	}
	var found []Declaration
	for _, d := range declarations {
		// A rest invoice's: no invoice of the order.
		if d.Rest != nil {
			continue
		}
		invoice := text.LowerASCII(d.Invoice)
		var lease *onchain.OpenedLease
		for _, l := range confirmed {
			if l.Terms.Invoice == "" || text.LowerASCII(l.Terms.Invoice) != invoice {
				continue
			}
			if lease == nil || nostr.Replaces(l.Event.CreatedAt, l.Event.ID, lease.Event.CreatedAt, lease.Event.ID) {
				lease = l
			}
		}
		sats := d.Decoded.AmountMsat / 1000
		var onchainAddress string
		var leaseEvent *nostr.Event
		if lease != nil {
			onchainAddress, leaseEvent = lease.Terms.Address, lease.Event
		}
		found = append(found, Declaration{
			Invoice: &model.DeclaredInvoice{
				Ref:            ref,
				Bolt11:         d.Invoice,
				PaymentHash:    d.Decoded.PaymentHash,
				VerifyURL:      d.VerifyURL,
				OnchainAddress: onchainAddress,
				PaymentURI:     address.PaymentURI(onchainAddress, sats, d.Invoice),
				Sats:           sats,
				Fiat:           FiatOf(d.Price),
				Note:           d.Note,
				CreatedAt:      d.Decoded.Timestamp,
				ExpiresAt:      d.Decoded.Timestamp + d.Decoded.ExpirySeconds,
				DeclaredAt:     d.DeclaredAt,
			},
			Event: d.Event,
			Lease: leaseEvent,
		})
	}
	for _, lease := range confirmed {
		if lease.Terms.Invoice != "" {
			continue
		}
		terms := lease.Terms
		found = append(found, Declaration{
			Invoice: &model.DeclaredInvoice{
				Ref:            ref,
				OnchainAddress: terms.Address,
				PaymentURI:     address.PaymentURI(terms.Address, terms.Sats, ""),
				Sats:           terms.Sats,
				Fiat:           FiatOf(terms.Price),
				Note:           terms.Note,
				CreatedAt:      terms.CreatedAt,
				ExpiresAt:      terms.ExpiresAt,
				// Its mark's date (SPEC.md §31, "Leasing an address").
				DeclaredAt: lease.Event.CreatedAt,
			},
			Lease: lease.Event,
		})
	}
	sort.SliceStable(found, func(i, j int) bool {
		a, b := found[i].Invoice, found[j].Invoice
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt < b.CreatedAt
		}
		if a.PaymentHash != b.PaymentHash {
			return a.PaymentHash < b.PaymentHash
		}
		return a.OnchainAddress < b.OnchainAddress
	})
	return found
}

// MayStillBePaid is step 1 of SPEC.md §21: the indices of the invoices (oldest first) that may
// still be paid at now, until their end and its grace period.
func MayStillBePaid(invoices []*model.DeclaredInvoice, now int64) []int {
	indices := []int{}
	for i, invoice := range invoices {
		if now < invoice.ExpiresAt+proto.GraceSeconds {
			indices = append(indices, i)
		}
	}
	return indices
}

// LastRecordOf is an invoice's last record: its declaration, or for an invoice paid on-chain only
// its lease's event (its mark). The one shown has it on a majority of the relays; one whose
// createInvoice failed before that, on a minority.
func LastRecordOf(d Declaration) *nostr.Event {
	if d.Event != nil {
		return d.Event
	}
	return d.Lease
}

// Held tells, of invoices (those that may still be paid), which ones a majority of n relays hold,
// by read, the sale's records read on every relay (SPEC.md §21): whether a majority of them sent
// its last record (LastRecordOf), as PendingOutcome takes it.
func Held(read *Records, invoices []Declaration, n int) []bool {
	needed := relay.Majority(n)
	held := make([]bool, len(invoices))
	for i, d := range invoices {
		held[i] = read.SentBy[LastRecordOf(d).ID] >= needed
	}
	return held
}

// Checked is the payment check of an invoice that may still be paid: its status, or its error.
type Checked struct {
	Status *model.PaymentStatus
	Err    error
}

// The outcomes of the invoices that may still be paid.
const (
	OutcomePaid    = "paid"
	OutcomeFailed  = "failed"
	OutcomeUnread  = "unread"
	OutcomeHandOut = "handOut"
)

// Outcome is what is done of the invoices that may still be paid: Index is the invoice paid, the
// one whose check failed, or the one handed out.
type Outcome struct {
	Kind  string
	Index int
}

// PendingOutcome is, of the invoices that may still be paid (oldest first, one or more), from the
// payment check of each and, of several, whether a majority of the relays sent its last record
// (held, read on every relay; nil when fewer than a majority answered that read): the first paid
// gives order_paid, whatever the read (a payment proves itself); else the first error is the error
// (that invoice may have been paid); else, when the read was not answered by a majority, it is
// unread (network). Else the one handed out is the only one; of several, the only one held (the one
// shown), else the newest that received something (unconfirmed, or open with received above 0),
// else the newest.
func PendingOutcome(statuses []Checked, held []bool) Outcome {
	for i, s := range statuses {
		if s.Err == nil && s.Status.State == model.Paid {
			return Outcome{Kind: OutcomePaid, Index: i}
		}
	}
	for i, s := range statuses {
		if s.Err != nil {
			return Outcome{Kind: OutcomeFailed, Index: i}
		}
	}
	if len(statuses) == 1 {
		return Outcome{Kind: OutcomeHandOut, Index: 0}
	}
	if held == nil {
		return Outcome{Kind: OutcomeUnread}
	}
	var shown []int
	for i, isHeld := range held {
		if isHeld {
			shown = append(shown, i)
		}
	}
	if len(shown) == 1 {
		return Outcome{Kind: OutcomeHandOut, Index: shown[0]}
	}
	for i := len(statuses) - 1; i >= 0; i-- {
		s := statuses[i].Status
		if s.State == model.Unconfirmed || s.State == model.Open && s.Received > 0 {
			return Outcome{Kind: OutcomeHandOut, Index: i}
		}
	}
	return Outcome{Kind: OutcomeHandOut, Index: len(statuses) - 1}
}

// RestKeyOf is the key, in Records.Rests, of the invoice with address and the end expiresAt.
func RestKeyOf(address string, expiresAt int64) string {
	return address + "/" + strconv.FormatInt(expiresAt, 10)
}

// FiatOf is the price of a declared sale as a Fiat: nil for a sale in sats.
func FiatOf(price declaration.Price) *model.Fiat {
	if price.Currency == proto.SatsCurrency {
		return nil
	}
	return &model.Fiat{
		Amount:   rates.Decimal{Units: big.NewInt(price.Minor), Scale: price.Digits}.Format(),
		Currency: price.Currency,
		Minor:    price.Minor,
		Digits:   price.Digits,
	}
}
