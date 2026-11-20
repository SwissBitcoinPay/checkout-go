package onchain

import (
	"context"
	"io"

	"github.com/sattleme/checkout-go/internal/address"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/key"
	"github.com/sattleme/checkout-go/internal/logx"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/relay"
)

// ParseLot reads a lot's value (SPEC.md §31, "The lot"): version 1, updatedAt its event's
// created_at, 0 to 100 deposit addresses, none twice. nostr.ErrNewer for a newer version,
// nostr.ErrInvalid when it is not valid.
func ParseLot(value any, createdAt int64) ([]string, error) {
	v, ok := jsonv.AsObject(value)
	if !ok {
		return nil, nostr.ErrInvalid
	}
	version, _ := v.Get("version")
	if n, ok := version.(float64); ok && n > proto.LotVersion {
		return nil, nostr.ErrNewer
	}
	updatedAt, _ := v.Get("updatedAt")
	if version != float64(proto.LotVersion) || updatedAt != float64(createdAt) {
		return nil, nostr.ErrInvalid
	}
	addressesValue, _ := v.Get("addresses")
	addresses, ok := addressesValue.([]any)
	if !ok || len(addresses) > proto.MaxLotAddresses {
		return nil, nostr.ErrInvalid
	}
	lot := make([]string, 0, len(addresses))
	seen := map[string]bool{}
	for _, a := range addresses {
		if !address.IsDeposit(a) || seen[a.(string)] {
			return nil, nostr.ErrInvalid
		}
		seen[a.(string)] = true
		lot = append(lot, a.(string))
	}
	return lot, nil
}

// ReadLot reads the addresses of the integration's lot on the merchant's relays, as soon as a
// majority answered: the newest valid copy. Empty when there is none, or when a copy of a newer
// format is at least as new (logged: update the package). The order means nothing. network when
// fewer than a majority answered.
func ReadLot(ctx context.Context, pool *relay.Pool, k *key.IntegrationKey, relays []string, random io.Reader, logger logx.Logger) ([]string, error) {
	lotAddress := derive.LotAddress(k.Key)
	_, encryptionKey := derive.ChannelKeys(k.Key)
	needed := relay.Majority(len(relays))
	// A relay that asks to authenticate (NIP-42) gets a key of this read alone.
	authKey := make([]byte, 32)
	if _, err := io.ReadFull(random, authKey); err != nil {
		return nil, err
	}
	memo := nostr.NewSigMemo()
	answers := pool.QueryRelays(relays, relay.Filter{Kinds: []int64{proto.RecordKind}, Authors: []string{k.TeamPublicKey}, D: []string{lotAddress}}, authKey, relay.QueryOptions{
		Needed: needed,
		Keep: nostr.RecordsThatOpen(func(_, author string) []byte {
			if author == k.TeamPublicKey {
				return encryptionKey
			}
			return nil
		}, memo),
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !relay.MajorityAnswered(answers, len(relays)) {
		return nil, errs.NewNetwork("Fewer than a majority of the merchant's relays answered", nil)
	}
	newest := nostr.NewestRecord(eventsOf(answers), func(v any) (nostr.Opened[[]string], bool) {
		return nostr.Open(v, nostr.OpenOptions{Type: proto.LotRecordType, EncryptionKey: encryptionKey, Address: lotAddress, Author: k.TeamPublicKey, Memo: memo},
			func(value any, e *nostr.Event) ([]string, error) { return ParseLot(value, e.CreatedAt) })
	})
	newestAt := int64(-1)
	if newest.Event != nil {
		newestAt = newest.Event.CreatedAt
	}
	if newest.HasUnreadable && newest.NewestUnreadable >= newestAt {
		logger.Warn("github.com/sattleme/checkout-go: the integration's lot of addresses has a newer format than this version reads; update the package (no on-chain payment until then)")
		return nil, nil
	}
	if newest.Event == nil {
		return nil, nil
	}
	return newest.Value, nil
}

// eventsOf is the events of each relay's answer.
func eventsOf(answers []relay.Answer) [][]any {
	events := make([][]any, len(answers))
	for i, a := range answers {
		events[i] = a.Events
	}
	return events
}
