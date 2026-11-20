package onchain

import (
	"context"
	"encoding/hex"
	"io"
	"regexp"
	"slices"
	"strconv"

	"github.com/sattleme/checkout-go/internal/address"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/relay"
	"github.com/sattleme/checkout-go/internal/text"
)

// Witnesses (SPEC.md §31, "Witnesses"): the integration records each transaction it sees paying an
// address of its lot, and when: during the lease of an order (that order's sale), or at no lease's
// time (none). So that every reader, the merchant's app included, tells the same order paid. Only
// the integration writes them (signed by its seen secret, sealed with its channel's key); the
// merchant's app reads them to attribute deposits and to take paid addresses out of the lot.

// SeenKeys are the keys of the integration's witnesses. Secrets but Author: never logged.
type SeenKeys struct {
	SecretKey []byte
	// Author is the author of every witness (hex).
	Author     string
	AddressKey []byte
	// EncryptionKey is the channel's: the witnesses' NIP-44 conversation key.
	EncryptionKey []byte
}

// SeenOf derives the integration's witness keys.
func SeenOf(integrationKey []byte) *SeenKeys {
	s := derive.SeenKeys(integrationKey)
	_, encryptionKey := derive.ChannelKeys(integrationKey)
	return &SeenKeys{SecretKey: s.SecretKey, Author: s.Author, AddressKey: s.AddressKey, EncryptionKey: encryptionKey}
}

func mac(key []byte, s string) string { return hex.EncodeToString(derive.HMAC(key, []byte(s))) }

// saleOrNone is a witness's sale as its addresses write it: the sale author, or none.
func saleOrNone(sale string) string {
	if sale == "" {
		return proto.NoSale
	}
	return sale
}

// WitnessAddress is where the witness of txid paying address for sale (a sale author, or "":
// none) is.
func WitnessAddress(addressKey []byte, address, txid, sale string) string {
	return mac(addressKey, address+"/"+txid+"/"+saleOrNone(sale))
}

// OrderTag is the o tag of the witnesses of address for sale ("": none): how a check reads them.
func OrderTag(addressKey []byte, address, sale string) string {
	return mac(addressKey, "order/"+address+"/"+saleOrNone(sale))
}

// AddressTag is the address tag of address: a witness's second o tag, which finds every witness
// of the address, whatever its sale.
func AddressTag(addressKey []byte, address string) string {
	return mac(addressKey, "address/"+address)
}

var coinPattern = regexp.MustCompile(`^[0-9a-f]{64}:(?:0|[1-9][0-9]{0,9})$`)

// IsCoin tells whether v is a coin as a witness writes it: <txid>:<vout>, the vout a u32 in
// decimal without leading zeros.
func IsCoin(v any) bool {
	s, ok := v.(string)
	if !ok || !coinPattern.MatchString(s) {
		return false
	}
	vout, err := strconv.ParseUint(s[65:], 10, 64)
	return err == nil && vout <= maxVout
}

// Witness is what a witness says.
type Witness struct {
	Address string
	Txid    string
	// Sale is the sale author (hex), or "": at no lease's time.
	Sale string
	// Sats is what Txid pays Address.
	Sats int64
	// Inputs are the coins Txid spends, its first 100, in their order.
	Inputs []string
	// Replacement is held for the sale only as a replacement of payments in time: Txid came after
	// the lease.
	Replacement bool
	// ExpiresAt is the expiresAt of the invoice whose lease Txid came in (or stands for); unused
	// for none.
	ExpiresAt int64
}

// WitnessEvent is the signed event of a witness, created_at when it was seen.
func WitnessEvent(seen *SeenKeys, w Witness, createdAt int64, random io.Reader) (*nostr.Event, error) {
	inputs := w.Inputs
	if len(inputs) > proto.MaxWitnessInputs {
		inputs = inputs[:proto.MaxWitnessInputs]
	}
	if inputs == nil {
		inputs = []string{}
	}
	var sale, expiresAt any
	if w.Sale != "" {
		sale, expiresAt = w.Sale, w.ExpiresAt
	}
	return nostr.Seal(nostr.SealOptions{
		Type: proto.WitnessRecordType,
		Value: jsonv.O(
			"version", proto.WitnessVersion,
			"address", w.Address,
			"txid", w.Txid,
			"sale", sale,
			"sats", w.Sats,
			"inputs", inputs,
			"replacement", w.Replacement,
			"expiresAt", expiresAt,
		),
		Signer:        seen.SecretKey,
		EncryptionKey: seen.EncryptionKey,
		Address:       WitnessAddress(seen.AddressKey, w.Address, w.Txid, w.Sale),
		CreatedAt:     createdAt,
		Tags: [][]string{
			{proto.OrderTag, OrderTag(seen.AddressKey, w.Address, w.Sale)},
			{proto.OrderTag, AddressTag(seen.AddressKey, w.Address)},
		},
		Random: random,
	})
}

