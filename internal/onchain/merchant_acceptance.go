package onchain

import (
	"context"
	"io"

	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/relay"
)

// The merchant's acceptance (SPEC.md §31, "The merchant's acceptance"): the merchant may accept by
// hand, in its app, what an order underpaid on-chain received as paying it in full. The app
// publishes a record of the team (the integration key opens it but cannot make one) holding what
// the order had received then; a check judges the payment against that amount when it is less
// than the least the order's tolerance accepts (tolerance.DueOnchain). The majority of the
// merchant's relays holds the truth: a check reads it on every relay and counts a record only when
// a majority of the relays sent it. A check never publishes one, nor publishes one again: the
// pages of the order's checkout links learn of it only through an acceptance record
// (acceptance.go).

// MerchantAcceptanceKeys are what a check opens the merchant's acceptance of an invoice with.
type MerchantAcceptanceKeys struct {
	// Team is the team's public key (hex): its only author.
	Team string
	// EncryptionKey is the channel's encryption key: its NIP-44 conversation key.
	EncryptionKey []byte
	// Address is its d tag (derive.MerchantAcceptanceAddress).
	Address string
}

// MerchantAcceptanceKeysOf are the keys of the merchant's acceptance of the invoice of the sale
// saleAuthor with the deposit address and the end expiresAt, for the integration integrationKey of
// team.
func MerchantAcceptanceKeysOf(integrationKey []byte, team, saleAuthor, address string, expiresAt int64) MerchantAcceptanceKeys {
	_, encryptionKey := derive.ChannelKeys(integrationKey)
	return MerchantAcceptanceKeys{
		Team:          team,
		EncryptionKey: encryptionKey,
		Address:       derive.MerchantAcceptanceAddress(integrationKey, saleAuthor, address, expiresAt),
	}
}

// AcceptedInvoice is the invoice a merchant's acceptance is read for: its amount and its end.
type AcceptedInvoice struct {
	Sats      int64
	ExpiresAt int64
}

// OpenedMerchantAcceptance is a merchant's acceptance a relay sent, opened: valid (Received, what
// the order had received when the merchant accepted it), or of a newer format (Newer: logged; the
// newest a majority sent, it leaves the order without one). SetAside is one of a newer format
// dated after the lease's end: set aside as any record dated so, logged all the same.
type OpenedMerchantAcceptance struct {
	Event    *nostr.Event
	Received int64
	Newer    bool
	SetAside bool
}

// OpenMerchantAcceptance reads what a relay sent as the merchant's acceptance of invoice (SPEC.md
// §31, "Reading it"): a record of type integration-onchain-accepted, author the team, sealed with
// the channel's key, at the address of keys, that checks out (its id, its signature); valid when
// its value is an object whose version is 1 and whose received is a safe integer from 1, below
// the invoice's sats (more fields ignored), of a newer format when its version is above 1 or its
// content of a newer NIP-44 version. False for anything else, and for a record dated after the
// lease's end (expiresAt + 120), which is set aside whatever its format: one of a newer format is
// still told, SetAside, for the warning. Its created_at is never compared with now. memo checks
// each copy's signature once (nil: each time).
func OpenMerchantAcceptance(v any, keys MerchantAcceptanceKeys, invoice AcceptedInvoice, memo *nostr.SigMemo) (OpenedMerchantAcceptance, bool) {
	opened, ok := nostr.Open(v, nostr.OpenOptions{Type: proto.MerchantAcceptanceRecordType, EncryptionKey: keys.EncryptionKey, Address: keys.Address, Author: keys.Team, Memo: memo},
		func(value any, _ *nostr.Event) (int64, error) {
			o, ok := jsonv.AsObject(value)
			if !ok {
				return 0, nostr.ErrInvalid
			}
			version, _ := o.Get("version")
			if n, ok := version.(float64); ok && n > proto.MerchantAcceptanceVersion {
				return 0, nostr.ErrNewer
			}
			if version != float64(proto.MerchantAcceptanceVersion) {
				return 0, nostr.ErrInvalid
			}
			receivedValue, _ := o.Get("received")
			received, ok := jsonv.AsSafeInteger(receivedValue)
			if !ok || received < 1 || received >= invoice.Sats {
				return 0, nostr.ErrInvalid
			}
			return received, nil
		})
	if !ok {
		return OpenedMerchantAcceptance{}, false
	}
	// After the lease's end: set aside, of a newer format or not (SPEC.md §31, "Reading it").
	late := opened.Event.CreatedAt > invoice.ExpiresAt+proto.GraceSeconds
	if opened.Newer {
		return OpenedMerchantAcceptance{Event: opened.Event, Newer: true, SetAside: late}, true
	}
	if late {
		return OpenedMerchantAcceptance{}, false
	}
	return OpenedMerchantAcceptance{Event: opened.Event, Received: opened.Value}, true
}

