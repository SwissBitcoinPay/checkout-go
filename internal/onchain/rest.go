package onchain

import (
	"context"
	"io"
	"sort"
	"strconv"

	"github.com/sattleme/checkout-go/internal/address"
	"github.com/sattleme/checkout-go/internal/bolt11"
	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/model"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/relay"
	"github.com/sattleme/checkout-go/internal/urls"
)

// The rest of an on-chain underpayment (SPEC.md §31, "The rest of an underpayment"): when less
// than the amount came on-chain in time, a payment check asks the merchant's Lightning address for
// an invoice of the rest, declares it for the order's sale with rest (the address), and publishes
// a rest record, which the pages of the order's checkout links read (they cannot read the
// registry). From 1 000 sats, the rest is payable on-chain to the same address too; below, in
// Lightning only.

// RestInvoice is a rest invoice: a declaration of the sale with rest, its invoice decoded.
type RestInvoice struct {
	Bolt11      string
	PaymentHash string
	VerifyURL   string
	Sats        int64
	// CreatedAt is its BOLT11 timestamp, ExpiresAt that and its expiry.
	CreatedAt int64
	ExpiresAt int64
}

// RestInvoiceOf is the rest invoice of a declaration with rest (opened: its invoice decodes, in
// whole sats).
func RestInvoiceOf(d *declaration.Opened) RestInvoice {
	return RestInvoice{
		Bolt11:      d.Invoice,
		PaymentHash: d.Decoded.PaymentHash,
		VerifyURL:   d.VerifyURL,
		Sats:        d.Decoded.AmountMsat / 1000,
		CreatedAt:   d.Decoded.Timestamp,
		ExpiresAt:   d.Decoded.Timestamp + d.Decoded.ExpirySeconds,
	}
}

// RestBefore is the order of the rest invoices: by date, then payment hash.
func RestBefore(a, b RestInvoice) bool {
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt < b.CreatedAt
	}
	return a.PaymentHash < b.PaymentHash
}

// SortRests sorts rest invoices in their order.
func SortRests(rests []RestInvoice) {
	sort.SliceStable(rests, func(i, j int) bool { return RestBefore(rests[i], rests[j]) })
}

// RestOf is the rest of an open status (SPEC.md §31, "The status's rest"): nil unless received is
// above 0 before the invoice's end; else what is left, the latest rest invoice of exactly that
// amount neither paid nor ended (paid: the payment hashes paid), and its QR code: on-chain to the
// address too from 1 000 sats, in Lightning only below.
func RestOf(sats, expiresAt int64, deposit string, received, now int64, rests []RestInvoice, paid map[string]bool) *model.Rest {
	if received <= 0 || received >= sats || now >= expiresAt {
		return nil
	}
	rest := sats - received
	sorted := append([]RestInvoice(nil), rests...)
	SortRests(sorted)
	bolt11 := ""
	for i := len(sorted) - 1; i >= 0; i-- {
		c := sorted[i]
		if c.Sats == rest && !paid[c.PaymentHash] && now < c.ExpiresAt {
			bolt11 = c.Bolt11
			break
		}
	}
	r := &model.Rest{Sats: rest, Bolt11: bolt11}
	switch {
	case rest >= proto.OnchainFloorSats:
		r.PaymentURI = address.PaymentURI(deposit, rest, bolt11)
	case bolt11 != "":
		r.PaymentURI = "lightning:" + bolt11
	}
	return r
}

// RestAsk is what a check asks for: an invoice of Sats, lasting ExpirySeconds.
type RestAsk struct {
	Sats          int64
	ExpirySeconds int64
}

// LeaseOfInvoice is the sats and end of the sale's lease of the address, as a rest is asked with.
type LeaseOfInvoice struct {
	Sats      int64
	ExpiresAt int64
}

