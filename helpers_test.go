package checkout_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/testkit"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// The integration tests: the client against local relays and fakes of breez.tips, of the rate
// sources, of the explorers (REST and WebSocket) and of BlockCypher, reached through the injected
// HTTP transport and WebSocket dialer, with their real host names. Never the network.

// quiet is a logger that keeps what it is told.
func quiet() *testkit.Logger { return &testkit.Logger{} }

// newMerchant is a merchant whose key's relays are started, its channel published at 100 naming
// them.
func newMerchant(t *testing.T) *testkit.Merchant {
	t.Helper()
	m := testkit.NewMerchant()
	m.Start(nil, nil)
	t.Cleanup(m.Stop)
	m.PublishChannel(m.ChannelValue(100, jsonv.Member{Name: "relays", Value: m.KeyRelays}), 100)
	return m
}

// clientOf is a client of the merchant: its transport, its relays, its clock; relay requests of
// 1 s each.
func clientOf(t *testing.T, m *testkit.Merchant, opts ...checkout.Option) *checkout.Client {
	t.Helper()
	base := []checkout.Option{
		checkout.WithHTTPTransport(m.HTTP),
		checkout.WithWebSocketDialer(checkout.NewWebSocketDialer(m.Net.Transport())),
		checkout.WithClock(m.Clock()),
		checkout.WithRelayTimeout(time.Second),
		checkout.WithLogger(quiet()),
	}
	c, err := checkout.New(m.Key(), append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// errorOf is err as an Error of the package (fatal otherwise).
func errorOf(t *testing.T, err error) *checkout.Error {
	t.Helper()
	var e *checkout.Error
	if !errors.As(err, &e) {
		t.Fatalf("not an Error of the package: %v", err)
	}
	return e
}

// wantCode fails unless err is an Error of the package with code (and param, when given).
func wantCode(t *testing.T, err error, code checkout.Code, param ...string) *checkout.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s", code)
	}
	e := errorOf(t, err)
	if e.Code != code {
		t.Fatalf("got %s (%s), want %s", e.Code, e.Message, code)
	}
	if len(param) > 0 && e.Param != param[0] {
		t.Fatalf("param %q, want %q", e.Param, param[0])
	}
	return e
}

// must fails on err.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// jsonOf is v's JSON (MarshalJSON), read back as jsonv reads it.
func jsonOf(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jsonv.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// declarations are every declaration (kind 30078 records of the registry) the key's relays
// received, in the order of the relays.
func declarations(m *testkit.Merchant) []*nostr.Event {
	var events []*nostr.Event
	for _, url := range m.KeyRelays {
		for _, e := range m.Net.Get(url).Received() {
			if e.Kind == proto.RecordKind && isDeclaration(m, e) {
				events = append(events, e)
			}
		}
	}
	return events
}

func isDeclaration(m *testkit.Merchant, e *nostr.Event) bool {
	_, err := testkit.OpenAsOwner(e, m.RegistryKey, m.ReaderSecret)
	return err == nil
}

// same fails unless got and want are the same JSON, whatever the order of their members.
func same(t *testing.T, name string, got, want any) {
	t.Helper()
	tv.Equal(t, name, got, want)
}

// readsOf makes a relay that never answers a read of the records of author (a sale's
// declarations and leases): a relay late for that read whatever the machine's load, which still
// takes every write and answers every other read. A read that goes on at a majority goes on
// without it; a read on every relay waits for it one relay timeout.
func readsOf(author string) func(filter *jsonv.Object) bool {
	return func(filter *jsonv.Object) bool {
		authors, _ := filter.Get("authors")
		list, _ := authors.([]any)
		for _, a := range list {
			if a == author {
				return true
			}
		}
		return false
	}
}

// completeReads tells a read of leases (a complete read: its filter has a limit) from the others:
// a relay muted for them never answers one, and answers every other read.
func completeReads(filter *jsonv.Object) bool { return filter.Has("limit") }
