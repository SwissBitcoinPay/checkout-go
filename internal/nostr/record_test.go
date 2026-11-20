package nostr

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/nip44"
	"github.com/sattleme/checkout-go/internal/secp"
)

var (
	signer        = bytes.Repeat([]byte{3}, 32)
	encryptionKey = bytes.Repeat([]byte{4}, 32)
	address       = strings.Repeat("ab", 32)
)

const createdAt = 1_791_378_000

func author(t *testing.T) string {
	t.Helper()
	pub, err := secp.PublicKey(signer)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

// relayCopy is an event as a relay sends it: JSON, read back.
func relayCopy(t *testing.T, e *Event) any {
	t.Helper()
	v, err := jsonv.Parse(jsonv.Marshal(e.JSON()))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func seal(t *testing.T, value any) *Event {
	t.Helper()
	e, err := Seal(SealOptions{Type: "t", Value: value, Signer: signer, EncryptionKey: encryptionKey, Address: address, CreatedAt: createdAt, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func open(t *testing.T, v any) (Opened[any], bool) {
	t.Helper()
	return Open(v, OpenOptions{Type: "t", EncryptionKey: encryptionKey, Address: address, Author: author(t)},
		func(value any, _ *Event) (any, error) { return value, nil })
}

func TestSealThenOpen(t *testing.T) {
	e := seal(t, jsonv.O("a", 1))
	plaintext, err := nip44.Decrypt(e.Content, encryptionKey)
	if err != nil || len(plaintext)%256 != 0 || !strings.HasPrefix(plaintext, `{"t":"t","v":{"a":1}}`) || strings.TrimRight(plaintext, " ") != `{"t":"t","v":{"a":1}}` {
		t.Errorf("plaintext %q, %v", plaintext, err)
	}
	opened, ok := open(t, relayCopy(t, e))
	if !ok || opened.Newer || string(jsonv.Marshal(opened.Value)) != `{"a":1}` || opened.Event.ID != e.ID {
		t.Errorf("opened %+v, %v", opened, ok)
	}
	// Of another type, address or author: not opened.
	for name, o := range map[string]OpenOptions{
		"another type":    {Type: "u", EncryptionKey: encryptionKey, Address: address, Author: author(t)},
		"another address": {Type: "t", EncryptionKey: encryptionKey, Address: strings.Repeat("cd", 32), Author: author(t)},
		"another author":  {Type: "t", EncryptionKey: encryptionKey, Address: address, Author: strings.Repeat("ef", 32)},
		"another key":     {Type: "t", EncryptionKey: bytes.Repeat([]byte{5}, 32), Address: address, Author: author(t)},
	} {
		if _, ok := Open(relayCopy(t, e), o, func(v any, _ *Event) (any, error) { return v, nil }); ok {
			t.Errorf("%s: opened", name)
		}
	}
	// A plaintext that is a multiple of 256 bytes already gets no padding.
	if p := Plaintext("t", strings.Repeat("x", 256-len(`{"t":"t","v":""}`))); len(p) != 256 || strings.HasSuffix(p, " ") {
		t.Errorf("%d bytes", len(p))
	}
}

// SPEC.md §12: a record's plaintext beginning with U+FEFF is not JSON, so not a record.
func TestNoRecordOpensWhosePlaintextStartsWithAByteOrderMark(t *testing.T) {
	if _, ok := open(t, relayCopy(t, seal(t, 1.0))); !ok {
		t.Fatal("the record does not open")
	}
	plaintext := "\xef\xbb\xbf" + `{"t":"t","v":1}`
	plaintext += strings.Repeat(" ", 256-len(plaintext))
	content, err := nip44.Encrypt(plaintext, encryptionKey, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Sign(Template{CreatedAt: createdAt, Kind: 30078, Tags: [][]string{{"d", address}}, Content: content}, signer, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := open(t, relayCopy(t, e)); ok {
		t.Error("a plaintext after a byte order mark opened")
	}
}

// A newer format counts only once the event checks out (its author's signature).
func TestNewerFormats(t *testing.T) {
	for _, content := range []string{"", "#", "#future"} {
		e, _ := Sign(Template{CreatedAt: createdAt, Kind: 30078, Tags: [][]string{{"d", address}}, Content: content}, signer, rand.Reader)
		opened, ok := open(t, relayCopy(t, e))
		if !ok || !opened.Newer {
			t.Errorf("%q: %+v, %v", content, opened, ok)
		}
		// The same, signed by somebody else under the author's name: ignored.
		forged := relayCopy(t, e)
		o, _ := jsonv.AsObject(forged)
		o.Set("sig", strings.Repeat("00", 64))
		if _, ok := open(t, forged); ok {
			t.Errorf("%q: a forged copy counts", content)
		}
	}
	// A value of a newer version.
	e := seal(t, jsonv.O("version", 2))
	opened, ok := Open(relayCopy(t, e), OpenOptions{Type: "t", EncryptionKey: encryptionKey, Address: address, Author: author(t)},
		func(any, *Event) (any, error) { return nil, ErrNewer })
	if !ok || !opened.Newer {
		t.Errorf("a newer value: %+v, %v", opened, ok)
	}
}

func TestEventChecks(t *testing.T) {
	e := seal(t, 1.0)
	valid := func(mutate func(o *jsonv.Object)) bool {
		v := relayCopy(t, e)
		o, _ := jsonv.AsObject(v)
		mutate(o)
		_, ok := Valid(v)
		return ok
	}
	if !valid(func(*jsonv.Object) {}) {
		t.Fatal("the event does not check out")
	}
	// A created_at written 1791378000.0 is the same event: its id hashes integers.
	text := strings.Replace(string(jsonv.Marshal(e.JSON())), `"created_at":1791378000`, `"created_at":1791378000.0`, 1)
	if v, _ := jsonv.ParseString(text); !func() bool { _, ok := Valid(v); return ok }() {
		t.Error("1791378000.0 does not check out")
	}
	for name, mutate := range map[string]func(o *jsonv.Object){
		"another created_at": func(o *jsonv.Object) { o.Set("created_at", float64(createdAt+1)) },
		"a fractional date":  func(o *jsonv.Object) { o.Set("created_at", createdAt+0.5) },
		"a date above 2^53":  func(o *jsonv.Object) { o.Set("created_at", float64(1<<53)) },
		"a kind as a string": func(o *jsonv.Object) { o.Set("kind", "30078") },
		"another content":    func(o *jsonv.Object) { o.Set("content", "x") },
		"a tag of a number":  func(o *jsonv.Object) { o.Set("tags", []any{[]any{"d", 1.0}}) },
		"an upper-case id":   func(o *jsonv.Object) { id, _ := o.Get("id"); o.Set("id", strings.ToUpper(id.(string))) },
		"another signature":  func(o *jsonv.Object) { o.Set("sig", strings.Repeat("ab", 64)) },
		"no signature":       func(o *jsonv.Object) { o.Set("sig", nil) },
	} {
		if valid(mutate) {
			t.Errorf("%s checks out", name)
		}
	}
	// A lone surrogate in the content or a tag: no UTF-8, which the hash needs.
	for _, field := range []string{"content", "tags"} {
		lone, _ := jsonv.ParseString(`"a\ud800"`)
		v := relayCopy(t, e)
		o, _ := jsonv.AsObject(v)
		if field == "content" {
			o.Set("content", lone)
		} else {
			o.Set("tags", []any{[]any{"d", lone}})
		}
		if _, ok := WithValidID(v); ok {
			t.Errorf("a lone surrogate in %s checks out", field)
		}
	}
}

func TestRecordsThatOpenKeepCopiesThatCheckOut(t *testing.T) {
	e := seal(t, 1.0)
	memo := NewSigMemo()
	keep := RecordsThatOpen(func(d, a string) []byte {
		if d == address && a == author(t) {
			return encryptionKey
		}
		return nil
	}, memo)
	if _, ok := keep(relayCopy(t, e)); !ok {
		t.Error("the record is not kept")
	}
	// A copy under an id of its own, its content and signature left as they are: no.
	other := relayCopy(t, e)
	o, _ := jsonv.AsObject(other)
	o.Set("created_at", float64(createdAt+1))
	h := hash(e.PubKey, createdAt+1, 30078, e.Tags, e.Content)
	o.Set("id", hexOf(h[:]))
	if _, ok := keep(other); ok {
		t.Error("a copy with its own id is kept")
	}
	// Of an address the read does not open: no.
	sealedElsewhere, _ := Seal(SealOptions{Type: "t", Value: 1.0, Signer: signer, EncryptionKey: encryptionKey, Address: strings.Repeat("cd", 32), CreatedAt: createdAt, Random: rand.Reader})
	if _, ok := keep(relayCopy(t, sealedElsewhere)); ok {
		t.Error("a record of another address is kept")
	}
	if DTagOf(relayCopy(t, e)) != address || DTagOf(1.0) != "" {
		t.Error("DTagOf")
	}
}

func TestNewestRecord(t *testing.T) {
	older := seal(t, "older")
	newer, _ := Seal(SealOptions{Type: "t", Value: "newer", Signer: signer, EncryptionKey: encryptionKey, Address: address, CreatedAt: createdAt + 1, Random: rand.Reader})
	unreadable, _ := Sign(Template{CreatedAt: createdAt + 5, Kind: 30078, Tags: [][]string{{"d", address}}, Content: "#"}, signer, rand.Reader)
	answers := [][]any{{relayCopy(t, older)}, {relayCopy(t, newer), relayCopy(t, unreadable), relayCopy(t, older)}}
	newest := NewestRecord(answers, func(v any) (Opened[any], bool) { return open(t, v) })
	if newest.Event == nil || newest.Value != "newer" || !newest.HasUnreadable || newest.NewestUnreadable != createdAt+5 {
		t.Errorf("%+v", newest)
	}
	// On a tie, the lowest id.
	a, b := seal(t, "a"), seal(t, "b")
	lowest := a
	if b.ID < a.ID {
		lowest = b
	}
	if n := NewestRecord([][]any{{relayCopy(t, a), relayCopy(t, b)}}, func(v any) (Opened[any], bool) { return open(t, v) }); n.Event.ID != lowest.ID {
		t.Error("not the lowest id")
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[2*i], out[2*i+1] = digits[c>>4], digits[c&15]
	}
	return string(out)
}