func hasOTag(e *nostr.Event, tag string) bool {
	for _, t := range e.Tags {
		if len(t) >= 2 && t[0] == proto.OrderTag && t[1] == tag {
			return true
		}
	}
	return false
}

// parseWitness reads a witness's value, checked against its event's tags: its d tag is the witness
// address of what it names, an o tag its order tag and an o tag its address's tag.
func parseWitness(value any, e *nostr.Event, addressKey []byte) (Witness, error) {
	v, ok := jsonv.AsObject(value)
	if !ok {
		return Witness{}, nostr.ErrInvalid
	}
	version, _ := v.Get("version")
	if n, ok := version.(float64); ok && n > proto.WitnessVersion {
		return Witness{}, nostr.ErrNewer
	}
	if version != float64(proto.WitnessVersion) {
		return Witness{}, nostr.ErrInvalid
	}
	addressValue, _ := v.Get("address")
	txidValue, _ := v.Get("txid")
	// Null for none: absent is not null.
	saleValue, hasSale := v.Get("sale")
	if !address.IsDeposit(addressValue) || !text.IsHex32Value(txidValue) || !hasSale {
		return Witness{}, nostr.ErrInvalid
	}
	if saleValue != nil && !text.IsHex32Value(saleValue) {
		return Witness{}, nostr.ErrInvalid
	}
	satsValue, _ := v.Get("sats")
	sats, ok := jsonv.AsSafeInteger(satsValue)
	if !ok || sats < 1 || sats > proto.MaxSats {
		return Witness{}, nostr.ErrInvalid
	}
	inputsValue, _ := v.Get("inputs")
	inputItems, ok := inputsValue.([]any)
	if !ok || len(inputItems) > proto.MaxWitnessInputs {
		return Witness{}, nostr.ErrInvalid
	}
	inputs := make([]string, 0, len(inputItems))
	seenCoins := map[string]bool{}
	for _, item := range inputItems {
		if !IsCoin(item) || seenCoins[item.(string)] {
			return Witness{}, nostr.ErrInvalid
		}
		seenCoins[item.(string)] = true
		inputs = append(inputs, item.(string))
	}
	replacementValue, _ := v.Get("replacement")
	replacement, ok := replacementValue.(bool)
	if !ok || saleValue == nil && replacement {
		return Witness{}, nostr.ErrInvalid
	}
	// Null for none, the invoice's end for a sale; absent, it is no witness.
	expiresAtValue, hasExpiresAt := v.Get("expiresAt")
	var expiresAt int64
	if saleValue == nil {
		if !hasExpiresAt || expiresAtValue != nil {
			return Witness{}, nostr.ErrInvalid
		}
	} else {
		n, ok := jsonv.AsSafeInteger(expiresAtValue)
		if !ok {
			return Witness{}, nostr.ErrInvalid
		}
		expiresAt = n
	}
	w := Witness{Address: addressValue.(string), Txid: txidValue.(string), Sats: sats, Inputs: inputs, Replacement: replacement, ExpiresAt: expiresAt}
	if saleValue != nil {
		w.Sale = saleValue.(string)
	}
	if d, _ := e.DTag(); d != WitnessAddress(addressKey, w.Address, w.Txid, w.Sale) ||
		!hasOTag(e, OrderTag(addressKey, w.Address, w.Sale)) || !hasOTag(e, AddressTag(addressKey, w.Address)) {
		return Witness{}, nostr.ErrInvalid
	}
	return w, nil
}

// OpenedWitness is a witness a relay sent, opened: its value, or Newer (authentic, but of a newer
// format: what it names cannot be read, its d tag can).
type OpenedWitness struct {
	Event   *nostr.Event
	Witness Witness
	Newer   bool
	D       string
}

// OpenWitness reads what a relay sent, if it is a witness of this integration.
func OpenWitness(v any, seen *SeenKeys, memo *nostr.SigMemo) (OpenedWitness, bool) {
	d := nostr.DTagOf(v)
	if d == "" {
		return OpenedWitness{}, false
	}
	opened, ok := nostr.Open(v, nostr.OpenOptions{Type: proto.WitnessRecordType, EncryptionKey: seen.EncryptionKey, Address: d, Author: seen.Author, Memo: memo},
		func(value any, e *nostr.Event) (Witness, error) { return parseWitness(value, e, seen.AddressKey) })
	if !ok {
		return OpenedWitness{}, false
	}
	return OpenedWitness{Event: opened.Event, Witness: opened.Value, Newer: opened.Newer, D: d}, true
}

