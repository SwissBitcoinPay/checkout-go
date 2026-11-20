package onchain

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"sync"

	"github.com/sattleme/checkout-go/internal/address"
	"github.com/sattleme/checkout-go/internal/bolt11"
	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/relay"
	"github.com/sattleme/checkout-go/internal/text"
	"github.com/sattleme/checkout-go/internal/tolerance"
	"github.com/sattleme/checkout-go/internal/urls"
)

// Leases (SPEC.md §31, "Leases"): an order holds a deposit address of the lot for the time of its
// invoice with a record in the merchant's registry, signed by the order's sale, at the address's
// registry address. Outside, what anyone holding the registry key reads (when the lease ends, or
// its withdrawal); inside, sealed to the merchant's reading key as a declaration's, the order's
// terms. Like a declaration, a lease stays: a payment of its address in its time is the order's
// for good.

// LeaseTerms is what a lease holds of its order (the lease's JSON, inside).
type LeaseTerms struct {
	Address string
	Sats    int64
	// Invoice and VerifyURL are the order's Lightning invoice and its verify URL, "" for an order
	// paid on-chain only.
	Invoice   string
	VerifyURL string
	// CreatedAt and ExpiresAt are the Invoice's dates.
	CreatedAt int64
	ExpiresAt int64
	Price     declaration.Price
	// Note is the label: the note, else the reference.
	Note string
	// Tolerance is the order's tolerance of on-chain underpayments (basis points): the channel's
	// when the order was created, which judges its payment for good. 0: none (absent).
	Tolerance int64
	// Confirmed tells whether the lease is marked confirmed (false as first published, true once
	// confirmed); HasConfirmed false when it is absent, which is not confirmed either.
	Confirmed    bool
	HasConfirmed bool
}

// IsConfirmedLease tells whether a lease's terms make it confirmed: confirmed true, nothing else.
func IsConfirmedLease(terms LeaseTerms) bool { return terms.HasConfirmed && terms.Confirmed }

func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// LeaseInside is a lease's JSON (inside).
func LeaseInside(sale *derive.Sale, terms LeaseTerms) jsonv.Members {
	inside := jsonv.O(
		"version", proto.LeaseVersion,
		"address", terms.Address,
		"sats", terms.Sats,
		"invoice", orNull(terms.Invoice),
		"verifyUrl", orNull(terms.VerifyURL),
		"createdAt", terms.CreatedAt,
		"expiresAt", terms.ExpiresAt,
		"sale", jsonv.O("key", hex.EncodeToString(sale.Key), "fiat", terms.Price.JSON(), "note", terms.Note),
		"taker", sale.Taker,
	)
	if terms.Tolerance > 0 {
		inside = append(inside, jsonv.Member{Name: "tolerance", Value: terms.Tolerance})
	}
	if terms.HasConfirmed {
		inside = append(inside, jsonv.Member{Name: "confirmed", Value: terms.Confirmed})
	}
	return inside
}

// LeaseEvent is the lease of terms.Address for the sale, signed at createdAt; it ends at
// expiresAt + 120. Its tolerance follows taker when it is above 0, and is absent otherwise.
func LeaseEventOf(sale *derive.Sale, terms LeaseTerms, createdAt int64, random io.Reader) (*nostr.Event, error) {
	sealed, err := nip44.Encrypt(string(jsonv.Marshal(LeaseInside(sale, terms))), sale.InnerKey, random)
	if err != nil {
		return nil, err
	}
	return sealSaleRecord(sale, terms.Address, jsonv.O("version", proto.LeaseVersion, "end", terms.ExpiresAt+proto.GraceSeconds, "sealed", sealed), createdAt, random)
}

// sizedAddress is a text of a deposit address's length (bc1p and 58 more, SPEC.md §31, "Deposit
// addresses"): a lease's size before its address is known.
const sizedAddress = "bc1pqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"

