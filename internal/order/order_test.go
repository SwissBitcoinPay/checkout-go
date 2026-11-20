package order

import (
	"testing"

	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/rates"
	tv "github.com/sattleme/checkout-go/internal/testvec"
	"github.com/sattleme/checkout-go/internal/urls"
)

func outcome(err error) jsonv.Members {
	e, _ := errs.Of(err)
	out := jsonv.O("error", string(e.Code))
	if e.Param != "" {
		out = append(out, jsonv.Member{Name: "param", Value: e.Param})
	}
	return out
}

func same(t *testing.T, name string, got, want any) {
	t.Helper()
	if g, w := string(jsonv.Marshal(got)), string(jsonv.Marshal(want)); g != w {
		t.Errorf("%s: got %s, want %s", name, g, w)
	}
}

func TestRefAndNoteVectors(t *testing.T) {
	file := tv.Load(t, "order-text.json")
	refs := tv.Objs(t, file, "refs")
	for _, c := range refs {
		ref := tv.Str(t, c, "ref")
		err := CheckRef(ref)
		if valid := tv.Get(c, "valid") == true; valid != (err == nil) {
			t.Errorf("ref %q: %v", ref, err)
		} else if err != nil {
			same(t, ref, outcome(err), jsonv.O("error", "invalid_order", "param", "ref"))
		}
	}
	notes := tv.Objs(t, file, "notes")
	for _, c := range notes {
		note, hasNote := tv.Get(c, "note").(string)
		checked, err := Check(Order{Ref: tv.Str(t, c, "ref"), Sats: 1, HasSats: true, Note: note, HasNote: hasNote}, nil)
		var got any
		if err != nil {
			got = outcome(err)
		} else {
			got = jsonv.O("label", checked.Label)
		}
		same(t, "note "+note, got, tv.Get(c, "expected"))
	}
	t.Logf("%d refs, %d notes", len(refs), len(notes))
}

func TestRedirectURLVectors(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "redirect-urls.json"), "cases")
	for _, c := range cases {
		url := tv.Str(t, c, "url")
		checked, err := Check(Order{Ref: "A-1", Sats: 1, HasSats: true, Checkout: true, RedirectURL: url, HasRedirectURL: true}, nil)
		expected := tv.Obj(t, c, "expected")
		shown, ok := urls.RedirectShown(url)
		if expected.Has("error") {
			if err == nil {
				t.Errorf("%q: accepted", url)
				continue
			}
			same(t, url, outcome(err), expected)
			if ok {
				t.Errorf("%q shown as %q", url, shown)
			}
			continue
		}
		if err != nil || checked.RedirectURL != url {
			t.Errorf("%q: %v", url, err)
		}
		same(t, url, jsonv.O("shown", shown), expected)
	}
	t.Logf("%d cases", len(cases))
}

// rate-sources.json at the order: invalid_order instead of invalid_option, and the client's
// sources when the order names none.
func TestOrderRateSourceVectors(t *testing.T) {
	values := tv.Objs(t, tv.Load(t, "rate-sources.json"), "values")
	client := []rates.Source{{Name: rates.Kraken}}
	for _, c := range values {
		name := tv.Str(t, c, "name")
		o := Order{Ref: "A-1", Fiat: &Price{Amount: "1", Currency: "CHF"}}
		if c.Has("rateSource") {
			o.RateSources, o.HasRateSources = tv.Get(c, "rateSource"), true
		}
		checked, err := Check(o, client)
		expected := tv.Obj(t, c, "expected")
		if expected.Has("error") {
			if err == nil {
				t.Errorf("%s: accepted", name)
				continue
			}
			same(t, name, outcome(err), jsonv.O("error", "invalid_order", "param", "rateSource"))
			continue
		}
		var got []any
		for _, s := range checked.RateSources {
			got = append(got, s.Name)
		}
		want := tv.Get(expected, "sources")
		if !c.Has("rateSource") {
			want = []any{"kraken"}
		}
		same(t, name, got, want)
	}
}