// RestToAsk tells whether a check whose status gave rest asks for an invoice of it (SPEC.md §31,
// "Asking for a rest invoice"), and which: none when it has one, with less than 60 s left, or when
// the sale's lease of the address is not the invoice's (lease: its sats and end, nil without one).
// The Lightning address's sendable sats are checked after: its pay request is asked only then.
func RestToAsk(rest *model.Rest, sats, expiresAt, now int64, lease *LeaseOfInvoice) (RestAsk, bool) {
	if rest == nil || rest.Bolt11 != "" {
		return RestAsk{}, false
	}
	if now+proto.MinRestSeconds > expiresAt {
		return RestAsk{}, false
	}
	if lease == nil || lease.Sats != sats || lease.ExpiresAt != expiresAt {
		return RestAsk{}, false
	}
	return RestAsk{Sats: rest.Sats, ExpirySeconds: expiresAt - now}, true
}

// Sendable tells whether sats is within the Lightning address's sendable sats.
func Sendable(sats, min, max int64) bool { return sats >= min && sats <= max }

// RestRecordEvent is the rest record of an invoice (SPEC.md §31, "Rest records"): its invoice and
// verify URL, sealed with the rest encryption key at the rest record address of its payment hash,
// signed by the rest secret at createdAt, dropped by the relays a week after the end of the
// order's invoice (expiresAt), as its checkout links.
func RestRecordEvent(keys derive.Rest, invoice RestInvoice, expiresAt, createdAt int64, random io.Reader) (*nostr.Event, error) {
	return nostr.Seal(nostr.SealOptions{
		Type:          proto.RestRecordType,
		Value:         jsonv.O("version", proto.RestVersion, "invoice", invoice.Bolt11, "verifyUrl", invoice.VerifyURL),
		Signer:        keys.SecretKey,
		EncryptionKey: keys.EncryptionKey,
		Address:       derive.RestRecordAddress(keys.Key, invoice.PaymentHash),
		CreatedAt:     createdAt,
		Tags:          [][]string{{"expiration", strconv.FormatInt(expiresAt+proto.CheckoutLifeSeconds, 10)}},
		Random:        random,
	})
}

// RestLinkTerms is the invoice of the order a rest record belongs to: its amount and dates, as
// its link holds them.
type RestLinkTerms struct {
	Sats      int64
	CreatedAt int64
	ExpiresAt int64
}

// RestReader is what reads an invoice's rest records: its rest key, author and encryption key.
type RestReader struct {
	Key           []byte
	Author        string
	EncryptionKey []byte
}

// ReaderOf is what reads the rest records of the rest keys.
func ReaderOf(keys derive.Rest) RestReader {
	return RestReader{Key: keys.Key, Author: keys.Author, EncryptionKey: keys.EncryptionKey}
}

// OpenRestRecord reads what a relay sent, if it is a rest record of the rest author and rest key
// of a checkout link of an invoice terms describes (SPEC.md §31, "Reading a rest record"): its
// invoice; newer for one of a newer format, which the page leaves aside; false when it is not one.
// An event at the link's acceptance record address (same author, which the read of rest records
// also returns) is no rest record, of a newer format or not.
func OpenRestRecord(v any, keys RestReader, terms RestLinkTerms, memo *nostr.SigMemo) (invoice RestInvoice, event *nostr.Event, newer, ok bool) {
	d := nostr.DTagOf(v)
	if d == "" || d == derive.AcceptanceRecordAddress(keys.Key) {
		return RestInvoice{}, nil, false, false
	}
	opened, ok := nostr.Open(v, nostr.OpenOptions{Type: proto.RestRecordType, EncryptionKey: keys.EncryptionKey, Address: d, Author: keys.Author, Memo: memo},
		func(value any, _ *nostr.Event) (RestInvoice, error) {
			o, ok := jsonv.AsObject(value)
			if !ok {
				return RestInvoice{}, nostr.ErrInvalid
			}
			version, _ := o.Get("version")
			if n, ok := version.(float64); ok && n > proto.RestVersion {
				return RestInvoice{}, nostr.ErrNewer
			}
			invoiceValue, _ := o.Get("invoice")
			invoice, isText := invoiceValue.(string)
			if version != float64(proto.RestVersion) || !isText {
				return RestInvoice{}, nostr.ErrInvalid
			}
			verifyValue, _ := o.Get("verifyUrl")
			verifyURL, isText := verifyValue.(string)
			if _, isURL := urls.ParseHTTPS(verifyURL); !isText || !isURL {
				return RestInvoice{}, nostr.ErrInvalid
			}
			decoded, ok := bolt11.Decode(invoice)
			if !ok || decoded.Network != "bc" || !decoded.HasAmount || decoded.AmountMsat <= 0 || decoded.AmountMsat%1000 != 0 {
				return RestInvoice{}, nostr.ErrInvalid
			}
			sats := decoded.AmountMsat / 1000
			end := decoded.Timestamp + decoded.ExpirySeconds
			if sats >= terms.Sats || decoded.Timestamp < terms.CreatedAt-proto.ClockToleranceSeconds || end > terms.ExpiresAt+proto.ClockToleranceSeconds {
				return RestInvoice{}, nostr.ErrInvalid
			}
			if d != derive.RestRecordAddress(keys.Key, decoded.PaymentHash) {
				return RestInvoice{}, nostr.ErrInvalid
			}
			return RestInvoice{Bolt11: invoice, PaymentHash: decoded.PaymentHash, VerifyURL: verifyURL, Sats: sats, CreatedAt: decoded.Timestamp, ExpiresAt: end}, nil
		})
	if !ok {
		return RestInvoice{}, nil, false, false
	}
	if opened.Newer {
		return RestInvoice{}, opened.Event, true, true
	}
	return opened.Value, opened.Event, false, true
}

