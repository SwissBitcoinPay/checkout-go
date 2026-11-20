package urls

import (
	"strings"
	"testing"

	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/proto"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

func TestRelayURLVectors(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "relay-urls.json"), "cases")
	if len(cases) == 0 {
		t.Fatal("no case")
	}
	for _, c := range cases {
		url := tv.Str(t, c, "url")
		if got, want := IsRelayURL(url), tv.Get(c, "valid") == true; got != want {
			t.Errorf("IsRelayURL(%q) = %v, want %v", url, got, want)
		}
	}
	t.Logf("%d cases", len(cases))
}

func TestHTTPSURLVectors(t *testing.T) {
	file := tv.Load(t, "https-urls.json")
	urls := tv.Objs(t, file, "urls")
	for _, c := range urls {
		url := tv.Str(t, c, "url")
		got, ok := ParseHTTPS(url)
		expected := tv.Get(c, "expected")
		if expected == nil {
			if ok {
				t.Errorf("%q read as %v", url, got)
			}
			continue
		}
		e, _ := jsonv.AsObject(expected)
		want := HTTPS{Host: tv.Str(t, e, "host"), Port: int(tv.Int(t, e, "port"))}
		if !ok || got != want {
			t.Errorf("%q = %v, %v; want %v", url, got, ok, want)
		}
	}
	same := tv.Objs(t, file, "sameHost")
	for _, c := range same {
		a, okA := ParseHTTPS(tv.Str(t, c, "a"))
		b, okB := ParseHTTPS(tv.Str(t, c, "b"))
		if !okA || !okB {
			t.Fatalf("%v: not https URLs", c.Keys())
		}
		if got := SameHost(a, b); got != (tv.Get(c, "expected") == true) {
			t.Errorf("SameHost(%v, %v) = %v", a, b, got)
		}
	}
	t.Logf("%d urls, %d sameHost", len(urls), len(same))
}

func TestRelayLists(t *testing.T) {
	ok := func(v any) bool { _, ok := ParseRelayList(v); return ok }
	if !ok([]any{"wss://a.example", "wss://b.example"}) {
		t.Error("a valid list refused")
	}
	if ok([]any{}) || ok([]any{"wss://a.example", "wss://a.example"}) || ok([]any{"wss://a.example", 1.0}) || ok("wss://a.example") || ok(nil) {
		t.Error("an invalid list read")
	}
	eleven := make([]any, 11)
	for i := range eleven {
		eleven[i] = "wss://r" + strings.Repeat("x", i) + ".example"
	}
	if ok(eleven) || !ok(eleven[:10]) {
		t.Error("10 relays at most")
	}
	// A lone surrogate in a path (a JSON escape makes one) is no text.
	v, _ := jsonv.ParseString(`["wss://relay.example/a\ud800"]`)
	if ok(v) {
		t.Error("a lone surrogate read")
	}
	if IsRelayURL("wss://relay.example/a\xffb") {
		t.Error("invalid UTF-8 read")
	}
	// 120 code points at most, counted in code points.
	long := "wss://relay.example/" + strings.Repeat("\xc3\xa9", 100)
	if !IsRelayURL(long) || IsRelayURL(long+"\xc3\xa9") {
		t.Error("120 code points")
	}
	// A key's or a link's part, and back.
	part := EncodeRelayList([]string{"wss://relay.example", "wss://nos.lol:4443", "wss://relay.example.ch/nostr"})
	list, decoded := DecodeRelayList(part)
	if !decoded || len(list) != 3 || list[1] != "wss://nos.lol:4443" {
		t.Errorf("DecodeRelayList = %v, %v", list, decoded)
	}
	for _, part := range []string{"", EncodeRelayList([]string{""}), "cmVsYXkuZXhhbXBsZQ=", "\xef\xbb\xbfcmVsYXkuZXhhbXBsZQ"} {
		if list, ok := DecodeRelayList(part); ok {
			t.Errorf("%q read as %v", part, list)
		}
	}
}

// A leading byte order mark is kept (strict UTF-8, SPEC.md §2): U+FEFF then "relay.example" names no
// relay. Only an HTTP body ignores one (§15).
func TestRelayListKeepsAByteOrderMark(t *testing.T) {
	part := EncodeRelayList([]string{"wss://\xef\xbb\xbfrelay.example"})
	if list, ok := DecodeRelayList(part); ok {
		t.Errorf("read as %v", list)
	}
}

func TestCheckoutOrigins(t *testing.T) {
	for v, want := range map[any]bool{
		"https://pay.sattle.me": true, "https://pay.sattle.me:8443": true,
		"https://pay.sattle.me/": false, "https://pay.sattle.me/pay": false, "https://pay.sattle.me?": false,
		"https://pay.sattle.me#": false, "http://pay.sattle.me": false, "https://localhost": false, 1.0: false, nil: false,
	} {
		if got := IsCheckoutOrigin(v); got != want {
			t.Errorf("IsCheckoutOrigin(%v) = %v", v, got)
		}
	}
}

func TestRefusedRedirectSchemes(t *testing.T) {
	if n := proto.RefusedRedirectSchemeCount(); n != 55 {
		t.Errorf("%d refused schemes", n)
	}
}
