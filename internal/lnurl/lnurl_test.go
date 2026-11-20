package lnurl

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/httpx"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/testkit"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

func client(route func(*http.Request) testkit.Answer) (*httpx.Client, *testkit.FakeHTTP) {
	fake := testkit.NewFakeHTTP(route)
	return httpx.New(fake, time.Second), fake
}

func TestLightningAddressVectors(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "https-urls.json"), "lightningAddresses")
	for _, c := range cases {
		address := tv.Str(t, c, "address")
		got, ok := AddressURL(address)
		want := tv.Get(c, "payUrl")
		if want == nil {
			if ok {
				t.Errorf("%q: %s", address, got)
			}
		} else if !ok || got != want {
			t.Errorf("%q: %q, %v; want %v", address, got, ok, want)
		}
	}
	t.Logf("%d cases", len(cases))
}

func TestInvoiceCheckVectors(t *testing.T) {
	file := tv.Load(t, "invoice-checks.json")
	payRequest := tv.Obj(t, file, "payRequest")
	pay := &PayRequest{
		Callback:        tv.Str(t, payRequest, "callback"),
		MinSendableMsat: 1000,
		MaxSendableMsat: 100_000_000,
		Metadata:        tv.Str(t, payRequest, "metadata"),
	}
	cases := tv.Objs(t, file, "cases")
	for _, c := range cases {
		answer := tv.Obj(t, c, "answer")
		sats, _ := strconv.ParseInt(tv.Str(t, c, "sats"), 10, 64)
		_, err := CheckInvoice(InvoiceAnswer{PR: tv.Str(t, answer, "pr"), Verify: tv.Str(t, answer, "verify")},
			pay, sats, int(tv.Int(t, c, "durationSeconds")), tv.Int(t, c, "now"))
		var got jsonv.Members
		if err == nil {
			got = jsonv.Members{{Name: "valid", Value: true}}
		} else {
			e, _ := errs.Of(err)
			got = jsonv.Members{{Name: "error", Value: string(e.Code)}, {Name: "check", Value: e.Check}}
		}
		if g, w := string(jsonv.Marshal(got)), string(jsonv.Marshal(tv.Get(c, "expected"))); g != w {
			t.Errorf("%s: got %s, want %s", tv.Str(t, c, "name"), g, w)
		}
	}
	t.Logf("%d cases", len(cases))
}

func TestCommentVectors(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "order-text.json"), "comments")
	for _, c := range cases {
		label := tv.Str(t, c, "label")
		http, fake := client(func(*http.Request) testkit.Answer {
			return testkit.JSON(jsonv.Members{{Name: "pr", Value: "lnbc1"}, {Name: "verify", Value: "https://x.example/v"}})
		})
		pay := &PayRequest{Callback: "https://x.example/cb", MinSendableMsat: 1000, MaxSendableMsat: 1_000_000, Metadata: "[]", CommentAllowed: tv.Int(t, c, "commentAllowed")}
		if _, err := RequestInvoice(context.Background(), http, pay, 1, 60, label); err != nil {
			t.Fatal(err)
		}
		query := fake.Calls()[0].URL.Query()
		sent := tv.Get(c, "sent") == true
		if got, has := query.Get("comment"), query.Has("comment"); has != sent || sent && got != label {
			t.Errorf("label %q, commentAllowed %d: comment %q sent %v", label, tv.Int(t, c, "commentAllowed"), got, has)
		}
	}
	t.Logf("%d cases", len(cases))
}

func TestCallbackKeepsItsParametersAndDropsItsFragment(t *testing.T) {
	pay := &PayRequest{Callback: "https://breez.tips/lnurlp/shop/invoice?k=1&amount=5&x=a+b%2Fc#frag", CommentAllowed: 100}
	got := CallbackURL(pay, 21, 900, "Order 1042 \xc3\xa9")
	want := "https://breez.tips/lnurlp/shop/invoice?k=1&amount=21000&x=a+b%2Fc&expiry=900&comment=Order+1042+%C3%A9"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if got := CallbackURL(&PayRequest{Callback: "https://breez.tips/cb"}, 1, 60, "x"); got != "https://breez.tips/cb?amount=1000&expiry=60" {
		t.Errorf("got %s", got)
	}
}