// zeros reads zero bytes: the nonces and the auxiliary randomness of a record built for its size
// alone, never published.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// CheckLeaseSize builds the lease of terms by its size alone, before anything is asked or
// published (SPEC.md §17, step 6): the lease holds the invoice too, sealed twice as the
// declaration, with some 150 bytes more, and a lease that does not fit a NIP-44 plaintext fails
// here, its error nip44's. It is measured as first published, "confirmed": false, the longer of
// its two forms; the leases published differ from it by their address alone, of one length for
// every deposit address, and their mark. It reads none of the client's random bytes: the records
// of deterministic tests and vectors stay as they are.
func CheckLeaseSize(sale *derive.Sale, terms LeaseTerms) error {
	sized := terms
	sized.Address, sized.Confirmed, sized.HasConfirmed = sizedAddress, false, true
	_, err := LeaseEventOf(sale, sized, terms.CreatedAt, zeros{})
	return err
}

// WithdrawalEvent is the withdrawal of the sale's lease of address, which replaces it (same
// author and d tag): its created_at is strictly after the lease's.
func WithdrawalEvent(sale *derive.Sale, address string, createdAt int64, random io.Reader) (*nostr.Event, error) {
	return sealSaleRecord(sale, address, jsonv.O("version", proto.LeaseVersion, "withdrawn", true), createdAt, random)
}

func sealSaleRecord(sale *derive.Sale, address string, value any, createdAt int64, random io.Reader) (*nostr.Event, error) {
	d := derive.LeaseAddress(sale.Registry.Key, address)
	return nostr.Seal(nostr.SealOptions{
		Type:          proto.LeaseRecordType,
		Value:         value,
		Signer:        sale.SecretKey,
		EncryptionKey: derive.OuterKey(sale.Registry.Key, d, sale.Author),
		Address:       d,
		CreatedAt:     createdAt,
		Random:        random,
	})
}

// The kinds of a lease record's outside.
const (
	OutsideLease     = "lease"
	OutsideWithdrawn = "withdrawn"
	OutsideNewer     = "newer"
)

// LeaseOutside is a record's outside, as anyone holding the registry key reads it: a lease (End,
// Sealed), a withdrawal, or one of a newer format (authentic: an unreadable lease is never written
// over).
type LeaseOutside struct {
	Kind   string
	End    int64
	Sealed string
}

func parseOutside(value any) (LeaseOutside, error) {
	v, ok := jsonv.AsObject(value)
	if !ok {
		return LeaseOutside{}, nostr.ErrInvalid
	}
	version, _ := v.Get("version")
	if n, ok := version.(float64); ok && n > proto.LeaseVersion {
		return LeaseOutside{}, nostr.ErrNewer
	}
	if version != float64(proto.LeaseVersion) {
		return LeaseOutside{}, nostr.ErrInvalid
	}
	if withdrawn, _ := v.Get("withdrawn"); withdrawn == true {
		return LeaseOutside{Kind: OutsideWithdrawn}, nil
	}
	endValue, _ := v.Get("end")
	sealedValue, _ := v.Get("sealed")
	end, ok := jsonv.AsSafeInteger(endValue)
	sealed, isText := sealedValue.(string)
	if !ok || !isText {
		return LeaseOutside{}, nostr.ErrInvalid
	}
	return LeaseOutside{Kind: OutsideLease, End: end, Sealed: sealed}, nil
}

// LeaseEvent is what a read of leases keeps of a record's event, its id checked: what the walk of
// its address and a lease's confirmation need, and its signature. A lease opens without its
// signature, so anybody may send copies of one under ids of their own: of each, only these few
// bytes are kept, never its tags nor its content (SPEC.md §13, Limits).
type LeaseEvent struct {
	ID        string
	PubKey    string
	CreatedAt int64
	Sig       string
}

// LeaseRecord is a record of the registry at a deposit address's registry address D, by any
// author: its id checked, its signature not yet (SPEC.md §31, "Opening a lease"). Signed checks
// it, once: whether a copy of the event (its id) a relay sent carries a signature that checks out,
// so that a relay's copy with a damaged signature never hides the valid one another relay sent.
// AddCopy takes the signature of another relay's copy of it.
type LeaseRecord struct {
	Event   LeaseEvent
	D       string
	Outside LeaseOutside

	mu         sync.Mutex
	signatures []string
	signed     *bool
	// verify checks a signature of the record's event (nostr's check when nil): what a test of
	// the walk counts.
	verify func(sig string) bool
}

