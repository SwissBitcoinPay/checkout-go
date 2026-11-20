// Package checkoutlink makes and reads checkout links (SPEC.md §30): a URL of the merchant's web
// app, <origin>/pay#c.<author>.<key>.<relays>, whose page reads the invoice from the merchant's
// relays, shows it, checks its payment from the customer's browser and, once it is paid, sends the
// customer to the order's redirect URL. The record is sealed with a key of the link alone (in the
// URL's fragment, which browsers never send to the web app's host) and signed by an author of the
// link alone, whose secret is forgotten once it is published: nobody, the integrator included, can
// change a link afterwards. The package writes links; it also reads them (fragments, records), as
// the page does, for the conformance vectors.
package checkoutlink

import (
	"encoding/hex"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/sattleme/checkout-go/internal/address"
	"github.com/sattleme/checkout-go/internal/b64"
	"github.com/sattleme/checkout-go/internal/bitcoinnode"
	"github.com/sattleme/checkout-go/internal/bolt11"
	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/secp"
	"github.com/sattleme/checkout-go/internal/text"
	"github.com/sattleme/checkout-go/internal/urls"
)

// OriginOf is the origin a channel's checkout field gives: "" when it gives none or an invalid
// one.
func OriginOf(checkout any) string {
	origin, _ := jsonv.Field(checkout, "origin")
	if !urls.IsCheckoutOrigin(origin) {
		return ""
	}
	return origin.(string)
}

// URL is the URL of a link: <origin>/pay#c.<author>.<key>.<relays>.
func URL(origin, author string, linkKey []byte, relays []string) string {
	authorBytes, _ := hex.DecodeString(author)
	parts := []string{proto.CheckoutMarker, b64.EncodeURL(authorBytes), b64.EncodeURL(linkKey), urls.EncodeRelayList(relays)}
	return origin + proto.CheckoutPath + "#" + strings.Join(parts, ".")
}

// Rest is the rest author (hex) and the rest key of an invoice with an address (SPEC.md §31).
type Rest struct {
	Author string
	Key    []byte
}

// Input is what a link's record holds.
type Input struct {
	BusinessName string
	// Invoice and VerifyURL are "" for an order paid on-chain only.
	Invoice   string
	VerifyURL string
	// OnchainAddress is "" without one: a record of version 1.
	OnchainAddress string
	Sats           int64
	CreatedAt      int64
	ExpiresAt      int64
	// Fiat is the order's price as declared; nil for an order in sats.
	Fiat *declaration.Price
	// Note is the label: the note, else the reference.
	Note        string
	RedirectURL string
	// Rest is, with an address, its rest author and rest key; nil without them.
	Rest *Rest
	// BitcoinNode is, with an address, the merchant's Bitcoin node the page reads the chain from
	// (bitcoinnode.ForRecord: the channel's, with payers true): its url and fallback. nil without
	// one, and never written in a record of version 1.
	BitcoinNode *bitcoinnode.RecordNode
}

func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Value is the record of a link to an invoice (SPEC.md §30, "The record"): version 2 with an
// address (with the node the page reads, when there is one), else version 1.
func Value(in Input) jsonv.Members {
	var fiat any
	if in.Fiat != nil {
		fiat = in.Fiat.JSON()
	}
	if in.OnchainAddress == "" {
		return jsonv.O(
			"version", proto.CheckoutVersion,
			"businessName", in.BusinessName,
			"invoice", in.Invoice,
			"verifyUrl", in.VerifyURL,
			"fiat", fiat,
			"note", in.Note,
			"redirectUrl", orNull(in.RedirectURL),
		)
	}
	value := jsonv.O(
		"version", proto.CheckoutOnchainVersion,
		"businessName", in.BusinessName,
		"invoice", orNull(in.Invoice),
		"verifyUrl", orNull(in.VerifyURL),
		"onchainAddress", in.OnchainAddress,
		"sats", in.Sats,
		"createdAt", in.CreatedAt,
		"expiresAt", in.ExpiresAt,
		"fiat", fiat,
		"note", in.Note,
		"redirectUrl", orNull(in.RedirectURL),
	)
	if in.Rest != nil {
		value = append(value, jsonv.O("restAuthor", in.Rest.Author, "restKey", hex.EncodeToString(in.Rest.Key))...)
	}
	// Absent, never null, without one: a page that does not know it ignores it.
	if in.BitcoinNode != nil {
		value = append(value, jsonv.Member{Name: "bitcoinNode", Value: in.BitcoinNode.JSON()})
	}
	return value
}