// MerchantAcceptance is the merchant's acceptance of an invoice, as a check counts it.
type MerchantAcceptance struct {
	// Received is the received of the merchant's acceptance that counts; 0 without one.
	Received int64
	// Newer tells whether a relay sent one of a newer format, whatever its date and however many
	// relays sent it: logged.
	Newer bool
}

// MerchantAcceptanceOf is the merchant's acceptance of invoice (SPEC.md §31, "Counting it"), from
// what each relay of the merchant's list (relayCount relays) sent to its read on every relay
// (answers, the events of each relay): every copy opened (OpenMerchantAcceptance), once per id and
// signature; a relay sent a record when it sent a copy of it that opens, and counts once for it
// whatever copies it sent. Of the records valid or of a newer format that a majority of the list
// sent, the newest (the highest created_at, then the lowest id, as relays keep them) decides: a
// valid one is the merchant's acceptance, one of a newer format leaves the order without one. One
// that fewer than a majority sent never counts, whichever relays sent it.
func MerchantAcceptanceOf(answers [][]any, keys MerchantAcceptanceKeys, invoice AcceptedInvoice, relayCount int, memo *nostr.SigMemo) MerchantAcceptance {
	type opening struct {
		opened OpenedMerchantAcceptance
		ok     bool
	}
	// Each copy opened once, by id and signature, whatever relays send it.
	copies := map[string]opening{}
	open := func(v any) (OpenedMerchantAcceptance, bool) {
		id, _ := jsonv.Field(v, "id")
		sig, _ := jsonv.Field(v, "sig")
		idText, ok1 := id.(string)
		sigText, ok2 := sig.(string)
		if !ok1 || !ok2 {
			return OpenMerchantAcceptance(v, keys, invoice, memo)
		}
		copyKey := idText + "/" + sigText
		known, seen := copies[copyKey]
		if !seen {
			known.opened, known.ok = OpenMerchantAcceptance(v, keys, invoice, memo)
			copies[copyKey] = known
		}
		return known.opened, known.ok
	}
	type counted struct {
		opened OpenedMerchantAcceptance
		sentBy int
	}
	// The records that count if enough relays sent them, by id, and how many relays sent each.
	records := map[string]*counted{}
	var order []string
	var accepted MerchantAcceptance
	for _, events := range answers {
		sent := map[string]bool{}
		for _, v := range events {
			opened, ok := open(v)
			if !ok {
				continue
			}
			if opened.Newer {
				accepted.Newer = true
			}
			if opened.SetAside {
				continue
			}
			id := opened.Event.ID
			// One relay counts once for a record, whatever copies of it it sent.
			if sent[id] {
				continue
			}
			sent[id] = true
			if known, ok := records[id]; ok {
				known.sentBy++
			} else {
				records[id] = &counted{opened: opened, sentBy: 1}
				order = append(order, id)
			}
		}
	}
	needed := relay.Majority(relayCount)
	var newest *OpenedMerchantAcceptance
	for _, id := range order {
		r := records[id]
		if r.sentBy < needed {
			continue
		}
		if newest == nil || nostr.Replaces(r.opened.Event.CreatedAt, id, newest.Event.CreatedAt, newest.Event.ID) {
			newest = &r.opened
		}
	}
	if newest != nil && !newest.Newer {
		accepted.Received = newest.Received
	}
	return accepted
}

// ReadMerchantAcceptance reads the merchant's acceptance of invoice (MerchantAcceptanceOf) on
// every relay of the merchant's relays (SPEC.md §13: once every relay answered or failed, or one
// timeout after a majority answered), not paged, by its address and the team, with a key of this
// read alone. network when fewer than a majority answered. It publishes nothing.
func ReadMerchantAcceptance(ctx context.Context, pool *relay.Pool, keys MerchantAcceptanceKeys, invoice AcceptedInvoice, relays []string, random io.Reader) (MerchantAcceptance, error) {
	authKey := make([]byte, 32)
	if _, err := io.ReadFull(random, authKey); err != nil {
		return MerchantAcceptance{}, err
	}
	memo := nostr.NewSigMemo()
	answers := pool.QueryRelays(relays, relay.Filter{Kinds: []int64{proto.RecordKind}, Authors: []string{keys.Team}, D: []string{keys.Address}}, authKey, relay.QueryOptions{
		Needed:     relay.Majority(len(relays)),
		EveryRelay: true,
		Keep: nostr.RecordsThatOpen(func(d, author string) []byte {
			if author == keys.Team && d == keys.Address {
				return keys.EncryptionKey
			}
			return nil
		}, memo),
	})
	if err := ctx.Err(); err != nil {
		return MerchantAcceptance{}, err
	}
	if !relay.MajorityAnswered(answers, len(relays)) {
		return MerchantAcceptance{}, errs.NewNetwork("Fewer than a majority of the merchant's relays answered", nil)
	}
	events := make([][]any, len(answers))
	for i, a := range answers {
		events[i] = a.Events
	}
	return MerchantAcceptanceOf(events, keys, invoice, len(relays), memo), nil
}
