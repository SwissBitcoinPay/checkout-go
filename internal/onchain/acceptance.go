package onchain

import (
	"context"
	"io"
	"strconv"

	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/logx"
	"github.com/sattleme/checkout-go/internal/model"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/relay"
)

// Acceptance records (SPEC.md §31, "Acceptance records"): the page of a checkout link cannot know
// the merchant's tolerance nor read the merchant's acceptance (merchant_acceptance.go), so an order
// either accepted would read underpaid there, its customer asked for the rest and never sent back
// to the shop. A check whose status is paid (or being confirmed) with less than the amount
// publishes what it received then, signed by the rest secret, for the pages of the order's
// checkout links: they count the payment as enough from that on. Never the tolerance itself.

// AcceptanceToPublish is what a check whose status is status must have the pages of the order's
// checkout links know (SPEC.md §31, "Publishing it"): the status's received, when the order's
// tolerance or the merchant's acceptance accepted less than its amount (unconfirmed, or paid with
// transactions) before the links' life ends; false otherwise.
func AcceptanceToPublish(status *model.PaymentStatus, sats, expiresAt, now int64) (int64, bool) {
	if now >= expiresAt+proto.CheckoutLifeSeconds {
		return 0, false
	}
	accepted := status.State == model.Unconfirmed ||
		status.State == model.Paid && status.Method != model.Lightning && len(status.Txids) > 0
	if !accepted || status.Received >= sats {
		return 0, false
	}
	return status.Received, true
}

// AcceptanceRecordEvent is the acceptance record of the invoice of keys (its rest keys):
// received, sealed with the rest encryption key at its acceptance record address, signed by the
// rest secret at createdAt, dropped by the relays a week after the invoice's end (expiresAt), as
// its checkout links.
func AcceptanceRecordEvent(keys derive.Rest, received, expiresAt, createdAt int64, random io.Reader) (*nostr.Event, error) {
	return nostr.Seal(nostr.SealOptions{
		Type:          proto.AcceptanceRecordType,
		Value:         jsonv.O("version", proto.AcceptanceVersion, "received", received),
		Signer:        keys.SecretKey,
		EncryptionKey: keys.EncryptionKey,
		Address:       derive.AcceptanceRecordAddress(keys.Key),
		CreatedAt:     createdAt,
		Tags:          [][]string{{"expiration", strconv.FormatInt(expiresAt+proto.CheckoutLifeSeconds, 10)}},
		Random:        random,
	})
}

// openAcceptance opens v as an acceptance record of keys for an invoice of sats.
func openAcceptance(v any, keys RestReader, sats int64, memo *nostr.SigMemo) (nostr.Opened[int64], bool) {
	addr := derive.AcceptanceRecordAddress(keys.Key)
	if nostr.DTagOf(v) != addr {
		return nostr.Opened[int64]{}, false
	}
	return nostr.Open(v, nostr.OpenOptions{Type: proto.AcceptanceRecordType, EncryptionKey: keys.EncryptionKey, Address: addr, Author: keys.Author, Memo: memo},
		func(value any, _ *nostr.Event) (int64, error) {
			o, ok := jsonv.AsObject(value)
			if !ok {
				return 0, nostr.ErrInvalid
			}
			version, _ := o.Get("version")
			if n, ok := version.(float64); ok && n > proto.AcceptanceVersion {
				return 0, nostr.ErrNewer
			}
			if version != float64(proto.AcceptanceVersion) {
				return 0, nostr.ErrInvalid
			}
			receivedValue, _ := o.Get("received")
			received, ok := jsonv.AsSafeInteger(receivedValue)
			if !ok || received < 1 || received >= sats {
				return 0, nostr.ErrInvalid
			}
			return received, nil
		})
}

// OpenAcceptanceRecord reads what a relay sent, if it is an acceptance record of the rest author
// and rest key of a checkout link of an invoice of sats (SPEC.md §31, "Reading an acceptance
// record"): its received, from 1, below sats; newer for one of a newer format, left aside; false
// when it is not one.
func OpenAcceptanceRecord(v any, keys RestReader, sats int64) (received int64, event *nostr.Event, newer, ok bool) {
	opened, ok := openAcceptance(v, keys, sats, nil)
	if !ok {
		return 0, nil, false, false
	}
	return opened.Value, opened.Event, opened.Newer, true
}

// AcceptanceKnown tells whether the pages already know received (SPEC.md §31, "Publishing it"): a
// majority of the merchant's relays each sent a valid acceptance record of the invoice of keys (of
// sats) whose received, the newest it sent, is at most received. Read with a key of this read
// alone, not paged (one record per invoice). True when a relay sent one of a newer format, never
// written over (SPEC.md §12), and logged. network when fewer than a majority answered: what the
// others hold, of a newer format perhaps, cannot be told, so nothing is published over it, and
// the next check reads again (SPEC.md §13: such a read decides nothing).
func AcceptanceKnown(ctx context.Context, pool *relay.Pool, keys RestReader, sats, received int64, relays []string, random io.Reader, logger logx.Logger) (bool, error) {
	needed := relay.Majority(len(relays))
	authKey := make([]byte, 32)
	if _, err := io.ReadFull(random, authKey); err != nil {
		return false, err
	}
	memo := nostr.NewSigMemo()
	answers := pool.QueryRelays(relays, relay.Filter{Kinds: []int64{proto.RecordKind}, Authors: []string{keys.Author}, D: []string{derive.AcceptanceRecordAddress(keys.Key)}}, authKey, relay.QueryOptions{
		Needed: needed,
		Keep: nostr.RecordsThatOpen(func(_, author string) []byte {
			if author == keys.Author {
				return keys.EncryptionKey
			}
			return nil
		}, memo),
	})
	if err := ctx.Err(); err != nil {
		return false, err
	}
	holders := 0
	for _, a := range answers {
		newest := nostr.NewestRecord([][]any{a.Events}, func(v any) (nostr.Opened[int64], bool) { return openAcceptance(v, keys, sats, memo) })
		if newest.HasUnreadable {
			logger.Warn("github.com/sattleme/checkout-go: an acceptance record of this order has a newer format than this version reads; " +
				"update the package")
			return true, nil
		}
		if newest.Event != nil && newest.Value <= received {
			holders++
		}
	}
	if !relay.MajorityAnswered(answers, len(relays)) {
		return false, errs.NewNetwork("Fewer than a majority of the merchant's relays answered", nil)
	}
	return holders >= needed, nil
}

// PublishAcceptanceRecord publishes an acceptance record on the merchant's relays, signed and
// authenticated with the rest secret: nil once a majority took it, else network.
func PublishAcceptanceRecord(ctx context.Context, pool *relay.Pool, event *nostr.Event, keys derive.Rest, relays []string) error {
	if err := pool.PublishToQuorum(relays, event, keys.SecretKey, relay.Majority(len(relays))); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errs.NewNetwork("Fewer than a majority of the merchant's relays took the acceptance record of a payment", err)
	}
	return nil
}
