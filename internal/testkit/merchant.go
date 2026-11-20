package testkit

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sattleme/checkout-go/internal/b64"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/secp"
	"github.com/sattleme/checkout-go/internal/urls"
)

// A merchant for tests, as the app would set it up: a team, an integration's key, the channel the
// team publishes on local relays, the registry, and a Lightning address (breez.tips) answered by a
// fake HTTP transport that makes real-looking invoices.

const (
	// LightningAddress is the merchant's Lightning address.
	LightningAddress = "sattleme02c6047f94@breez.tips"
	// PayURL is its LUD-16 pay request; Callback its callback.
	PayURL   = "https://breez.tips/.well-known/lnurlp/sattleme02c6047f94"
	Callback = "https://breez.tips/lnurlp/sattleme02c6047f94/invoice"
	// VerifyPrefix is where its LUD-21 verify URLs are.
	VerifyPrefix = "https://breez.tips/lnurlp/verify/"
)

// Metadata is the pay request's metadata.
var Metadata = string(jsonv.Marshal([]any{[]any{"text/identifier", LightningAddress}, []any{"text/plain", "Pay to Sunrise Bakery"}}))

// Bytes is length bytes of seed: tests are reproducible.
func Bytes(seed byte, length int) []byte { return bytes.Repeat([]byte{seed}, length) }

// FormatKey writes an integration key, as the merchant's app does.
func FormatKey(team string, key []byte, relays []string) string {
	teamBytes, _ := hex.DecodeString(team)
	return proto.IntegrationKeyPrefix + b64.EncodeURL(teamBytes) + "." + b64.EncodeURL(key) + "." + urls.EncodeRelayList(relays)
}

// MadeInvoice is an invoice the fake breez.tips made, with what pays it.
type MadeInvoice struct {
	PR          string
	Verify      string
	PaymentHash string
	Preimage    string
	AmountMsat  int64
	Expiry      int64
	// Comment is the comment it was asked with (HasComment).
	Comment    string
	HasComment bool
}

// absent is the value of a member Merge removes.
type absent struct{}

// Absent removes a member in Merge.
var Absent any = absent{}

// Merge is base with the members of extra: a member of both keeps its place with extra's value,
// one of extra only comes last, one whose value is Absent is removed (as {...base, ...extra} in
// JavaScript, undefined values dropped).
func Merge(base jsonv.Members, extra ...jsonv.Member) jsonv.Members {
	out := append(jsonv.Members(nil), base...)
	for _, m := range extra {
		found := false
		for i := range out {
			if out[i].Name == m.Name {
				out[i].Value, found = m.Value, true
				break
			}
		}
		if !found {
			out = append(out, m)
		}
	}
	kept := out[:0]
	for _, m := range out {
		if m.Value != Absent {
			kept = append(kept, m)
		}
	}
	return kept
}

// Logger is a logger of a test: what it was told.
type Logger struct {
	mu       sync.Mutex
	messages []string
}

// Warn records message.
func (l *Logger) Warn(message string) {
	l.mu.Lock()
	l.messages = append(l.messages, message)
	l.mu.Unlock()
}

// Messages are the warnings so far.
func (l *Logger) Messages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.messages...)
}

// Has tells whether a warning holds part.
func (l *Logger) Has(part string) bool {
	for _, m := range l.Messages() {
		if strings.Contains(m, part) {
			return true
		}
	}
	return false
}

// Merchant is a merchant of a test.
type Merchant struct {
	Net            *Net
	TeamSecret     []byte
	Team           string
	IntegrationKey []byte
	RegistryKey    []byte
	ReaderSecret   []byte
	Reader         string
	KeyRelays      []string
	// Chain is the explorers and BlockCypher.
	Chain *FakeChain
	// HTTP is the transport the client is given: breez.tips, the chain, Override.
	HTTP *FakeHTTP

	// now is the test's clock, in Unix seconds: read by tampers while the fake answers.
	now      atomic.Int64
	mu       sync.Mutex
	invoices map[string]*MadeInvoice
	order    []string
	paid     map[string]bool
	pay      jsonv.Members
	tamper   func(*MadeInvoice)
	override func(req *http.Request) (Answer, bool)
	seed     int
}