// Link is a new link, before it is published.
type Link struct {
	URL   string
	Event *nostr.Event
	// SecretKey is the author's secret: it answers the relays' authentication while the event is
	// published, then is dropped. Never stored, returned to the caller nor logged.
	SecretKey []byte
}

// authorSecret is 32 random bytes, drawn again until they are a secp256k1 secret key.
func authorSecret(random io.Reader) ([]byte, error) {
	for {
		secret := make([]byte, proto.KeyBytes)
		if _, err := io.ReadFull(random, secret); err != nil {
			return nil, err
		}
		if secp.IsSecretKey(secret) {
			return secret, nil
		}
	}
}

// New makes a new link to the invoice value holds, ending expiresAt: a fresh link key and
// author, the record signed at createdAt, dropped by the relays a week after the invoice's end,
// and the URL on origin naming relays (where it is to be published).
func New(origin string, relays []string, value jsonv.Members, expiresAt, createdAt int64, random io.Reader) (*Link, error) {
	linkKey := make([]byte, proto.KeyBytes)
	if _, err := io.ReadFull(random, linkKey); err != nil {
		return nil, err
	}
	secret, err := authorSecret(random)
	if err != nil {
		return nil, err
	}
	addr, encryptionKey := derive.CheckoutKeys(linkKey)
	event, err := nostr.Seal(nostr.SealOptions{
		Type:          proto.CheckoutRecordType,
		Value:         value,
		Signer:        secret,
		EncryptionKey: encryptionKey,
		Address:       addr,
		CreatedAt:     createdAt,
		Tags:          [][]string{{"expiration", strconv.FormatInt(expiresAt+proto.CheckoutLifeSeconds, 10)}},
		Random:        random,
	})
	if err != nil {
		return nil, err
	}
	author, err := secp.PublicKey(secret)
	if err != nil {
		return nil, err
	}
	return &Link{URL: URL(origin, author, linkKey, relays), Event: event, SecretKey: secret}, nil
}

// The statuses of a fragment.
const (
	FragmentLink    = "link"
	FragmentNewer   = "newer"
	FragmentInvalid = "invalid"
	FragmentOther   = "other"
)

// Fragment is what a URL's fragment names (SPEC.md §30, "Reading the fragment").
type Fragment struct {
	Status  string
	Author  string
	LinkKey []byte
	Relays  []string
}

var markerPattern = regexp.MustCompile(`^[a-z0-9]{1,8}$`)

// ParseFragment reads the link a URL's fragment (with or without "#") names.
func ParseFragment(fragment string) Fragment {
	parts := strings.Split(strings.TrimPrefix(fragment, "#"), ".")
	if parts[0] != proto.CheckoutMarker {
		if markerPattern.MatchString(parts[0]) {
			return Fragment{Status: FragmentNewer}
		}
		return Fragment{Status: FragmentOther}
	}
	if len(parts) != 4 {
		return Fragment{Status: FragmentInvalid}
	}
	author, okAuthor := b64.DecodeURL(parts[1])
	linkKey, okKey := b64.DecodeURL(parts[2])
	relays, okRelays := urls.DecodeRelayList(parts[3])
	if !okAuthor || !okKey || !okRelays || len(author) != proto.KeyBytes || len(linkKey) != proto.KeyBytes {
		return Fragment{Status: FragmentInvalid}
	}
	if !secp.IsPublicKeyBytes(author) {
		return Fragment{Status: FragmentInvalid}
	}
	return Fragment{Status: FragmentLink, Author: hex.EncodeToString(author), LinkKey: linkKey, Relays: relays}
}