// NewLeaseRecord is the record of event at d with its outside, its signature checked when first
// asked.
func NewLeaseRecord(event LeaseEvent, d string, outside LeaseOutside) *LeaseRecord {
	// What holding needs of the outside: never its sealed inside.
	outside.Sealed = ""
	return &LeaseRecord{Event: event, D: d, Outside: outside, signatures: []string{event.Sig}}
}

// Signed tells whether a copy of the record carries a signature that checks out.
func (r *LeaseRecord) Signed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.signed == nil {
		valid := false
		for _, sig := range r.signatures {
			if r.verify != nil {
				valid = r.verify(sig)
			} else {
				valid = nostr.SignatureChecks(&nostr.Event{ID: r.Event.ID, PubKey: r.Event.PubKey, Sig: sig})
			}
			if valid {
				break
			}
		}
		r.signed = &valid
	}
	return *r.signed
}

// AddCopy takes the signature of another relay's copy of the event (its id is its hash): one more
// to try.
func (r *LeaseRecord) AddCopy(sig string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.signatures {
		if s == sig {
			return
		}
	}
	r.signatures = append(r.signatures, sig)
	if r.signed != nil && !*r.signed {
		r.signed = nil
	}
}

// OpenLeaseRecord opens what a relay sent as a record of type registry-onchain at the registry
// address d, with the outer key of d and of the event's own author (SPEC.md §31, "Opening a
// lease"): its id first; a content of a newer NIP-44 version is no record here (anybody can write
// one at d); its signature is left to IsLeased, which checks it only where it decides.
func OpenLeaseRecord(v any, registryKey []byte, d string) (*LeaseRecord, bool) {
	event, outside, ok := OpenLeaseOutside(v, registryKey, d)
	if !ok {
		return nil, false
	}
	return NewLeaseRecord(event, d, outside), true
}

// OpenLeaseOutside is OpenLeaseRecord's opening, with the whole outside (its sealed included).
func OpenLeaseOutside(v any, registryKey []byte, d string) (LeaseEvent, LeaseOutside, bool) {
	e, ok := nostr.WithValidID(v)
	if !ok || e.Kind != proto.RecordKind || nostr.DTagOf(v) != d || !text.IsHex32(e.PubKey) {
		return LeaseEvent{}, LeaseOutside{}, false
	}
	// A content of a newer NIP-44 version included: only the outer key seals a newer lease.
	plaintext, err := nip44.Decrypt(e.Content, derive.OuterKey(registryKey, d, e.PubKey))
	if err != nil {
		return LeaseEvent{}, LeaseOutside{}, false
	}
	envelope, err := jsonv.ParseString(plaintext)
	if err != nil {
		return LeaseEvent{}, LeaseOutside{}, false
	}
	o, isObject := jsonv.AsObject(envelope)
	if !isObject {
		return LeaseEvent{}, LeaseOutside{}, false
	}
	value, hasV := o.Get("v")
	t, _ := o.Get("t")
	if !hasV || t != proto.LeaseRecordType {
		return LeaseEvent{}, LeaseOutside{}, false
	}
	outside, err := parseOutside(value)
	if errors.Is(err, nostr.ErrNewer) {
		outside, err = LeaseOutside{Kind: OutsideNewer}, nil
	}
	if err != nil {
		return LeaseEvent{}, LeaseOutside{}, false
	}
	return LeaseEvent{ID: e.ID, PubKey: e.PubKey, CreatedAt: e.CreatedAt, Sig: e.Sig}, outside, true
}

// holds tells whether a record holds its address at t: a lease not ended (at most the clock
// tolerance before t: another server's clock may be behind), or of a newer format.
func holds(r *LeaseRecord, t int64) bool {
	return r.Outside.Kind == OutsideNewer || r.Outside.Kind == OutsideLease && r.Outside.End >= t-proto.ClockToleranceSeconds
}

