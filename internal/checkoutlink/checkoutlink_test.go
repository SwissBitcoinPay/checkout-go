package checkoutlink

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/sattleme/checkout-go/internal/bitcoinnode"
	"github.com/sattleme/checkout-go/internal/derive"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/secp"
	tv "github.com/sattleme/checkout-go/internal/testvec"
	"github.com/sattleme/checkout-go/internal/urls"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func same(t *testing.T, name string, got, want any) {
	t.Helper()
	if g, w := string(jsonv.Marshal(got)), string(jsonv.Marshal(want)); g != w {
		t.Errorf("%s:\ngot  %s\nwant %s", name, g, w)
	}
}

func stringsOf(list []any) []string {
	out := make([]string, len(list))
	for i, v := range list {
		out[i] = v.(string)
	}
	return out
}

func TestCheckoutLinkVectors(t *testing.T) {
	file := tv.Load(t, "checkout-links.json")
	origins := tv.Objs(t, file, "origins")
	for _, c := range origins {
		got := any(OriginOf(tv.Get(c, "checkout")))
		if got == "" {
			got = nil
		}
		same(t, "origin of "+string(jsonv.Marshal(tv.Get(c, "checkout"))), got, tv.Get(c, "expected"))
	}
	derivations := tv.Objs(t, file, "derivations")
	for _, c := range derivations {
		address, encryptionKey := derive.CheckoutKeys(unhex(t, tv.Str(t, c, "linkKey")))
		same(t, "keys of "+tv.Str(t, c, "linkKey"), jsonv.O("address", address, "encryptionKey", hex.EncodeToString(encryptionKey)),
			jsonv.O("address", tv.Str(t, c, "address"), "encryptionKey", tv.Str(t, c, "encryptionKey")))
	}
	links := tv.Objs(t, file, "urls")
	for _, c := range links {
		got := URL(tv.Str(t, c, "origin"), tv.Str(t, c, "author"), unhex(t, tv.Str(t, c, "linkKey")), stringsOf(tv.Arr(t, c, "relays")))
		if got != tv.Str(t, c, "url") {
			t.Errorf("url: %s, want %s", got, tv.Str(t, c, "url"))
		}
	}
	fragments := tv.Objs(t, file, "fragments")
	for _, c := range fragments {
		read := ParseFragment(tv.Str(t, c, "fragment"))
		var got any = read.Status
		if read.Status == FragmentLink {
			relays := make([]any, len(read.Relays))
			for i, r := range read.Relays {
				relays[i] = r
			}
			got = jsonv.O("author", read.Author, "linkKey", hex.EncodeToString(read.LinkKey), "relays", relays)
		}
		same(t, "fragment: "+tv.Str(t, c, "name"), got, tv.Get(c, "expected"))
	}
	records := tv.Objs(t, file, "records")
	for _, c := range records {
		record, err := ParseValue(tv.Get(c, "value"))
		var got any
		switch {
		case errors.Is(err, nostr.ErrNewer):
			got = "newer"
		case err != nil:
			got = "invalid"
		default:
			var rest any
			if record.Rest != nil {
				rest = jsonv.O("author", record.Rest.Author, "key", hex.EncodeToString(record.Rest.Key))
			}
			got = jsonv.O("sats", strconv.FormatInt(record.Sats, 10), "paymentHash", orNull(record.PaymentHash),
				"onchainAddress", orNull(record.OnchainAddress), "expiresAt", record.ExpiresAt,
				"redirect", orNull(record.RedirectShown), "rest", rest)
		}
		same(t, "record: "+tv.Str(t, c, "name"), got, tv.Get(c, "expected"))
	}
	t.Logf("%d origins, %d derivations, %d urls, %d fragments, %d records", len(origins), len(derivations), len(links), len(fragments), len(records))
}