// Record is a link's record, checked: what the page shows.
type Record struct {
	BusinessName string
	// Invoice, PaymentHash and VerifyURL are "" for an order paid on-chain only.
	Invoice     string
	PaymentHash string
	VerifyURL   string
	// OnchainAddress is "" without one (a record of version 1).
	OnchainAddress string
	// Sats is the invoice's amount, and what the address must receive.
	Sats      int64
	CreatedAt int64
	ExpiresAt int64
	Fiat      *declaration.Price
	Note      string
	// RedirectURL and RedirectShown (what the page shows of it) are "" without one.
	RedirectURL   string
	RedirectShown string
	// Rest is the rest author and rest key of a record of version 2 that carries them, else nil.
	Rest *Rest
	// BitcoinNode is the merchant's Bitcoin node the page reads the chain from, with its
	// fallback: of a record of version 2 whose bitcoinNode is valid; nil for mempool.space (none,
	// one set aside, or a record of version 1, which ignores it).
	BitcoinNode *bitcoinnode.RecordNode
}

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// parsePrice reads a record's fiat: null (nil, true), or a price; false when invalid.
func parsePrice(v any, present bool) (*declaration.Price, bool) {
	if !present {
		return nil, false
	}
	if v == nil {
		return nil, true
	}
	o, ok := jsonv.AsObject(v)
	if !ok {
		return nil, false
	}
	minorValue, _ := o.Get("minor")
	digitsValue, _ := o.Get("digits")
	currencyValue, _ := o.Get("currency")
	minor, ok := jsonv.AsSafeInteger(minorValue)
	if !ok || minor < 1 {
		return nil, false
	}
	digits, ok := jsonv.AsNumber(digitsValue)
	if !ok || !jsonv.IsInteger(digitsValue) || digits < 0 || digits > proto.MaxDeclaredDigits {
		return nil, false
	}
	currency, ok := currencyValue.(string)
	if !ok || !currencyPattern.MatchString(currency) || currency == proto.SatsCurrency {
		return nil, false
	}
	return &declaration.Price{Minor: minor, Digits: int(digits), Currency: currency}, true
}