// RestRecords is what a read of an invoice's rest records tells.
type RestRecords struct {
	// Held are the payment hashes of the rest records a majority of the relays sent (a page
	// reading a majority meets each of them), as the page would read them.
	Held map[string]bool
	// Newer are the d tags of those of a newer format any relay sent: their invoice is told by
	// the address, which they hold (never written over, SPEC.md §12).
	Newer map[string]bool
	// Answered is false when fewer than a majority answered: nothing else is told then.
	Answered bool
}

// ReadRestRecords reads the rest records of keys on the merchant's relays. Read with a key of this
// read alone, not paged (a rest record per rest invoice: a few).
func ReadRestRecords(ctx context.Context, pool *relay.Pool, keys RestReader, terms RestLinkTerms, relays []string, random io.Reader) (*RestRecords, error) {
	needed := relay.Majority(len(relays))
	authKey := make([]byte, 32)
	if _, err := io.ReadFull(random, authKey); err != nil {
		return nil, err
	}
	memo := nostr.NewSigMemo()
	answers := pool.QueryRelays(relays, relay.Filter{Kinds: []int64{proto.RecordKind}, Authors: []string{keys.Author}}, authKey, relay.QueryOptions{
		Needed: needed,
		Keep: nostr.RecordsThatOpen(func(_, author string) []byte {
			if author == keys.Author {
				return keys.EncryptionKey
			}
			return nil
		}, memo),
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	read := &RestRecords{Held: map[string]bool{}, Newer: map[string]bool{}}
	if !relay.MajorityAnswered(answers, len(relays)) {
		return read, nil
	}
	read.Answered = true
	// How many relays sent a valid record of each payment hash.
	sentBy := map[string]int{}
	for _, a := range answers {
		ofRelay := map[string]bool{}
		for _, v := range a.Events {
			invoice, _, newer, ok := OpenRestRecord(v, keys, terms, memo)
			switch {
			case ok && newer:
				read.Newer[nostr.DTagOf(v)] = true
			case ok:
				ofRelay[invoice.PaymentHash] = true
			}
		}
		for hash := range ofRelay {
			sentBy[hash]++
		}
	}
	for hash, count := range sentBy {
		if count >= needed {
			read.Held[hash] = true
		}
	}
	return read, nil
}

// PublishRestRecord publishes a rest record on the merchant's relays, signed and authenticated
// with the rest secret: nil once a majority took it, else network.
func PublishRestRecord(ctx context.Context, pool *relay.Pool, event *nostr.Event, keys derive.Rest, relays []string) error {
	if err := pool.PublishToQuorum(relays, event, keys.SecretKey, relay.Majority(len(relays))); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errs.NewNetwork("Fewer than a majority of the merchant's relays took the record of an invoice of the rest", err)
	}
	return nil
}