// SaleWitness is a witness of the order's sale, as a check uses it.
type SaleWitness struct {
	Sats        int64
	Inputs      []string
	Replacement bool
	// SeenAt is its created_at: when it was seen.
	SeenAt int64
}

// OrderWitnesses are the witnesses of an invoice: of this invoice (its sale and its expiresAt), of
// other invoices (another sale, or another expiresAt of its sale), of none, and of a newer format.
type OrderWitnesses struct {
	// Sale are of this invoice, by txid.
	Sale map[string]SaleWitness
	// Others are of other invoices, by txid: what they name counts for nothing here.
	Others map[string]SaleWitness
	// None are the txids a witness none names.
	None map[string]bool
	// Newer are the witness addresses (d tags) of the witnesses of a newer format.
	Newer map[string]bool
}

// readTags are the o tags of the read of an invoice's witnesses: of the sale, of none, of the
// address.
func readTags(seen *SeenKeys, address, saleAuthor string) []string {
	return []string{OrderTag(seen.AddressKey, address, saleAuthor), OrderTag(seen.AddressKey, address, ""), AddressTag(seen.AddressKey, address)}
}

// ReadOrderWitnesses reads the witnesses of the invoice with the address and the end expiresAt,
// for the sale saleAuthor, on the merchant's relays by their order tags (of the sale and of none)
// and the address tag (every sale's), as soon as a majority answered (on every relay with
// everyRelay), with a key of this read alone. network when fewer than a majority answered.
func ReadOrderWitnesses(ctx context.Context, pool *relay.Pool, seen *SeenKeys, address, saleAuthor string, expiresAt int64, relays []string, random io.Reader, everyRelay bool) (*OrderWitnesses, error) {
	needed := relay.Majority(len(relays))
	authKey := make([]byte, 32)
	if _, err := io.ReadFull(random, authKey); err != nil {
		return nil, err
	}
	memo := nostr.NewSigMemo()
	answers := pool.QueryRelays(relays, relay.Filter{Kinds: []int64{proto.RecordKind}, Authors: []string{seen.Author}, O: readTags(seen, address, saleAuthor)}, authKey, relay.QueryOptions{
		Needed:     needed,
		EveryRelay: everyRelay,
		Keep: nostr.RecordsThatOpen(func(_, author string) []byte {
			if author == seen.Author {
				return seen.EncryptionKey
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
	var events []any
	for _, a := range answers {
		events = append(events, a.Events...)
	}
	return OrderWitnessesOf(events, seen, address, saleAuthor, expiresAt, memo), nil
}

// OrderWitnessesOf reads the witnesses of the invoice from what the relays sent to its read: of
// each witness address, the newest event that opens (one of a newer format, its event checked, is
// kept as such). A witness that holds none of the read's o tags, or whose address is not address,
// is of no invoice of this read and is ignored, of a newer format or not: only a relay that does
// not keep to the filter sends one.
func OrderWitnessesOf(events []any, seen *SeenKeys, address, saleAuthor string, expiresAt int64, memo *nostr.SigMemo) *OrderWitnesses {
	tags := map[string]bool{}
	for _, t := range readTags(seen, address, saleAuthor) {
		tags[t] = true
	}
	var order []string
	newest := map[string]OpenedWitness{}
	checked := map[string]bool{}
	for _, v := range events {
		if id, ok := jsonv.Field(v, "id"); ok {
			if s, ok := id.(string); ok && checked[s] {
				continue
			}
		}
		opened, ok := OpenWitness(v, seen, memo)
		if !ok {
			continue
		}
		checked[opened.Event.ID] = true
		ofRead := false
		for _, t := range opened.Event.Tags {
			if len(t) >= 2 && t[0] == proto.OrderTag && tags[t[1]] {
				ofRead = true
				break
			}
		}
		if !ofRead || !opened.Newer && opened.Witness.Address != address {
			continue
		}
		d, _ := opened.Event.DTag()
		known, has := newest[d]
		if !has {
			order = append(order, d)
		}
		if !has || nostr.Replaces(opened.Event.CreatedAt, opened.Event.ID, known.Event.CreatedAt, known.Event.ID) {
			newest[d] = opened
		}
	}
	w := &OrderWitnesses{Sale: map[string]SaleWitness{}, Others: map[string]SaleWitness{}, None: map[string]bool{}, Newer: map[string]bool{}}
	for _, d := range order {
		opened := newest[d]
		if opened.Newer {
			w.Newer[d] = true
			continue
		}
		of := SaleWitness{Sats: opened.Witness.Sats, Inputs: opened.Witness.Inputs, Replacement: opened.Witness.Replacement, SeenAt: opened.Event.CreatedAt}
		switch {
		case opened.Witness.Sale == "":
			w.None[opened.Witness.Txid] = true
		case opened.Witness.Sale == saleAuthor && opened.Witness.ExpiresAt == expiresAt:
			w.Sale[opened.Witness.Txid] = of
		default:
			w.Others[opened.Witness.Txid] = of
		}
	}
	return w
}

// AddressesNamed is the addresses an event names when leasing (SPEC.md §31, "Leasing an address",
// step 3): when it is of the seen author and checks out (SPEC.md §11), each address of byTag (by
// its address tag) whose tag it holds as an o tag, whatever its content (one that does not open
// as a witness, or of a newer format, names it all the same); none otherwise. memo checks the
// signature of each copy once (nil: each time), and only of an event that would name an address.
func AddressesNamed(v any, seenAuthor string, byTag map[string]string, memo *nostr.SigMemo) []string {
	pubkey, _ := jsonv.Field(v, "pubkey")
	tagsValue, _ := jsonv.Field(v, "tags")
	items, ok := tagsValue.([]any)
	if pubkey != seenAuthor || !ok {
		return nil
	}
	var named []string
	for _, item := range items {
		tag, ok := item.([]any)
		if !ok || len(tag) < 2 || tag[0] != proto.OrderTag {
			continue
		}
		if s, ok := tag[1].(string); ok {
			if a, ok := byTag[s]; ok && !slices.Contains(named, a) {
				named = append(named, a)
			}
		}
	}
	if len(named) == 0 {
		return nil
	}
	// The cheap checks first: the signature only of an event that would name an address.
	e, ok := nostr.WithValidID(v)
	if !ok || !memo.Check(e) {
		return nil
	}
	return named
}

// ReadNamedAddresses reads the addresses of addresses a witness names (SPEC.md §31, "Leasing an
// address", step 2): on the merchant's relays by their address tags, not paged, as soon as a
// majority answered, with a key of this read alone. An event of the seen author that checks out
// with an address's tag as an o tag names that address, one of a newer format included
// (AddressesNamed). network when fewer than a majority answered.
func ReadNamedAddresses(ctx context.Context, pool *relay.Pool, seen *SeenKeys, addresses, relays []string, random io.Reader) (map[string]bool, error) {
	named := map[string]bool{}
	if len(addresses) == 0 {
		return named, nil
	}
	byTag := map[string]string{}
	var tags []string
	for _, a := range addresses {
		t := AddressTag(seen.AddressKey, a)
		if _, ok := byTag[t]; !ok {
			tags = append(tags, t)
		}
		byTag[t] = a
	}
	memo := nostr.NewSigMemo()
	needed := relay.Majority(len(relays))
	authKey := make([]byte, 32)
	if _, err := io.ReadFull(random, authKey); err != nil {
		return nil, err
	}
	answers := pool.QueryRelays(relays, relay.Filter{Kinds: []int64{proto.RecordKind}, Authors: []string{seen.Author}, O: tags}, authKey, relay.QueryOptions{
		Needed: needed,
		// Only the addresses an event names are kept.
		Keep: func(v any) (any, bool) {
			names := AddressesNamed(v, seen.Author, byTag, memo)
			return names, len(names) > 0
		},
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !relay.MajorityAnswered(answers, len(relays)) {
		return nil, errs.NewNetwork("Fewer than a majority of the merchant's relays answered", nil)
	}
	for _, a := range answers {
		for _, v := range a.Events {
			for _, addr := range v.([]string) {
				named[addr] = true
			}
		}
	}
	return named, nil
}

// PublishWitness publishes a witness on the merchant's relays, signed and authenticated with the
// seen secret: nil once a majority took it, else network.
func PublishWitness(ctx context.Context, pool *relay.Pool, seen *SeenKeys, w Witness, relays []string, createdAt int64, random io.Reader) error {
	event, err := WitnessEvent(seen, w, createdAt, random)
	if err != nil {
		return err
	}
	if err := pool.PublishToQuorum(relays, event, seen.SecretKey, relay.Majority(len(relays))); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errs.NewNetwork("Fewer than a majority of the merchant's relays took the witness of a payment", err)
	}
	return nil
}