func TestOrderChecks(t *testing.T) {
	sats := func(o Order) Order { o.Ref, o.Sats, o.HasSats = "A-1", 1000, true; return o }
	for name, c := range map[string]struct {
		order Order
		param string
	}{
		"no ref":                    {Order{Sats: 1, HasSats: true}, "ref"},
		"neither sats nor fiat":     {Order{Ref: "A-1"}, "sats"},
		"both":                      {Order{Ref: "A-1", Sats: 1, HasSats: true, Fiat: &Price{"1", "CHF"}}, "sats"},
		"0 sats":                    {Order{Ref: "A-1", Sats: 0, HasSats: true}, "sats"},
		"more than 21 million BTC":  {Order{Ref: "A-1", Sats: 2_100_000_000_000_001, HasSats: true}, "sats"},
		"an invalid price":          {Order{Ref: "A-1", Fiat: &Price{"1.234", "CHF"}}, "fiat"},
		"a rate source in sats":     {sats(Order{RateSources: "kraken", HasRateSources: true}), "rateSource"},
		"59 s":                      {sats(Order{DurationSeconds: 59, HasDuration: true}), "durationSeconds"},
		"3601 s":                    {sats(Order{DurationSeconds: 3601, HasDuration: true}), "durationSeconds"},
		"a redirect without a link": {sats(Order{RedirectURL: "https://shop.example", HasRedirectURL: true}), "redirectUrl"},
		"a note with a tab":         {sats(Order{Note: "a\tb", HasNote: true}), "note"},
	} {
		_, err := Check(c.order, nil)
		if e, ok := errs.Of(err); !ok || e.Code != errs.InvalidOrder || e.Param != c.param {
			t.Errorf("%s: %v", name, err)
		}
	}
	checked, err := Check(sats(Order{DurationSeconds: 60, HasDuration: true, Checkout: true}), nil)
	if err != nil || checked.DurationSeconds != 60 || checked.Label != "A-1" || !checked.Checkout || checked.RedirectURL != "" || checked.RateSources != nil {
		t.Errorf("%+v, %v", checked, err)
	}
	if checked, _ := Check(sats(Order{}), nil); checked.DurationSeconds != 900 {
		t.Errorf("the default duration: %d", checked.DurationSeconds)
	}
}

func TestStoredInvoiceChecks(t *testing.T) {
	app := tv.Load(t, "app/app.json")
	real := tv.Obj(t, app, "realInvoice")
	bolt11, hash := tv.Str(t, real, "invoice"), tv.Str(t, real, "paymentHash")
	expiresAt := tv.Int(t, real, "timestamp") + tv.Int(t, real, "expirySeconds")
	valid := Stored{Ref: "ORD-1042", Bolt11: bolt11, PaymentHash: hash, VerifyURL: "https://breez.tips/lnurlp/verify/" + hash, Sats: 21, ExpiresAt: expiresAt}
	invoice, lightning, err := CheckStored(valid)
	if err != nil || lightning == nil || lightning.ExpiresAt != expiresAt || invoice.Ref != "" {
		t.Fatalf("%v, %v, %v", invoice, lightning, err)
	}
	deposit := "bc1px22v5j99gdwhxqyelg2p6tdcfajjd3njhzn22c2gura2dvxg2xlqw2wte4"
	withAddress := valid
	withAddress.OnchainAddress = deposit
	if invoice, _, err := CheckStored(withAddress); err != nil || invoice.Ref != "ORD-1042" {
		t.Errorf("with an address: %v, %v", invoice, err)
	}
	onchain := Stored{Ref: "ORD-1", OnchainAddress: deposit, Sats: 49000, ExpiresAt: 1791379180}
	if _, lightning, err := CheckStored(onchain); err != nil || lightning != nil {
		t.Errorf("on-chain only: %v", err)
	}
	for name, change := range map[string]func(*Stored){
		"0 sats":                    func(s *Stored) { s.Sats = 0 },
		"another amount":            func(s *Stored) { s.Sats = 22 },
		"another payment hash":      func(s *Stored) { s.PaymentHash = "00" + s.PaymentHash[2:] },
		"another end":               func(s *Stored) { s.ExpiresAt++ },
		"a verify URL in http":      func(s *Stored) { s.VerifyURL = "http://breez.tips/v" },
		"no verify URL":             func(s *Stored) { s.VerifyURL = "" },
		"not BOLT11":                func(s *Stored) { s.Bolt11 = "lnbc1" },
		"a hash without an invoice": func(s *Stored) { s.Bolt11 = "" },
		"an invalid address":        func(s *Stored) { s.OnchainAddress = "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq" },
		"an address without a ref":  func(s *Stored) { s.OnchainAddress, s.Ref = deposit, "" },
		"an end beyond 2^53":        func(s *Stored) { s.ExpiresAt = 1 << 53 },
	} {
		s := valid
		change(&s)
		if _, _, err := CheckStored(s); !errs.Is(err, errs.InvalidOrder) {
			t.Errorf("%s: %v", name, err)
		} else if e, _ := errs.Of(err); e.Param != "invoice" {
			t.Errorf("%s: param %s", name, e.Param)
		}
	}
	if _, _, err := CheckStored(Stored{Sats: 1, ExpiresAt: 1}); !errs.Is(err, errs.InvalidOrder) {
		t.Errorf("neither: %v", err)
	}
}