// ParseValue reads a record's value as the page does (SPEC.md §30, "The record"): version 1 or
// 2. nostr.ErrNewer for a newer version, nostr.ErrInvalid when it is not valid.
func ParseValue(value any) (*Record, error) {
	v, ok := jsonv.AsObject(value)
	if !ok {
		return nil, nostr.ErrInvalid
	}
	version, _ := v.Get("version")
	if n, ok := version.(float64); ok && n > proto.CheckoutOnchainVersion {
		return nil, nostr.ErrNewer
	}
	if version != float64(proto.CheckoutVersion) && version != float64(proto.CheckoutOnchainVersion) {
		return nil, nostr.ErrInvalid
	}
	onchain := version == float64(proto.CheckoutOnchainVersion)
	businessNameValue, _ := v.Get("businessName")
	businessName, ok := businessNameValue.(string)
	if !ok || !text.IsWellFormed(businessName) || text.TrimASCII(businessName) == "" || text.CodePoints(businessName) > proto.MaxBusinessNameLength {
		return nil, nostr.ErrInvalid
	}
	record := &Record{BusinessName: businessName}
	invoiceValue, hasInvoice := v.Get("invoice")
	verifyValue, hasVerify := v.Get("verifyUrl")
	// On-chain only when both are null (absent is not null).
	lightning := !onchain || !hasInvoice || invoiceValue != nil || !hasVerify || verifyValue != nil
	if lightning {
		invoice, ok := invoiceValue.(string)
		if !ok {
			return nil, nostr.ErrInvalid
		}
		decoded, ok := bolt11.Decode(invoice)
		if !ok || decoded.Network != "bc" || !decoded.HasAmount || decoded.AmountMsat <= 0 || decoded.AmountMsat%1000 != 0 {
			return nil, nostr.ErrInvalid
		}
		verifyURL, ok := verifyValue.(string)
		if _, isURL := urls.ParseHTTPS(verifyURL); !ok || !isURL {
			return nil, nostr.ErrInvalid
		}
		record.Invoice, record.PaymentHash, record.VerifyURL = invoice, decoded.PaymentHash, verifyURL
		record.Sats, record.CreatedAt, record.ExpiresAt = decoded.AmountMsat/1000, decoded.Timestamp, decoded.Timestamp+decoded.ExpirySeconds
	}
	if onchain {
		addr, _ := v.Get("onchainAddress")
		if !address.IsDeposit(addr) {
			return nil, nostr.ErrInvalid
		}
		satsValue, _ := v.Get("sats")
		sats, ok := jsonv.AsSafeInteger(satsValue)
		if !ok || sats < 1 || sats > proto.MaxSats {
			return nil, nostr.ErrInvalid
		}
		createdAtValue, _ := v.Get("createdAt")
		expiresAtValue, _ := v.Get("expiresAt")
		createdAt, ok1 := jsonv.AsSafeInteger(createdAtValue)
		expiresAt, ok2 := jsonv.AsSafeInteger(expiresAtValue)
		if !ok1 || !ok2 || createdAt >= expiresAt {
			return nil, nostr.ErrInvalid
		}
		if lightning && (record.Sats != sats || record.CreatedAt != createdAt || record.ExpiresAt != expiresAt) {
			return nil, nostr.ErrInvalid
		}
		record.OnchainAddress = addr.(string)
		record.Sats, record.CreatedAt, record.ExpiresAt = sats, createdAt, expiresAt
		restAuthor, hasAuthor := v.Get("restAuthor")
		restKey, hasKey := v.Get("restKey")
		if hasAuthor || hasKey {
			if !secp.IsPublicKey(restAuthor) || !text.IsHex32Value(restKey) {
				return nil, nostr.ErrInvalid
			}
			key, _ := hex.DecodeString(restKey.(string))
			record.Rest = &Rest{Author: restAuthor.(string), Key: key}
		}
		// Any other bitcoinNode (null included) is set aside, and leaves the record valid.
		node, _ := v.Get("bitcoinNode")
		record.BitcoinNode = bitcoinnode.OfRecord(node)
	}
	fiatValue, hasFiat := v.Get("fiat")
	fiat, ok := parsePrice(fiatValue, hasFiat)
	if !ok {
		return nil, nostr.ErrInvalid
	}
	record.Fiat = fiat
	noteValue, _ := v.Get("note")
	note, ok := noteValue.(string)
	if !ok || !text.IsWellFormed(note) || text.HasControl(note) || text.CodePoints(note) < 1 || text.CodePoints(note) > proto.MaxNoteLength {
		return nil, nostr.ErrInvalid
	}
	record.Note = note
	redirectValue, hasRedirect := v.Get("redirectUrl")
	if !hasRedirect {
		return nil, nostr.ErrInvalid
	}
	if redirectValue != nil {
		redirect, ok := redirectValue.(string)
		if !ok {
			return nil, nostr.ErrInvalid
		}
		shown, ok := urls.RedirectShown(redirect)
		if !ok {
			return nil, nostr.ErrInvalid
		}
		record.RedirectURL, record.RedirectShown = redirect, shown
	}
	return record, nil
}

// Open reads what a relay sent as the record of a link: opened with the link encryption key, the
// link address and the author of the URL (SPEC.md §30, "Reading a record").
func Open(v any, author string, linkKey []byte) (nostr.Opened[*Record], bool) {
	addr, encryptionKey := derive.CheckoutKeys(linkKey)
	return nostr.Open(v, nostr.OpenOptions{Type: proto.CheckoutRecordType, EncryptionKey: encryptionKey, Address: addr, Author: author},
		func(value any, _ *nostr.Event) (*Record, error) { return ParseValue(value) })
}
