package declaration

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/sattleme/checkout-go/internal/bolt11"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/nostr"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type vectors struct {
	app, parsed, derived, channel, declaration, real *jsonv.Object
	integrationKey                                   []byte
	registry                                         derive.Registry
}

func load(t *testing.T) vectors {
	app := tv.Load(t, "app/app.json")
	v := vectors{app: app, parsed: tv.Obj(t, app, "parsed"), derived: tv.Obj(t, app, "derived"), channel: tv.Obj(t, app, "channel"),
		declaration: tv.Obj(t, app, "declaration"), real: tv.Obj(t, app, "realInvoice")}
	v.integrationKey = unhex(t, tv.Str(t, v.parsed, "integrationKey"))
	channelValue := tv.Obj(t, tv.Obj(t, v.channel, "value"), "v")
	v.registry = RegistryOf(tv.Str(t, channelValue, "registryKey"), tv.Str(t, channelValue, "registryReader"))
	return v
}

func sameJSON(t *testing.T, name string, got, want any) {
	t.Helper()
	if g, w := string(jsonv.Marshal(got)), string(jsonv.Marshal(want)); g != w {
		t.Errorf("%s:\ngot  %s\nwant %s", name, g, w)
	}
}

// Every value of vectors/app/app.json the derivations give (SPEC.md §10).
func TestTheAppsVectorsGiveTheKeys(t *testing.T) {
	v := load(t)
	address, encryptionKey := derive.ChannelKeys(v.integrationKey)
	sameJSON(t, "derived", jsonv.O(
		"channelAddress", address,
		"channelEncryptionKey", hex.EncodeToString(encryptionKey),
		"takerKey", hex.EncodeToString(derive.TakerKey(v.integrationKey)),
	), v.derived)

	d := v.declaration
	key, secret, author := derive.SaleKeys(v.integrationKey, tv.Str(t, d, "ref"))
	for name, got := range map[string]string{
		"saleKey":    hex.EncodeToString(key),
		"saleSecret": hex.EncodeToString(secret),
		"saleAuthor": author,
		"taker":      derive.TakerTag(derive.TakerKey(v.integrationKey), author),
		"address":    derive.RegistryAddress(v.registry.Key, tv.Str(t, d, "paymentHash")),
		"outerKey":   hex.EncodeToString(derive.OuterKey(v.registry.Key, tv.Str(t, d, "address"), author)),
	} {
		if got != tv.Str(t, d, name) {
			t.Errorf("%s: %s", name, got)
		}
	}
	inner, err := nip44.ConversationKey(secret, unhex(t, v.registry.Reader))
	if err != nil || hex.EncodeToString(inner) != tv.Str(t, d, "innerConversationKey") {
		t.Errorf("innerConversationKey: %x, %v", inner, err)
	}
	sale, err := derive.SaleOf(v.integrationKey, v.registry, tv.Str(t, d, "ref"))
	if err != nil || !bytes.Equal(sale.InnerKey, inner) || sale.Taker != tv.Str(t, d, "taker") {
		t.Errorf("SaleOf: %v", err)
	}
}

func TestTheAppsRealInvoiceDecodes(t *testing.T) {
	v := load(t)
	decoded, ok := bolt11.Decode(tv.Str(t, v.real, "invoice"))
	if !ok {
		t.Fatal("does not decode")
	}
	sameJSON(t, "realInvoice", jsonv.O(
		"network", decoded.Network, "amountMsat", jsonv.NumberString(float64(decoded.AmountMsat)),
		"timestamp", decoded.Timestamp, "expirySeconds", decoded.ExpirySeconds,
		"paymentHash", decoded.PaymentHash, "descriptionHash", decoded.DescriptionHash,
	), jsonv.O(
		"network", "bc", "amountMsat", tv.Str(t, v.real, "amountMsat"),
		"timestamp", tv.Int(t, v.real, "timestamp"), "expirySeconds", tv.Int(t, v.real, "expirySeconds"),
		"paymentHash", tv.Str(t, v.real, "paymentHash"), "descriptionHash", tv.Str(t, v.real, "descriptionHash"),
	))
}

// The app's declaration opens as its sale's (SPEC.md §20, "Opening"), and as no other sale's.
func TestTheAppsDeclarationOpens(t *testing.T) {
	v := load(t)
	d := v.declaration
	event := tv.Get(d, "event")
	if _, ok := nostr.Valid(event); !ok {
		t.Fatal("the vector's event does not check out")
	}
	content, _ := jsonv.Field(event, "content")
	outside, err := nip44.Decrypt(content.(string), unhex(t, tv.Str(t, d, "outerKey")))
	if err != nil || int64(len(outside)) != tv.Int(t, d, "outsideBytes") {
		t.Errorf("outside: %d bytes, %v", len(outside), err)
	}
	sale, _ := derive.SaleOf(v.integrationKey, v.registry, tv.Str(t, d, "ref"))
	opened, newer, ok := Open(event, sale, nil)
	if !ok || newer {
		t.Fatalf("not opened (newer %v)", newer)
	}
	fiat := tv.Obj(t, d, "fiat")
	if opened.Invoice != tv.Str(t, d, "invoice") || opened.VerifyURL != tv.Str(t, d, "verifyUrl") ||
		opened.Note != tv.Str(t, d, "note") || opened.Rest != nil || opened.DeclaredAt != opened.Event.CreatedAt ||
		opened.Price != (Price{Minor: tv.Int(t, fiat, "minor"), Digits: int(tv.Int(t, fiat, "digits")), Currency: tv.Str(t, fiat, "currency")}) ||
		opened.Decoded.PaymentHash != tv.Str(t, d, "paymentHash") {
		t.Errorf("opened %+v", opened)
	}
	other, _ := derive.SaleOf(v.integrationKey, v.registry, "ORD-1043")
	if _, _, ok := Open(event, other, nil); ok {
		t.Error("another order's sale opens it")
	}
}

