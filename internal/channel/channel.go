// Package channel reads the integration's channel (SPEC.md §14): one record signed by the team,
// which the merchant's app publishes on the account's relays and which only the integration's key
// opens. It gives the merchant's Lightning address, registry and relays, or says that the
// integration was removed.
package channel

import (
	"context"
	"io"

	"github.com/sattleme/checkout-go/internal/bitcoinnode"
	"github.com/sattleme/checkout-go/internal/checkoutlink"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/key"
	"github.com/sattleme/checkout-go/internal/lnurl"
	"github.com/sattleme/checkout-go/internal/logx"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/relay"
	"github.com/sattleme/checkout-go/internal/secp"
	"github.com/sattleme/checkout-go/internal/text"
	"github.com/sattleme/checkout-go/internal/tolerance"
	"github.com/sattleme/checkout-go/internal/urls"
)

// Channel is the channel, as far as the server uses it.
type Channel struct {
	// Removed is a removed integration: nothing else is set but UpdatedAt.
	Removed      bool
	BusinessName string
	// LightningAddress is never shown to the integrator by the app: the package does not return
	// it either.
	LightningAddress string
	// RegistryKey is hex, 32 bytes.
	RegistryKey string
	// RegistryReader is hex: the public half of the merchant's reading key.
	RegistryReader string
	// Relays are the account's relays, when the channel names them (nil otherwise).
	Relays []string
	// CheckoutOrigin is the merchant's web app for checkout links (checkout.origin), "" when the
	// merchant's app gives none or an invalid one.
	CheckoutOrigin string
	// Tolerance is the merchant's tolerance of on-chain underpayments (onchainTolerance, basis
	// points), 0 when the channel gives none or an invalid one. An order's lease keeps it; never
	// returned to the integrator.
	Tolerance int64
	// BitcoinNode is the merchant's Bitcoin node (bitcoinNode, SPEC.md §31), the explorer the
	// server reads; nil when the channel names none or an invalid one (mempool.space is read).
	BitcoinNode *bitcoinnode.Node
	// UpdatedAt is the record's created_at.
	UpdatedAt int64
}

// Parse reads a channel's value, as the merchant's app reads it, for what the server uses
// (SPEC.md §14, "A valid channel value"). The other fields are the app's own and not read here, so
// that a change to them never stops a shop's server. nostr.ErrNewer for a newer version,
// nostr.ErrInvalid when it is not valid.
func Parse(value any, createdAt int64) (*Channel, error) {
	v, ok := jsonv.AsObject(value)
	if !ok {
		return nil, nostr.ErrInvalid
	}
	version, _ := v.Get("version")
	if n, ok := version.(float64); ok && n > proto.ChannelVersion {
		return nil, nostr.ErrNewer
	}
	updatedAt, _ := v.Get("updatedAt")
	if version != float64(proto.ChannelVersion) || updatedAt != float64(createdAt) {
		return nil, nostr.ErrInvalid
	}
	status, _ := v.Get("status")
	if status == "removed" {
		return &Channel{Removed: true, UpdatedAt: createdAt}, nil
	}
	role, _ := v.Get("role")
	if status != "active" || role != "integration" {
		return nil, nostr.ErrInvalid
	}
	profile, ok := jsonv.AsObject(func() any { p, _ := v.Get("profile"); return p }())
	if !ok {
		return nil, nostr.ErrInvalid
	}
	businessNameValue, _ := profile.Get("businessName")
	businessName, ok := businessNameValue.(string)
	if !ok || !text.IsWellFormed(businessName) || text.TrimASCII(businessName) == "" || text.CodePoints(businessName) > proto.MaxBusinessNameLength {
		return nil, nostr.ErrInvalid
	}
	addressValue, _ := profile.Get("lightningAddress")
	lightningAddress, ok := addressValue.(string)
	if !ok {
		return nil, nostr.ErrInvalid
	}
	if _, ok := lnurl.AddressURL(lightningAddress); !ok {
		return nil, nostr.ErrInvalid
	}
	registryKey, _ := v.Get("registryKey")
	registryReader, _ := v.Get("registryReader")
	// The reading key must be a point of the curve: every declaration's inside is sealed to it.
	if !text.IsHex32Value(registryKey) || !secp.IsPublicKey(registryReader) {
		return nil, nostr.ErrInvalid
	}
	c := &Channel{
		BusinessName:     businessName,
		LightningAddress: lightningAddress,
		RegistryKey:      registryKey.(string),
		RegistryReader:   registryReader.(string),
		UpdatedAt:        createdAt,
	}
	if relaysValue, has := v.Get("relays"); has {
		relays, ok := urls.ParseRelayList(relaysValue)
		if !ok {
			return nil, nostr.ErrInvalid
		}
		c.Relays = relays
	}
	checkout, _ := v.Get("checkout")
	c.CheckoutOrigin = checkoutlink.OriginOf(checkout)
	// Absent or invalid, none: the channel stays valid (SPEC.md §14).
	onchainTolerance, _ := v.Get("onchainTolerance")
	c.Tolerance = tolerance.Of(onchainTolerance)
	// Absent or invalid in any way, none: the channel stays valid (SPEC.md §14). It is no new
	// version of the channel: a reader that does not know it ignores it.
	bitcoinNode, _ := v.Get("bitcoinNode")
	c.BitcoinNode = bitcoinnode.Of(bitcoinNode)
	return c, nil
}