// IsLeased tells whether the records of one address lease it at t (SPEC.md §31, "Leases"): of each
// author, its records from the newest (a withdrawal replaces its author's lease): one that holds
// the address leases it; one that holds nothing ends the author's records, unless an older one of
// them holds and its signature does not check out. A record that holds is never checked for its
// signature: only a holder of the registry key seals one, and it may lease.
func IsLeased(records []*LeaseRecord, t int64) bool {
	var authors []string
	byAuthor := map[string][]*LeaseRecord{}
	for _, r := range records {
		if _, ok := byAuthor[r.Event.PubKey]; !ok {
			authors = append(authors, r.Event.PubKey)
		}
		byAuthor[r.Event.PubKey] = append(byAuthor[r.Event.PubKey], r)
	}
	leased := false
	// Every author walked, as the vectors count the signatures checked.
	for _, author := range authors {
		walk := append([]*LeaseRecord(nil), byAuthor[author]...)
		sort.SliceStable(walk, func(i, j int) bool {
			return nostr.Replaces(walk[i].Event.CreatedAt, walk[i].Event.ID, walk[j].Event.CreatedAt, walk[j].Event.ID)
		})
		for i, r := range walk {
			if holds(r, t) {
				leased = true
				break
			}
			olderHolds := false
			for _, older := range walk[i+1:] {
				if holds(older, t) {
					olderHolds = true
					break
				}
			}
			if !olderHolds || r.Signed() {
				break
			}
		}
	}
	return leased
}

// IsConfirmed tells whether the lease own (an event of the sale) is confirmed by records, a
// complete read of its address's records (SPEC.md §31, "Leasing an address", step 3.3): no record
// of another author holds the address at t, and own is the newest record of its author there.
func IsConfirmed(records []*LeaseRecord, own LeaseEvent, t int64) bool {
	found := false
	var others []*LeaseRecord
	for _, r := range records {
		if r.Event.ID == own.ID {
			found = true
		}
		if r.Event.PubKey != own.PubKey {
			others = append(others, r)
		}
	}
	if !found || IsLeased(others, t) {
		return false
	}
	for _, r := range records {
		if r.Event.PubKey == own.PubKey && r.Event.ID != own.ID && nostr.Replaces(r.Event.CreatedAt, r.Event.ID, own.CreatedAt, own.ID) {
			return false
		}
	}
	return true
}

// LeaseRead is the records of the registry at the registry addresses of deposit addresses.
type LeaseRead struct {
	// Records are by address.
	Records map[string][]*LeaseRecord
	// Answered tells whether a majority of the relays answered: else the read is no answer.
	Answered bool
	// Complete tells whether a majority of the relays read them completely.
	Complete bool
}

// Confirms tells whether this read, the leases of address read again on every relay once the
// lease own was published (SPEC.md §31, "Leasing an address", step 3.3), confirms own at t: a
// majority answered and read completely, and of its records (IsConfirmed) no other author's holds
// the address while own is the newest of its author's there.
func (r *LeaseRead) Confirms(address string, own LeaseEvent, t int64) bool {
	return r.Answered && r.Complete && IsConfirmed(r.Records[address], own, t)
}

// ReadLeases reads the records of the registry at the registry addresses of addresses, by
// address: a complete read on the merchant's relays, since 7200 s before now (older leases have
// ended), with a key of this read alone (leases are signed by sales the integration does not
// know). Answered says whether a majority answered; Complete whether a majority read them
// completely. Each relay's first copy of an event counts, its signature tried with the other
// relays'.
func ReadLeases(ctx context.Context, pool *relay.Pool, registryKey []byte, addresses, relays []string, now int64, random io.Reader, everyRelay bool) (*LeaseRead, error) {
	byD := map[string]string{}
	var ds []string
	found := map[string][]*LeaseRecord{}
	for _, a := range addresses {
		d := derive.LeaseAddress(registryKey, a)
		if _, ok := byD[d]; !ok {
			ds = append(ds, d)
		}
		byD[d] = a
		found[a] = nil
	}
	if len(addresses) == 0 {
		return &LeaseRead{Records: found, Answered: true, Complete: true}, nil
	}
	needed := relay.Majority(len(relays))
	authKey := make([]byte, 32)
	if _, err := io.ReadFull(random, authKey); err != nil {
		return nil, err
	}
	since := now - proto.LeaseLookbackSeconds
	read := pool.QueryComplete(relays, relay.Filter{Kinds: []int64{proto.RecordKind}, D: ds, Since: &since}, authKey, needed,
		// At a lease's address, by any author: the record each event opens to, as it comes.
		func(v any) (any, bool) {
			d := nostr.DTagOf(v)
			if _, ok := byD[d]; !ok || d == "" {
				return nil, false
			}
			record, ok := OpenLeaseRecord(v, registryKey, d)
			return record, ok
		}, everyRelay)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Ids that checked out: another copy of one is the same event, its signature one more to try.
	checked := map[string]*LeaseRecord{}
	for _, answer := range read.Answers {
		// A relay's first copy of each id: at most one signature per relay to try.
		ofRelay := map[string]bool{}
		for _, v := range answer.Events {
			record := v.(*LeaseRecord)
			id := record.Event.ID
			if ofRelay[id] {
				continue
			}
			ofRelay[id] = true
			if known, ok := checked[id]; ok {
				known.AddCopy(record.Event.Sig)
				continue
			}
			checked[id] = record
			a := byD[record.D]
			found[a] = append(found[a], record)
		}
	}
	return &LeaseRead{Records: found, Answered: read.MajorityAnswered(len(relays)), Complete: read.MajorityComplete(len(relays))}, nil
}