// NewMerchant is a merchant whose key names the relays a, b and c (not started).
func NewMerchant() *Merchant {
	m := &Merchant{
		Net:            NewNet(),
		TeamSecret:     Bytes(1, 32),
		IntegrationKey: Bytes(2, 32),
		RegistryKey:    Bytes(3, 32),
		ReaderSecret:   Bytes(4, 32),
		KeyRelays:      []string{"wss://relay-a.test", "wss://relay-b.test", "wss://relay-c.test"},
		Chain:          NewFakeChain(),
		invoices:       map[string]*MadeInvoice{},
		paid:           map[string]bool{},
		seed:           10,
		pay: jsonv.O(
			"tag", "payRequest",
			"callback", Callback,
			"minSendable", 1000,
			"maxSendable", int64(100_000_000_000),
			"metadata", Metadata,
			"commentAllowed", 255,
		),
	}
	m.now.Store(1_791_378_000)
	m.Team = secp.MustPublicKey(m.TeamSecret)
	m.Reader = secp.MustPublicKey(m.ReaderSecret)
	m.HTTP = NewFakeHTTP(m.route)
	return m
}

// Key is the integration key's text.
func (m *Merchant) Key() string { return FormatKey(m.Team, m.IntegrationKey, m.KeyRelays) }

// Start starts the key's relays and others, each with its behaviour.
func (m *Merchant) Start(others []string, behaviours map[string]Behaviour) *Merchant {
	seen := map[string]bool{}
	for _, url := range append(append([]string(nil), m.KeyRelays...), others...) {
		if seen[url] {
			continue
		}
		seen[url] = true
		m.Net.Add(url, behaviours[url])
	}
	return m
}

// Stop stops the network.
func (m *Merchant) Stop() { m.Net.Close() }

// Now is the test's clock, in Unix seconds.
func (m *Merchant) Now() int64 { return m.now.Load() }

// SetNow sets the test's clock, in Unix seconds.
func (m *Merchant) SetNow(now int64) { m.now.Store(now) }

// Clock is the test's clock, for the client.
func (m *Merchant) Clock() func() time.Time {
	return func() time.Time { return time.Unix(m.Now(), 0) }
}

// SetPay changes members of what breez.tips's pay request says.
func (m *Merchant) SetPay(members ...jsonv.Member) {
	m.mu.Lock()
	m.pay = Merge(m.pay, members...)
	m.mu.Unlock()
}

// SetTamper changes the invoices breez.tips gives (a test of a check); nil for none.
func (m *Merchant) SetTamper(f func(*MadeInvoice)) {
	m.mu.Lock()
	m.tamper = f
	m.mu.Unlock()
}

// SetOverride answers the requests it handles itself; nil for none.
func (m *Merchant) SetOverride(f func(req *http.Request) (Answer, bool)) {
	m.mu.Lock()
	m.override = f
	m.mu.Unlock()
}

// PayInvoice marks an invoice paid on breez.tips.
func (m *Merchant) PayInvoice(paymentHash string) {
	m.mu.Lock()
	m.paid[paymentHash] = true
	m.mu.Unlock()
}

// Invoice is the invoice breez.tips made of a payment hash.
func (m *Merchant) Invoice(paymentHash string) *MadeInvoice {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.invoices[paymentHash]
}

// Invoices are the invoices breez.tips made, in order.
func (m *Merchant) Invoices() []*MadeInvoice {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*MadeInvoice, len(m.order))
	for i, hash := range m.order {
		out[i] = m.invoices[hash]
	}
	return out
}

// ChannelValue is the channel's value, active, as the merchant's app writes it, with extra.
func (m *Merchant) ChannelValue(createdAt int64, extra ...jsonv.Member) jsonv.Members {
	return Merge(jsonv.O(
		"version", 1,
		"status", "active",
		"name", "Shopify store",
		"role", "integration",
		"profile", jsonv.O(
			"businessName", "Sunrise Bakery",
			"lightningAddress", LightningAddress,
			"currency", "CHF",
			"onchain", true,
			"logo", nil,
		),
		"registryKey", hex.EncodeToString(m.RegistryKey),
		"registryReader", m.Reader,
		"linkOwner", strings.Repeat("ab", 32),
		"updatedAt", createdAt,
	), extra...)
}

// ChannelEvent is the channel's event, signed by signer (the team's secret when nil).
func (m *Merchant) ChannelEvent(value any, createdAt int64, signer []byte) *nostr.Event {
	if signer == nil {
		signer = m.TeamSecret
	}
	address, encryptionKey := derive.ChannelKeys(m.IntegrationKey)
	e, err := nostr.Seal(nostr.SealOptions{
		Type:          proto.ChannelRecordType,
		Value:         value,
		Signer:        signer,
		EncryptionKey: encryptionKey,
		Address:       address,
		CreatedAt:     createdAt,
		Random:        rand.Reader,
	})
	if err != nil {
		panic(err)
	}
	return e
}