func TestFetchPayRequest(t *testing.T) {
	body := func(members ...jsonv.Member) jsonv.Members {
		base := jsonv.Members{
			{Name: "tag", Value: "payRequest"},
			{Name: "callback", Value: "https://breez.tips/lnurlp/shop/invoice"},
			{Name: "minSendable", Value: 1000},
			{Name: "maxSendable", Value: 100_000_000},
			{Name: "metadata", Value: "[]"},
		}
		return append(base, members...)
	}
	for name, c := range map[string]struct {
		answer testkit.Answer
		code   errs.Code
		allow  int64
	}{
		"valid":                     {testkit.JSON(body(jsonv.Member{Name: "commentAllowed", Value: 255})), "", 255},
		"a negative comment length": {testkit.JSON(body(jsonv.Member{Name: "commentAllowed", Value: -5})), "", 0},
		"a fractional length":       {testkit.JSON(body(jsonv.Member{Name: "commentAllowed", Value: 1.5})), "", 0},
		"a refusal":                 {testkit.JSON(jsonv.Members{{Name: "status", Value: "ERROR"}, {Name: "reason", Value: "no"}}), errs.Network, 0},
		"a refusal with a 400":      {testkit.Status(400, jsonv.Members{{Name: "status", Value: "Error"}}), errs.Network, 0},
		"HTTP 500":                  {testkit.Status(500, jsonv.Members{}), errs.Network, 0},
		"another tag":               {testkit.JSON(body(jsonv.Member{Name: "tag", Value: "withdrawRequest"})), errs.InvalidInvoice, 0},
		"a callback in http":        {testkit.JSON(body(jsonv.Member{Name: "callback", Value: "http://breez.tips/cb"})), errs.InvalidInvoice, 0},
		"min above max":             {testkit.JSON(body(jsonv.Member{Name: "minSendable", Value: 200_000_000})), errs.InvalidInvoice, 0},
		"a fractional minimum":      {testkit.JSON(body(jsonv.Member{Name: "minSendable", Value: 1000.5})), errs.InvalidInvoice, 0},
		"a minimum as a string":     {testkit.JSON(body(jsonv.Member{Name: "minSendable", Value: "1000"})), errs.InvalidInvoice, 0},
		"no metadata":               {testkit.JSON(body(jsonv.Member{Name: "metadata", Value: nil})), errs.InvalidInvoice, 0},
		"no answer":                 {testkit.Answer{Err: testkit.ErrNoAnswer}, errs.Network, 0},
		"a redirect":                {testkit.Answer{Status: 302, Headers: map[string]string{"Location": "https://evil.example/"}}, errs.Network, 0},
	} {
		http, fake := client(func(*http.Request) testkit.Answer { return c.answer })
		pay, err := FetchPayRequest(context.Background(), http, " Shop@Breez.Tips ")
		if c.code == "" {
			if err != nil || pay.CommentAllowed != c.allow || pay.MinSendableMsat != 1000 {
				t.Errorf("%s: %v, %v", name, pay, err)
			}
		} else if !errs.Is(err, c.code) {
			t.Errorf("%s: %v", name, err)
		} else if strings.Contains(err.Error(), "shop") {
			t.Errorf("%s: the message names the Lightning address: %s", name, err)
		}
		calls := fake.Calls()
		if len(calls) != 1 || calls[0].URL.String() != "https://breez.tips/.well-known/lnurlp/shop" {
			t.Errorf("%s: %d calls", name, len(calls))
		}
	}
	if _, err := FetchPayRequest(context.Background(), nil, "shop@localhost"); !errs.Is(err, errs.InvalidInvoice) {
		t.Errorf("an invalid address: %v", err)
	}
}