// OpenedLease is a lease of the sale, opened by the sale (or the merchant): its terms.
type OpenedLease struct {
	Event *nostr.Event
	// D is the d tag: the registry address of Terms.Address.
	D     string
	Terms LeaseTerms
	// Inside is the lease's JSON, inside, as it was sealed.
	Inside *jsonv.Object
}

// OpenLeaseInside opens the inside of a lease's outside with the sale's inner key (SPEC.md §31,
// "Opening it as the sale"): its terms (its tolerance read as 0 when it is absent or not one).
// nostr.ErrNewer for a newer version, nostr.ErrInvalid when they are not valid.
func OpenLeaseInside(outside LeaseOutside, d string, sale *derive.Sale) (LeaseTerms, *jsonv.Object, error) {
	plaintext, err := nip44.Decrypt(outside.Sealed, sale.InnerKey)
	if err != nil {
		return LeaseTerms{}, nil, nostr.ErrInvalid
	}
	insideValue, err := jsonv.ParseString(plaintext)
	if err != nil {
		return LeaseTerms{}, nil, nostr.ErrInvalid
	}
	v, ok := jsonv.AsObject(insideValue)
	if !ok {
		return LeaseTerms{}, nil, nostr.ErrInvalid
	}
	version, _ := v.Get("version")
	if n, ok := version.(float64); ok && n > proto.LeaseVersion {
		return LeaseTerms{}, nil, nostr.ErrNewer
	}
	if version != float64(proto.LeaseVersion) {
		return LeaseTerms{}, nil, nostr.ErrInvalid
	}
	addressValue, _ := v.Get("address")
	if !address.IsDeposit(addressValue) || derive.LeaseAddress(sale.Registry.Key, addressValue.(string)) != d {
		return LeaseTerms{}, nil, nostr.ErrInvalid
	}
	satsValue, _ := v.Get("sats")
	sats, ok := jsonv.AsSafeInteger(satsValue)
	if !ok || sats < 1 || sats > proto.MaxSats {
		return LeaseTerms{}, nil, nostr.ErrInvalid
	}
	createdAtValue, _ := v.Get("createdAt")
	expiresAtValue, _ := v.Get("expiresAt")
	createdAt, ok1 := jsonv.AsSafeInteger(createdAtValue)
	expiresAt, ok2 := jsonv.AsSafeInteger(expiresAtValue)
	if !ok1 || !ok2 || createdAt >= expiresAt || outside.End != expiresAt+proto.GraceSeconds {
		return LeaseTerms{}, nil, nostr.ErrInvalid
	}
	terms := LeaseTerms{Address: addressValue.(string), Sats: sats, CreatedAt: createdAt, ExpiresAt: expiresAt}
	// Both null for an order paid on-chain only: absent is not null.
	invoiceValue, hasInvoice := v.Get("invoice")
	verifyValue, hasVerify := v.Get("verifyUrl")
	if !hasInvoice || !hasVerify {
		return LeaseTerms{}, nil, nostr.ErrInvalid
	}
	if invoiceValue == nil {
		if verifyValue != nil {
			return LeaseTerms{}, nil, nostr.ErrInvalid
		}
	} else {
		invoice, ok1 := invoiceValue.(string)
		verifyURL, ok2 := verifyValue.(string)
		if !ok1 || !ok2 {
			return LeaseTerms{}, nil, nostr.ErrInvalid
		}
		if _, ok := urls.ParseHTTPS(verifyURL); !ok {
			return LeaseTerms{}, nil, nostr.ErrInvalid
		}
		decoded, ok := bolt11.Decode(invoice)
		if !ok || decoded.Network != "bc" || !decoded.HasAmount || decoded.AmountMsat != sats*1000 ||
			decoded.Timestamp != createdAt || decoded.Timestamp+decoded.ExpirySeconds != expiresAt {
			return LeaseTerms{}, nil, nostr.ErrInvalid
		}
		terms.Invoice, terms.VerifyURL = invoice, verifyURL
	}
	saleValue, _ := v.Get("sale")
	saleObject, ok := jsonv.AsObject(saleValue)
	if !ok {
		return LeaseTerms{}, nil, nostr.ErrInvalid
	}
	keyValue, _ := saleObject.Get("key")
	noteValue, _ := saleObject.Get("note")
	note, isText := noteValue.(string)
	if keyValue != hex.EncodeToString(sale.Key) || !isText {
		return LeaseTerms{}, nil, nostr.ErrInvalid
	}
	fiatValue, _ := saleObject.Get("fiat")
	price, ok := declaration.ParsePrice(fiatValue)
	if !ok {
		return LeaseTerms{}, nil, nostr.ErrInvalid
	}
	terms.Price, terms.Note = price, note
	// Absent (a lease without tolerance) or invalid: none.
	toleranceValue, _ := v.Get("tolerance")
	terms.Tolerance = tolerance.Of(toleranceValue)
	// Absent, or anything but true: not confirmed.
	if confirmed, has := v.Get("confirmed"); has {
		terms.Confirmed, terms.HasConfirmed = confirmed == true, true
	}
	return terms, v, nil
}