// PublishChannel publishes the channel on relays (the key's when none).
func (m *Merchant) PublishChannel(value any, createdAt int64, relays ...string) *nostr.Event {
	e := m.ChannelEvent(value, createdAt, nil)
	m.store(e, relays)
	return e
}

func (m *Merchant) store(e *nostr.Event, relays []string) {
	if len(relays) == 0 {
		relays = m.KeyRelays
	}
	for _, url := range relays {
		m.Net.Get(url).StoreEvent(e)
	}
}

// LotEvent is the integration's lot of deposit addresses, as the merchant's app writes it.
func (m *Merchant) LotEvent(addresses []string, createdAt int64) *nostr.Event {
	_, encryptionKey := derive.ChannelKeys(m.IntegrationKey)
	e, err := nostr.Seal(nostr.SealOptions{
		Type:          proto.LotRecordType,
		Value:         jsonv.O("version", 1, "addresses", addresses, "updatedAt", createdAt),
		Signer:        m.TeamSecret,
		EncryptionKey: encryptionKey,
		Address:       derive.LotAddress(m.IntegrationKey),
		CreatedAt:     createdAt,
		Random:        rand.Reader,
	})
	if err != nil {
		panic(err)
	}
	return e
}

// PublishLot publishes the lot on relays (the key's when none), as the merchant's app does.
func (m *Merchant) PublishLot(addresses []string, createdAt int64, relays ...string) *nostr.Event {
	e := m.LotEvent(addresses, createdAt)
	m.store(e, relays)
	return e
}

// Registry is the merchant's registry.
func (m *Merchant) Registry() derive.Registry {
	return derive.Registry{Key: m.RegistryKey, Reader: m.Reader}
}

// Sale is the sale of the order ref.
func (m *Merchant) Sale(ref string) *derive.Sale {
	sale, err := derive.SaleOf(m.IntegrationKey, m.Registry(), ref)
	if err != nil {
		panic(err)
	}
	return sale
}

func (m *Merchant) route(req *http.Request) Answer {
	m.mu.Lock()
	override := m.override
	m.mu.Unlock()
	if override != nil {
		if a, ok := override(req); ok {
			return a
		}
	}
	url := req.URL.String()
	if a, ok := m.Chain.Route(url); ok {
		return a
	}
	if req.URL.Scheme+"://"+req.URL.Host+req.URL.Path == Callback {
		return m.makeInvoice(req)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if url == PayURL {
		return JSON(append(jsonv.Members(nil), m.pay...))
	}
	if strings.HasPrefix(url, VerifyPrefix) {
		hash := req.URL.Path[strings.LastIndexByte(req.URL.Path, '/')+1:]
		made := m.invoices[hash]
		if made == nil {
			return JSON(jsonv.O("status", "ERROR", "reason", "Not found"))
		}
		settled := m.paid[hash]
		var preimage any
		if settled {
			preimage = made.Preimage
		}
		return JSON(jsonv.O("status", "OK", "settled", settled, "preimage", preimage, "pr", made.PR))
	}
	return Answer{Err: errors.New("testkit: fetch failed: " + req.URL.Host)}
}

// makeInvoice answers the callback: a new invoice of the amount asked, as the tamper changes it.
func (m *Merchant) makeInvoice(req *http.Request) Answer {
	query := req.URL.Query()
	amount, _ := strconv.ParseInt(query.Get("amount"), 10, 64)
	expiry := int64(3600)
	if query.Has("expiry") {
		expiry, _ = strconv.ParseInt(query.Get("expiry"), 10, 64)
	}
	m.mu.Lock()
	seed, tamper := m.seed, m.tamper
	m.seed++
	m.mu.Unlock()
	preimage, hash := PreimagePair(seed)
	made := &MadeInvoice{
		PR: MakeInvoice(Invoice{AmountMsat: amount, HasAmount: true, PaymentHash: hash, DescriptionHash: MetadataHash(Metadata),
			Timestamp: m.Now(), ExpirySeconds: expiry, HasExpiry: true}),
		Verify:      VerifyPrefix + hash,
		PaymentHash: hash,
		Preimage:    preimage,
		AmountMsat:  amount,
		Expiry:      expiry,
		Comment:     query.Get("comment"),
		HasComment:  query.Has("comment"),
	}
	// Outside the lock: a tamper may pay the invoice, or read the merchant's state.
	if tamper != nil {
		tamper(made)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, known := m.invoices[made.PaymentHash]; !known {
		m.order = append(m.order, made.PaymentHash)
	}
	m.invoices[made.PaymentHash] = made
	return JSON(jsonv.O("pr", made.PR, "verify", made.Verify, "routes", []any{}))
}
