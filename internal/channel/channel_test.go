package channel

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/sattleme/checkout-go/internal/bitcoinnode"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/nostr"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// The app's channel (vectors/app/app.json) opens with the key alone, and reads as the package
// reads it.
func TestTheAppsChannel(t *testing.T) {
	app := tv.Load(t, "app/app.json")
	parsed := tv.Obj(t, app, "parsed")
	channel := tv.Obj(t, app, "channel")
	event := tv.Get(channel, "event")
	key, _ := hex.DecodeString(tv.Str(t, parsed, "integrationKey"))
	address, encryptionKey := derive.ChannelKeys(key)
	e, ok := nostr.Valid(event)
	if !ok || e.PubKey != tv.Str(t, parsed, "team") || len(e.Tags) != 1 || e.Tags[0][0] != "d" || e.Tags[0][1] != address {
		t.Fatalf("the channel's event: %+v", e)
	}
	plaintext, err := nip44.Decrypt(e.Content, encryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	opened, _ := jsonv.ParseString(plaintext)
	if string(jsonv.Marshal(opened)) != string(jsonv.Marshal(tv.Get(channel, "value"))) {
		t.Errorf("the plaintext is not the vector's value")
	}
	value := tv.Obj(t, tv.Obj(t, channel, "value"), "v")
	c, err := Parse(value, e.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	want := Channel{
		BusinessName:     "Sunrise Bakery",
		LightningAddress: "sattleme02c6047f94@breez.tips",
		RegistryKey:      tv.Str(t, value, "registryKey"),
		RegistryReader:   tv.Str(t, value, "registryReader"),
		Relays:           []string{"wss://relay.example"},
		// The app's channel of this vector names no web app: no checkout link; nor a tolerance
		// of on-chain underpayments: none.
		CheckoutOrigin: "",
		Tolerance:      0,
		UpdatedAt:      e.CreatedAt,
	}
	if c.BusinessName != want.BusinessName || c.LightningAddress != want.LightningAddress || c.RegistryKey != want.RegistryKey ||
		c.RegistryReader != want.RegistryReader || strings.Join(c.Relays, ",") != "wss://relay.example" || c.CheckoutOrigin != "" ||
		c.Tolerance != 0 || c.UpdatedAt != want.UpdatedAt || c.Removed {
		t.Errorf("%+v", c)
	}
	// opened with the key, through the package's own opening
	o, ok := nostr.Open(event, nostr.OpenOptions{Type: "team-channel", EncryptionKey: encryptionKey, Address: address, Author: e.PubKey},
		func(v any, ev *nostr.Event) (*Channel, error) { return Parse(v, ev.CreatedAt) })
	if !ok || o.Newer || o.Value.BusinessName != "Sunrise Bakery" {
		t.Errorf("open: %v", ok)
	}
}

func value(members ...any) *jsonv.Object {
	base := jsonv.O(
		"version", 1, "status", "active", "role", "integration",
		"profile", jsonv.O("businessName", "Sunrise Bakery", "lightningAddress", "a@breez.tips"),
		"registryKey", strings.Repeat("11", 32),
		"registryReader", "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
		"updatedAt", 100,
	)
	o, _ := jsonv.Parse(jsonv.Marshal(base))
	obj, _ := jsonv.AsObject(o)
	for i := 0; i < len(members); i += 2 {
		obj.Set(members[i].(string), members[i+1])
	}
	return obj
}

func TestChannelValues(t *testing.T) {
	if c, err := Parse(value(), 100); err != nil || c.Removed || c.Relays != nil {
		t.Fatalf("%+v, %v", c, err)
	}
	if c, err := Parse(value("status", "removed", "role", nil, "profile", nil), 100); err != nil || !c.Removed {
		t.Errorf("removed: %+v, %v", c, err)
	}
	for _, version := range []float64{2, 1.5} {
		if _, err := Parse(value("version", version), 100); !errors.Is(err, nostr.ErrNewer) {
			t.Errorf("version %v: %v", version, err)
		}
	}
	for name, v := range map[string]*jsonv.Object{
		"another date":              value(),
		"version 0":                 value("version", 0.0),
		"another role":              value("role", "pos"),
		"a business name of spaces": value("profile", jsonv.O("businessName", " \t", "lightningAddress", "a@breez.tips").Object()),
		"an invalid address":        value("profile", jsonv.O("businessName", "B", "lightningAddress", "a@localhost").Object()),
		"a reader off the curve":    value("registryReader", "eefdea4cdb677750a420fee807eacf21eb9898ae79b9768766e4faa04a2d4a34"),
		"a registry key in upper":   value("registryKey", strings.Repeat("AB", 32)),
		"an invalid relay list":     value("relays", []any{"wss://Relay.example"}),
		"an empty relay list":       value("relays", []any{}),
	} {
		createdAt := int64(100)
		if name == "another date" {
			createdAt = 101
		}
		if _, err := Parse(v, createdAt); !errors.Is(err, nostr.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A checkout field or a tolerance that is invalid gives none, and leaves the channel valid.
	c, err := Parse(value("checkout", "https://pay.example", "onchainTolerance", 1001.0), 100)
	if err != nil || c.CheckoutOrigin != "" || c.Tolerance != 0 {
		t.Errorf("%+v, %v", c, err)
	}
	c, err = Parse(value("checkout", jsonv.O("origin", "https://pay.example").Object(), "onchainTolerance", 500.0, "relays", []any{"wss://a.example", "wss://b.example"}), 100)
	if err != nil || c.CheckoutOrigin != "https://pay.example" || c.Tolerance != 500 || len(c.Relays) != 2 {
		t.Errorf("%+v, %v", c, err)
	}
	// The merchant's Bitcoin node: absent or invalid in any way, none, the channel valid.
	if c.BitcoinNode != nil {
		t.Errorf("a node: %+v", c.BitcoinNode)
	}
	for _, node := range []any{nil, "http://192.168.1.10:3006", jsonv.O("url", "http://node.example", "fallback", "mempool", "payers", false).Object(),
		jsonv.O("url", "http://192.168.1.10:3006", "fallback", "mempool", "payers", true).Object()} {
		c, err = Parse(value("bitcoinNode", node), 100)
		if err != nil || c.BitcoinNode != nil {
			t.Errorf("%v: %+v, %v", node, c, err)
		}
	}
	c, err = Parse(value("bitcoinNode", jsonv.O("url", "http://192.168.1.10:3006", "fallback", "none", "payers", false).Object()), 100)
	if err != nil || c.BitcoinNode == nil || *c.BitcoinNode != (bitcoinnode.Node{URL: "http://192.168.1.10:3006", Fallback: bitcoinnode.None}) {
		t.Errorf("%+v, %v", c, err)
	}
	// The app's own fields are ignored.
	if _, err := Parse(value("linkOwner", 1, "name", nil, "profile", jsonv.O("businessName", "B", "lightningAddress", "a@breez.tips", "logo", 5).Object()), 100); err != nil {
		t.Errorf("the app's fields: %v", err)
	}
}

// bitcoin-nodes.json's channels, through the reading of the channel (PORTING.md, step 4): an
// otherwise valid channel whose bitcoinNode is the case's value (absent when the case has none),
// valid either way, and the node it gives.
func TestChannelNodeVectors(t *testing.T) {
	channels := tv.Objs(t, tv.Load(t, "bitcoin-nodes.json"), "channels")
	for _, c := range channels {
		name := tv.Str(t, c, "name")
		v := value()
		if node, has := c.Get("value"); has {
			v.Set("bitcoinNode", node)
		}
		read, err := Parse(v, 100)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var got any
		if n := read.BitcoinNode; n != nil {
			got = jsonv.O("url", n.URL, "fallback", string(n.Fallback), "payers", n.Payers)
		}
		tv.Equal(t, name, got, tv.Get(c, "expected"))
	}
	t.Logf("%d channels", len(channels))
}