// A declaration the package makes opens with the vector's keys, holding what the app's holds.
func TestADeclarationThePackageMakes(t *testing.T) {
	v := load(t)
	d := v.declaration
	sale, _ := derive.SaleOf(v.integrationKey, v.registry, tv.Str(t, d, "ref"))
	fiat := tv.Obj(t, d, "fiat")
	appEvent := tv.Get(d, "event")
	createdAt, _ := jsonv.Field(appEvent, "created_at")
	event, err := Event(Input{
		Sale: sale, Invoice: tv.Str(t, d, "invoice"), VerifyURL: tv.Str(t, d, "verifyUrl"), PaymentHash: tv.Str(t, d, "paymentHash"),
		Price: Price{Minor: tv.Int(t, fiat, "minor"), Digits: int(tv.Int(t, fiat, "digits")), Currency: tv.Str(t, fiat, "currency")},
		Note:  tv.Str(t, d, "note"), CreatedAt: int64(createdAt.(float64)), Random: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	relayCopy, _ := jsonv.Parse(jsonv.Marshal(event.JSON()))
	if _, ok := nostr.Valid(relayCopy); !ok || event.Kind != 30078 || event.PubKey != tv.Str(t, d, "saleAuthor") {
		t.Error("not a valid event of the sale")
	}
	sameJSON(t, "tags", event.Tags, [][]string{{"d", tv.Str(t, d, "address")}})
	outerKey, innerKey := unhex(t, tv.Str(t, d, "outerKey")), unhex(t, tv.Str(t, d, "innerConversationKey"))
	inside := func(content string) any {
		outside, err := nip44.Decrypt(content, outerKey)
		if err != nil || len(outside)%256 != 0 {
			t.Fatalf("outside: %d bytes, %v", len(outside), err)
		}
		envelope, _ := jsonv.ParseString(outside)
		if tt, _ := jsonv.Field(envelope, "t"); tt != "registry-declaration" {
			t.Errorf("type %v", tt)
		}
		value, _ := jsonv.Field(envelope, "v")
		sealed, _ := jsonv.Field(value, "sealed")
		plaintext, err := nip44.Decrypt(sealed.(string), innerKey)
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := jsonv.ParseString(plaintext)
		return parsed
	}
	appContent, _ := jsonv.Field(appEvent, "content")
	ours := inside(event.Content)
	// The same declaration, field for field, as the vector's (which the app made).
	sameJSON(t, "inside", ours, inside(appContent.(string)))
	sameJSON(t, "inside", ours, jsonv.O(
		"version", 1, "invoice", tv.Str(t, d, "invoice"), "verifyUrl", tv.Str(t, d, "verifyUrl"),
		"sale", jsonv.O("key", tv.Str(t, d, "saleKey"), "fiat", fiat, "note", tv.Str(t, d, "note")),
		"taker", tv.Str(t, d, "taker"),
	))
	if _, _, ok := Open(relayCopy, sale, nil); !ok {
		t.Error("our declaration does not open")
	}
	// Refused with another registry key or reader.
	otherKey := v.registry
	otherKey.Key = bytes.Repeat([]byte{9}, 32)
	saleOtherKey, _ := derive.SaleOf(v.integrationKey, otherKey, tv.Str(t, d, "ref"))
	otherReader := v.registry
	otherReader.Reader = tv.Str(t, v.parsed, "team")
	saleOtherReader, _ := derive.SaleOf(v.integrationKey, otherReader, tv.Str(t, d, "ref"))
	for _, s := range []*derive.Sale{saleOtherKey, saleOtherReader} {
		if _, _, ok := Open(relayCopy, s, nil); ok {
			t.Error("opened with another registry")
		}
	}
}

func TestARestInvoicesDeclaration(t *testing.T) {
	v := load(t)
	d := v.declaration
	sale, _ := derive.SaleOf(v.integrationKey, v.registry, tv.Str(t, d, "ref"))
	rest := &RestOf{Address: "bc1px22v5j99gdwhxqyelg2p6tdcfajjd3njhzn22c2gura2dvxg2xlqw2wte4", ExpiresAt: 1791379180}
	in := Input{Sale: sale, Invoice: tv.Str(t, d, "invoice"), VerifyURL: tv.Str(t, d, "verifyUrl"), PaymentHash: tv.Str(t, d, "paymentHash"),
		Price: Price{Minor: 21, Digits: 0, Currency: "SAT"}, Note: "ORD-1042", CreatedAt: 1791378281, Rest: rest, Random: rand.Reader}
	sameJSON(t, "inside", Inside(in), jsonv.O(
		"version", 1, "invoice", in.Invoice, "verifyUrl", in.VerifyURL,
		"sale", jsonv.O("key", hex.EncodeToString(sale.Key), "fiat", jsonv.O("minor", 21, "digits", 0, "currency", "SAT"), "note", "ORD-1042"),
		"taker", sale.Taker, "rest", jsonv.O("address", rest.Address, "expiresAt", rest.ExpiresAt),
	))
	event, err := Event(in)
	if err != nil {
		t.Fatal(err)
	}
	relayCopy, _ := jsonv.Parse(jsonv.Marshal(event.JSON()))
	opened, _, ok := Open(relayCopy, sale, nil)
	if !ok || opened.Rest == nil || *opened.Rest != *rest {
		t.Errorf("rest: %+v", opened)
	}
}