// pay-requests.json (SPEC.md §16): the pay request read from the answer of its pay URL, served by
// the fake HTTP transport the client is given, as the client reads it: its callback, its sendable
// sats, its commentAllowed and the description hash its invoices are checked with; or the error.
func TestPayRequestVectors(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "pay-requests.json"), "cases")
	for _, c := range cases {
		name := tv.Str(t, c, "name")
		served := tv.Obj(t, c, "http")
		answer := testkit.Answer{Status: int(tv.Int(t, served, "status"))}
		if text, ok := tv.Get(served, "bodyText").(string); ok {
			answer.Text = &text
		} else {
			answer.JSON = tv.Get(served, "body")
		}
		http, fake := client(func(*http.Request) testkit.Answer { return answer })
		var got any
		pay, err := FetchPayRequest(context.Background(), http, "shop@breez.tips")
		if err != nil {
			e, ok := errs.Of(err)
			if !ok {
				t.Fatalf("%s: %T %v", name, err, err)
			}
			got = jsonv.O("error", string(e.Code))
		} else {
			min, max := SendableSats(pay)
			got = jsonv.O("callback", pay.Callback, "sendable", jsonv.O("min", strconv.FormatInt(min, 10), "max", strconv.FormatInt(max, 10)),
				"commentAllowed", pay.CommentAllowed, "descriptionHash", DescriptionHashOf(pay))
		}
		tv.Equal(t, name, got, tv.Get(c, "expected"))
		if calls := fake.Calls(); len(calls) != 1 || calls[0].URL.String() != "https://breez.tips/.well-known/lnurlp/shop" {
			t.Errorf("%s: %d requests", name, len(calls))
		}
	}
	t.Logf("%d cases", len(cases))
}

func TestSendableSatsRoundInwards(t *testing.T) {
	if min, max := SendableSats(&PayRequest{MinSendableMsat: 1001, MaxSendableMsat: 1_999_999}); min != 2 || max != 1999 {
		t.Errorf("%d to %d", min, max)
	}
}

func TestRequestInvoiceAnswers(t *testing.T) {
	pay := &PayRequest{Callback: "https://breez.tips/cb"}
	for name, c := range map[string]struct {
		answer testkit.Answer
		code   errs.Code
	}{
		"a refusal":    {testkit.Status(400, jsonv.Members{{Name: "status", Value: "ERROR"}, {Name: "reason", Value: "Amount out of range"}}), errs.Network},
		"no verify":    {testkit.JSON(jsonv.Members{{Name: "pr", Value: "lnbc1"}}), errs.InvalidInvoice},
		"pr a number":  {testkit.JSON(jsonv.Members{{Name: "pr", Value: 1}, {Name: "verify", Value: "https://breez.tips/v"}}), errs.InvalidInvoice},
		"HTTP 429":     {testkit.Status(429, jsonv.Members{{Name: "status", Value: "ERROR"}}), errs.Network},
		"HTTP 408":     {testkit.Status(408, jsonv.Members{{Name: "status", Value: "ERROR"}}), errs.Network},
		"a JSON array": {testkit.JSON([]any{}), errs.Network},
	} {
		http, _ := client(func(*http.Request) testkit.Answer { return c.answer })
		if _, err := RequestInvoice(context.Background(), http, pay, 1, 60, ""); !errs.Is(err, c.code) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// payments.json at the level of the Lightning part: the states and errors of each answer.
func TestVerifyPaymentVectors(t *testing.T) {
	file := tv.Load(t, "payments.json")
	invoice := tv.Obj(t, file, "invoice")
	inv := Invoice{
		Bolt11:      tv.Str(t, invoice, "bolt11"),
		PaymentHash: tv.Str(t, invoice, "paymentHash"),
		VerifyURL:   tv.Str(t, invoice, "verifyUrl"),
		ExpiresAt:   tv.Int(t, invoice, "expiresAt"),
	}
	cases := tv.Objs(t, file, "cases")
	for _, c := range cases {
		h := tv.Obj(t, c, "http")
		answer := testkit.Answer{Status: int(tv.Int(t, h, "status")), JSON: tv.Get(h, "body")}
		if h.Has("bodyText") {
			body := tv.Str(t, h, "bodyText")
			answer.Text = &body
		}
		http, _ := client(func(*http.Request) testkit.Answer { return answer })
		part, err := VerifyPayment(context.Background(), http, inv, tv.Int(t, c, "now"))
		expected := tv.Obj(t, c, "expected")
		name := tv.Str(t, c, "name")
		if expected.Has("error") {
			if !errs.Is(err, errs.Code(tv.Str(t, expected, "error"))) {
				t.Errorf("%s: %v, %v", name, part, err)
			}
			continue
		}
		if err != nil || part.State != tv.Str(t, expected, "state") {
			t.Errorf("%s: %v, %v", name, part, err)
		}
		if part.State == Paid && part.Preimage != tv.Str(t, expected, "preimage") {
			t.Errorf("%s: preimage %s", name, part.Preimage)
		}
	}
	t.Logf("%d cases", len(cases))
}