// Lookup is what the relays of one list hold.
type Lookup struct {
	// Found is the newest valid channel, nil when there is none.
	Found *Channel
	// Unreachable is fewer than a majority of the relays answered.
	Unreachable bool
	// Newer is no valid channel, but one of a newer format.
	Newer bool
}

// Options is what a read of the channel uses.
type Options struct {
	Random io.Reader
	Logger logx.Logger
}

// LookupOn reads the newest copy of the integration's channel on relays, signed by its team, over
// the connections of the pool: every relay at once, read as soon as a majority of them answered
// (the app publishes the channel, and its removal, on a majority of them: any two majorities share
// a relay). The call's cancellation is its context's error.
func LookupOn(ctx context.Context, pool *relay.Pool, k *key.IntegrationKey, relays []string, o Options) (Lookup, error) {
	address, encryptionKey := derive.ChannelKeys(k.Key)
	// A relay that asks to authenticate (NIP-42) gets a key of this read alone.
	authKey := make([]byte, 32)
	if _, err := io.ReadFull(o.Random, authKey); err != nil {
		return Lookup{}, err
	}
	memo := nostr.NewSigMemo()
	answers := pool.QueryRelays(relays, relay.Filter{Kinds: []int64{proto.RecordKind}, Authors: []string{k.TeamPublicKey}, D: []string{address}}, authKey, relay.QueryOptions{
		Needed: relay.Majority(len(relays)),
		Keep: relay.Keep(nostr.RecordsThatOpen(func(_, author string) []byte {
			if author == k.TeamPublicKey {
				return encryptionKey
			}
			return nil
		}, memo)),
	})
	if err := ctx.Err(); err != nil {
		return Lookup{}, err
	}
	if !relay.MajorityAnswered(answers, len(relays)) {
		return Lookup{Unreachable: true}, nil
	}
	events := make([][]any, len(answers))
	for i, a := range answers {
		events[i] = a.Events
	}
	newest := nostr.NewestRecord(events, func(v any) (nostr.Opened[*Channel], bool) {
		return nostr.Open(v, nostr.OpenOptions{Type: proto.ChannelRecordType, EncryptionKey: encryptionKey, Address: address, Author: k.TeamPublicKey, Memo: memo},
			func(value any, e *nostr.Event) (*Channel, error) { return Parse(value, e.CreatedAt) })
	})
	newestAt := int64(-1)
	if newest.Event != nil {
		newestAt = newest.Event.CreatedAt
	}
	if newest.HasUnreadable && newest.NewestUnreadable > newestAt {
		o.Logger.Warn("github.com/sattleme/checkout-go: the integration's channel has a newer format than this version reads; update the package")
	}
	if newest.Event == nil {
		return Lookup{Newer: newest.HasUnreadable}, nil
	}
	return Lookup{Found: newest.Value}, nil
}

// Read is the integration's channel, active, and the relays it was read on.
type Read struct {
	Channel *Channel
	Relays  []string
}

// ReadChannel reads the integration's channel, active, on the account's current relays (SPEC.md
// §14, "Reading the channel"): on the key's relays, then where the channel says the account moved
// (3 times at most), since only the new list gets its later changes, its removal included. revoked
// when it is removed, network when fewer than a majority of a list answered, channel_not_found
// when a majority of the key's relays answered without it.
func ReadChannel(ctx context.Context, pool *relay.Pool, k *key.IntegrationKey, o Options) (*Read, error) {
	relays := k.Relays
	lookup, err := LookupOn(ctx, pool, k, relays, o)
	if err != nil {
		return nil, err
	}
	if lookup.Unreachable {
		return nil, errs.NewNetwork("Fewer than a majority of the integration key's relays answered", nil)
	}
	if lookup.Found == nil {
		return nil, errs.NewChannelNotFound(lookup.Newer)
	}
	c := lookup.Found
	for move := 0; move < proto.MaxRelayMoves; move++ {
		if c.Removed || c.Relays == nil || urls.SameRelayList(c.Relays, relays) {
			break
		}
		next := c.Relays
		lookup, err = LookupOn(ctx, pool, k, next, o)
		if err != nil {
			return nil, err
		}
		if lookup.Unreachable {
			return nil, errs.NewNetwork("Fewer than a majority of the merchant's relays answered", nil)
		}
		// Not copied there (yet): the channel read so far stands, and the declarations still go to
		// the account's relays it names.
		if lookup.Found == nil {
			break
		}
		c, relays = lookup.Found, next
	}
	if c.Removed {
		return nil, errs.NewRevoked()
	}
	return &Read{Channel: c, Relays: relays}, nil
}

// MerchantRelays are the merchant's relays, where declarations go: the channel's, else the key's.
func MerchantRelays(c *Channel, k *key.IntegrationKey) []string {
	if c.Relays != nil {
		return c.Relays
	}
	return k.Relays
}