// The kinds of a sale's lease record.
const (
	SaleLease     = "lease"
	SaleWithdrawn = "withdrawn"
	SaleNewer     = "newer"
)

// SaleLeaseRecord is a record of the sale at a registry address: its lease, its withdrawal, or one
// of a newer format (authentic, unreadable: the newest at its address, it leaves no lease there).
type SaleLeaseRecord struct {
	Kind  string
	Lease *OpenedLease
	Event *nostr.Event
	D     string
}

// OpenSaleLease reads what a relay sent, if it is a lease (or a withdrawal) of the sale: at a
// registry address (its d tag, hex32), signed by the sale, with valid terms inside. Kind
// SaleNewer for one of a newer format; false otherwise.
func OpenSaleLease(v any, sale *derive.Sale, memo *nostr.SigMemo) (SaleLeaseRecord, bool) {
	d := nostr.DTagOf(v)
	pubkey, _ := jsonv.Field(v, "pubkey")
	if d == "" || pubkey != sale.Author {
		return SaleLeaseRecord{}, false
	}
	opened, ok := nostr.Open(v, nostr.OpenOptions{
		Type:          proto.LeaseRecordType,
		EncryptionKey: derive.OuterKey(sale.Registry.Key, d, sale.Author),
		Address:       d,
		Author:        sale.Author,
		Memo:          memo,
	}, func(value any, _ *nostr.Event) (LeaseOutside, error) { return parseOutside(value) })
	if !ok {
		return SaleLeaseRecord{}, false
	}
	if opened.Newer {
		return SaleLeaseRecord{Kind: SaleNewer, Event: opened.Event, D: d}, true
	}
	if opened.Value.Kind == OutsideWithdrawn {
		return SaleLeaseRecord{Kind: SaleWithdrawn, Event: opened.Event, D: d}, true
	}
	terms, inside, err := OpenLeaseInside(opened.Value, d, sale)
	if errors.Is(err, nostr.ErrNewer) {
		return SaleLeaseRecord{Kind: SaleNewer, Event: opened.Event, D: d}, true
	}
	if err != nil {
		return SaleLeaseRecord{}, false
	}
	return SaleLeaseRecord{Kind: SaleLease, Lease: &OpenedLease{Event: opened.Event, D: d, Terms: terms, Inside: inside}, Event: opened.Event, D: d}, true
}