// A new link: its record opens with the key its URL's link key derives, signed by its author.
func TestANewLinkOpensWithItsURL(t *testing.T) {
	value := Value(Input{BusinessName: "Sunrise Bakery", Invoice: "lnbc1", VerifyURL: "https://breez.tips/v", Note: "Order 1042", RedirectURL: "https://shop.example/thanks"})
	same(t, "value", value, jsonv.O("version", 1, "businessName", "Sunrise Bakery", "invoice", "lnbc1", "verifyUrl", "https://breez.tips/v",
		"fiat", nil, "note", "Order 1042", "redirectUrl", "https://shop.example/thanks"))
	relays := []string{"wss://relay.example", "wss://nos.lol:4443"}
	link, err := New("https://pay.example", relays, value, 1791379179, 1791378281, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "https://pay.example/pay#"
	if len(link.URL) < len(prefix) || link.URL[:len(prefix)] != prefix {
		t.Fatalf("url %s", link.URL)
	}
	fragment := ParseFragment(link.URL[len(prefix)-1:])
	if fragment.Status != FragmentLink || fragment.Author != link.Event.PubKey || len(fragment.Relays) != 2 {
		t.Fatalf("fragment %+v", fragment)
	}
	address, encryptionKey := derive.CheckoutKeys(fragment.LinkKey)
	same(t, "tags", link.Event.Tags, [][]string{{"d", address}, {"expiration", strconv.Itoa(1791379179 + 604800)}})
	plaintext, err := nip44.Decrypt(link.Event.Content, encryptionKey)
	if err != nil || len(plaintext)%256 != 0 {
		t.Fatalf("%d bytes, %v", len(plaintext), err)
	}
	relayCopy, _ := jsonv.Parse(jsonv.Marshal(link.Event.JSON()))
	opened, ok := nostr.Open(relayCopy, nostr.OpenOptions{Type: "checkout", EncryptionKey: encryptionKey, Address: link.Event.Tags[0][1], Author: fragment.Author},
		func(v any, _ *nostr.Event) (any, error) { return v, nil })
	if !ok || opened.Newer {
		t.Fatal("does not open")
	}
	same(t, "opened", opened.Value, value)
	// Its secret is its author's: the key that signed its record.
	if author, err := secp.PublicKey(link.SecretKey); err != nil || author != link.Event.PubKey || author != fragment.Author {
		t.Errorf("the link's secret is not its author's: %v", err)
	}
}

// A link's parts: its fragment read back, and only the fragments of checkout links.
func TestALinksFragmentIsReadBack(t *testing.T) {
	author := secp.MustPublicKey(bytes.Repeat([]byte{7}, 32))
	linkKey := bytes.Repeat([]byte{8}, 32)
	url := URL("https://pay.example", author, linkKey, []string{"wss://relay.example", "wss://nos.lol"})
	fragment := ParseFragment(url[strings.IndexByte(url, '#'):])
	if fragment.Status != FragmentLink || fragment.Author != author || !bytes.Equal(fragment.LinkKey, linkKey) ||
		strings.Join(fragment.Relays, ",") != "wss://relay.example,wss://nos.lol" {
		t.Errorf("%+v", fragment)
	}
	for text, status := range map[string]string{
		"#d.a.b.c": FragmentNewer,
		"c2.a.b.c": FragmentNewer,
		"c.a.b":    FragmentInvalid,
		// A payment link of the point of sale: its author first, 43 characters.
		strings.Repeat("A", 43) + "." + strings.Repeat("B", 43): FragmentOther,
	} {
		if got := ParseFragment(text).Status; got != status {
			t.Errorf("%s: %s, want %s", text, got, status)
		}
	}
}

func TestWhatThePageShowsOfARedirectURL(t *testing.T) {
	for url, want := range map[string]string{
		"https://shop.example:8443/a?b#c":     "shop.example:8443",
		"myshop://orders/42":                  "myshop://",
		"https://shop.example\\@evil.example": "",
	} {
		if got, _ := urls.RedirectShown(url); got != want {
			t.Errorf("%s: %q, want %q", url, got, want)
		}
	}
}

// bitcoin-nodes.json's writes, through the making of a checkout link's record (Value, with the
// node the channel names as bitcoinnode.Of reads it and ForRecord copies it), and its records,
// through the reading of a record as a page reads it (ParseValue), valid in every case (PORTING.md,
// step 4).
func TestNodeRecordVectors(t *testing.T) {
	file := tv.Load(t, "bitcoin-nodes.json")
	writes := tv.Objs(t, file, "writes")
	for _, c := range writes {
		name := tv.Str(t, c, "name")
		var node *bitcoinnode.Node
		if v, has := c.Get("channel"); has {
			node = bitcoinnode.Of(v)
		}
		in := Input{BusinessName: "Sunrise Bakery", Invoice: "", Sats: 150_000_000, CreatedAt: 1_791_381_280, ExpiresAt: 1_791_382_180, Note: "Order 1042",
			BitcoinNode: bitcoinnode.ForRecord(node)}
		version := 1.0
		if tv.Get(c, "address") == true {
			in.OnchainAddress, version = "bc1p4gmh64rd4m5y5nn8hwfnvtz2mm6lngpmwys5a257klyg03ju0pyqu65xe8", 2
		}
		written, _ := jsonv.AsObject(mustParse(t, jsonv.Marshal(Value(in))))
		if tv.Get(written, "version") != version {
			t.Errorf("%s: version %v", name, tv.Get(written, "version"))
		}
		got, _ := written.Get("bitcoinNode")
		tv.Equal(t, "write: "+name, got, tv.Get(c, "expected"))
		// Absent, never null.
		if _, has := written.Get("bitcoinNode"); has && got == nil {
			t.Errorf("%s: null", name)
		}
	}
	records := tv.Objs(t, file, "records")
	for _, c := range records {
		name := tv.Str(t, c, "name")
		record, err := ParseValue(tv.Get(c, "value"))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var got any
		if n := record.BitcoinNode; n != nil {
			got = jsonv.O("url", n.URL, "fallback", string(n.Fallback))
		}
		tv.Equal(t, "record: "+name, got, tv.Get(c, "expected"))
	}
	t.Logf("%d writes, %d records", len(writes), len(records))
}

func mustParse(t *testing.T, data []byte) any {
	t.Helper()
	v, err := jsonv.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
