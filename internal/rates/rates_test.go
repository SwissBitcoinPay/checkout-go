package rates

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/httpx"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/logx"
	"github.com/sattleme/checkout-go/internal/model"
	"github.com/sattleme/checkout-go/internal/testkit"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

const now = 1_791_385_300

func i64(n int64) *int64 { return &n }

// outcome writes an error as the vectors do: {"error": code, "param"?}.
func outcome(err error) jsonv.Members {
	e, ok := errs.Of(err)
	if !ok {
		return jsonv.Members{{Name: "error", Value: err.Error()}}
	}
	out := jsonv.Members{{Name: "error", Value: string(e.Code)}}
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

func TestFiatVectors(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "fiat.json"), "cases")
	for _, c := range cases {
		input := tv.Obj(t, c, "input")
		fiat, err := ParseFiat(tv.Str(t, input, "amount"), tv.Str(t, input, "currency"))
		var got any
		if err != nil {
			got = outcome(err)
		} else {
			got = jsonv.Members{{Name: "amount", Value: fiat.Amount}, {Name: "minor", Value: big.NewInt(fiat.Minor).String()},
				{Name: "digits", Value: fiat.Digits}, {Name: "currency", Value: fiat.Currency}}
		}
		same(t, tv.Str(t, input, "amount")+" "+tv.Str(t, input, "currency"), got, tv.Get(c, "expected"))
	}
	t.Logf("%d cases", len(cases))
}

func TestPriceVectors(t *testing.T) {
	file := tv.Load(t, "prices.json")
	read := func(s string, ok bool) any {
		if !ok {
			return nil
		}
		d, ok := ParseDecimal(s)
		if !ok {
			return nil
		}
		return d.Format()
	}
	texts := tv.Objs(t, file, "text")
	for _, c := range texts {
		same(t, "text "+tv.Str(t, c, "price"), read(tv.Str(t, c, "price"), true), tv.Get(c, "expected"))
	}
	numbers := tv.Objs(t, file, "numbers")
	for _, c := range numbers {
		v, err := jsonv.ParseString(tv.Str(t, c, "json"))
		if err != nil {
			t.Fatal(err)
		}
		s, ok := NumberText(v.(float64))
		same(t, "number "+tv.Str(t, c, "json"), read(s, ok), tv.Get(c, "expected"))
	}
	t.Logf("%d texts, %d numbers", len(texts), len(numbers))
}

func TestConversionVectors(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "conversions.json"), "cases")
	for _, c := range cases {
		fiat := tv.Obj(t, c, "fiat")
		sats, err := FiatToSats(tv.Str(t, fiat, "amount"), tv.Str(t, fiat, "currency"), tv.Str(t, c, "price"))
		if err != nil || sats.String() != tv.Str(t, c, "sats") {
			t.Errorf("%s %s at %s: %v, %v", tv.Str(t, fiat, "amount"), tv.Str(t, fiat, "currency"), tv.Str(t, c, "price"), sats, err)
		}
	}
	t.Logf("%d cases", len(cases))
}

func names(sources []Source) []any {
	out := make([]any, len(sources))
	for i, s := range sources {
		out[i] = s.Name
	}
	return out
}

func TestRateSourceVectors(t *testing.T) {
	file := tv.Load(t, "rate-sources.json")
	defaults := names(Default())
	same(t, "default", defaults, tv.Get(file, "default"))
	values := tv.Objs(t, file, "values")
	for _, c := range values {
		var got any
		if !c.Has("rateSource") {
			got = jsonv.Members{{Name: "sources", Value: names(Default())}}
		} else if sources, ok := SourcesOf(tv.Get(c, "rateSource")); ok {
			got = jsonv.Members{{Name: "sources", Value: names(sources)}}
		} else {
			got = outcome(errs.NewInvalidOption("rateSource", "invalid"))
		}
		same(t, tv.Str(t, c, "name"), got, tv.Get(c, "expected"))
	}
	lists := tv.Obj(t, file, "lists")
	currency, at := tv.Str(t, lists, "currency"), tv.Int(t, lists, "now")
	cases := tv.Objs(t, lists, "cases")
	for _, c := range cases {
		answers := tv.Obj(t, c, "answers")
		var asked []any
		ask := func(ctx context.Context, source Source, currency string, rc Context, now int64) (Rate, error) {
			asked = append(asked, source.Name)
			answer, _ := answers.Get(source.Name)
			switch answer {
			case "network":
				return Rate{}, errs.NewNetwork(source.Name+" is down", nil)
			case "rate":
				return Rate{}, errs.NewRate(source.Name+" gave no price", nil)
			case "unsupported_currency":
				return Rate{}, errs.NewUnsupportedCurrency(currency, source.Name, "")
			}
			// The price, checked as a source's: §25.
			quote, _ := jsonv.AsObject(answer)
			price, _ := quote.Get("price")
			q := Quote{Price: price.(string)}
			if v, _ := quote.Get("time"); v != nil {
				n, _ := jsonv.AsSafeInteger(v)
				q.Time = &n
			}
			got, err := FetchRate(ctx, Source{Name: CustomName, Custom: func(context.Context, string) (Quote, error) { return q, nil }}, currency, rc, now)
			got.Info.Source = source.Name
			return got, err
		}
		var sources []Source
		for _, name := range tv.Arr(t, c, "sources") {
			sources = append(sources, Source{Name: name.(string)})
		}
		var warnings []string
		rate, err := FetchFirstRate(context.Background(), sources, currency, Context{}, func() int64 { return at },
			logx.Func(func(m string) { warnings = append(warnings, m) }), ask)
		var got jsonv.Members
		if err != nil {
			e, _ := errs.Of(err)
			got = jsonv.Members{{Name: "error", Value: string(e.Code)}, {Name: "asked", Value: asked}}
		} else {
			got = jsonv.Members{{Name: "source", Value: rate.Info.Source}, {Name: "price", Value: rate.Info.Price}, {Name: "asked", Value: asked}}
		}
		same(t, tv.Str(t, c, "name"), got, tv.Get(c, "expected"))
		// Each failure the next source replaces is logged, never the last one.
		if len(warnings) != len(asked)-1 {
			t.Errorf("%s: %d warnings for %d sources asked", tv.Str(t, c, "name"), len(warnings), len(asked))
		}
	}
	t.Logf("%d values, %d lists", len(values), len(cases))
}

func TestExactConversion(t *testing.T) {
	// ⌈2450 × 10^8 / 6 901 200⌉: the exact quotient is 35 501.07…
	for _, price := range []string{"69012", "69012.000"} {
		if sats, _ := FiatToSats("24.50", "CHF", price); sats.Int64() != 35_502 {
			t.Errorf("%s: %v", price, sats)
		}
	}
	if sats, _ := FiatToSats("1.00", "CHF", "100000"); sats.Int64() != 1000 {
		t.Errorf("1 CHF: %v", sats)
	}
	price := "69025.1084416666694295"
	if d, _ := ParseDecimal(price); d.Format() != price {
		t.Errorf("%s read as %s", price, d.Format())
	}
	dividend, _ := new(big.Int).SetString("2450"+"00000000"+"0000000000000000", 10)
	divisor, _ := new(big.Int).SetString("69025108441666669429500", 10)
	want := new(big.Int).Quo(new(big.Int).Sub(new(big.Int).Add(dividend, divisor), big.NewInt(1)), divisor)
	if sats, _ := FiatToSats("24.50", "CHF", price); sats.Cmp(want) != 0 {
		t.Errorf("%v, want %v", sats, want)
	}
	for text, want := range map[string]string{"8e-8": "8/8", "1.5e+21": "1500000000000000000000/0", "6.9012e4": "69012/0", "69012.50": "690125/1"} {
		d, ok := ParseDecimal(text)
		if got := d.Units.String() + "/" + big.NewInt(int64(d.Scale)).String(); !ok || got != want {
			t.Errorf("%s: %s", text, got)
		}
	}
	for _, price := range []string{"0", "0.000", "-69012", "", "abc", "1,5", " ", " 69012", "69012\n", "0x10", "1e65", strings.Repeat("1", 65)} {
		if _, ok := ParseDecimal(price); ok {
			t.Errorf("price %q read", price)
		}
		if _, err := FiatToSats("1", "CHF", price); !errs.Is(err, errs.Rate) {
			t.Errorf("price %q: %v", price, err)
		}
	}
	for _, fiat := range [][2]string{{"24.505", "CHF"}, {"1.5", "JPY"}, {"0", "CHF"}, {"0.00", "CHF"}, {"-1", "CHF"}, {"1e3", "CHF"},
		{"24.50", "SAT"}, {"24.50", "CHFX"}, {"99999999999999999", "CHF"}, {"1", "hasOwnProperty"}} {
		if _, err := ParseFiat(fiat[0], fiat[1]); !errs.Is(err, errs.InvalidOrder) {
			t.Errorf("%v: %v", fiat, err)
		}
	}
	jpy, _ := ParseFiat("1500", "JPY")
	chf, _ := ParseFiat("24.500", "CHF")
	if jpy != (model.Fiat{Amount: "1500", Currency: "JPY", Minor: 1500, Digits: 0}) || chf.Amount != "24.50" || chf.Minor != 2450 {
		t.Errorf("%v %v", jpy, chf)
	}
}

func askPrice(t *testing.T, source Source, currency string, route func(*http.Request) testkit.Answer, apiKey string) (model.RateInfo, []testkit.Call, error) {
	t.Helper()
	fake := testkit.NewFakeHTTP(route)
	rate, err := FetchRate(context.Background(), source, currency, Context{HTTP: httpx.New(fake, time.Second), CoinGeckoAPIKey: apiKey}, now)
	return rate.Info, fake.Calls(), err
}

func answer(v any) func(*http.Request) testkit.Answer {
	return func(*http.Request) testkit.Answer { return testkit.JSON(v) }
}

func TestCoinGecko(t *testing.T) {
	route := func(req *http.Request) testkit.Answer {
		if strings.HasSuffix(req.URL.Path, "/simple/price") {
			return testkit.JSON(jsonv.O("bitcoin", jsonv.O("chf", 69012, "last_updated_at", now-30)))
		}
		return testkit.Status(404, jsonv.O())
	}
	info, calls, err := askPrice(t, Source{Name: CoinGecko}, "CHF", route, "")
	if err != nil || info != (model.RateInfo{Source: "coingecko", Currency: "CHF", Price: "69012", FetchedAt: now, Time: info.Time}) || *info.Time != now-30 {
		t.Errorf("%v, %v", info, err)
	}
	if got := calls[0].URL.String(); got != "https://api.coingecko.com/api/v3/simple/price?ids=bitcoin&vs_currencies=chf&include_last_updated_at=true" {
		t.Errorf("asked %s", got)
	}
	// CoinGecko answers HTTP 403 to a request without a User-Agent (SPEC.md §15).
	if calls[0].Headers.Get("x-cg-demo-api-key") != "" || calls[0].Headers.Get("User-Agent") != "sattleme-Checkout/1" {
		t.Errorf("headers %v", calls[0].Headers)
	}
	_, calls, _ = askPrice(t, Source{Name: CoinGecko}, "CHF", route, "demo")
	if calls[0].Headers.Get("x-cg-demo-api-key") != "demo" || calls[0].Headers.Get("User-Agent") != "sattleme-Checkout/1" {
		t.Errorf("headers %v", calls[0].Headers)
	}
	if _, _, err := askPrice(t, Source{Name: CoinGecko}, "XYZ", answer(jsonv.O("bitcoin", jsonv.O())), ""); !errs.Is(err, errs.UnsupportedCurrency) {
		t.Errorf("XYZ: %v", err)
	}
	if _, _, err := askPrice(t, Source{Name: CoinGecko}, "CHF", answer(jsonv.O("nope", 1)), ""); !errs.Is(err, errs.Rate) {
		t.Errorf("no bitcoin: %v", err)
	}
	if _, _, err := askPrice(t, Source{Name: CoinGecko}, "CHF", answer(jsonv.O("bitcoin", jsonv.O("chf", 69012, "last_updated_at", 1.5))), ""); !errs.Is(err, errs.Rate) {
		t.Errorf("a date that is no integer: %v", err)
	}
	info, _, err = askPrice(t, Source{Name: CoinGecko}, "CHF", answer(jsonv.O("bitcoin", jsonv.O("chf", "69012.50", "last_updated_at", "x"))), "")
	if err != nil || info.Time != nil || info.Price != "69012.5" {
		t.Errorf("an undated price: %v, %v", info, err)
	}
}

func TestMempool(t *testing.T) {
	body := jsonv.O("time", now-60, "USD", 82977, "EUR", 74000, "GBP", 63000, "CAD", 118000, "CHF", 69080, "AUD", 119000, "JPY", 13187638)
	info, calls, err := askPrice(t, Source{Name: Mempool}, "CHF", answer(body), "")
	if err != nil || info.Source != "mempool" || info.Price != "69080" || *info.Time != now-60 {
		t.Errorf("%v, %v", info, err)
	}
	if calls[0].URL.String() != "https://mempool.space/api/v1/prices" {
		t.Errorf("asked %s", calls[0].URL)
	}
	if _, calls, err := askPrice(t, Source{Name: Mempool}, "SEK", answer(body), ""); !errs.Is(err, errs.UnsupportedCurrency) || len(calls) != 0 {
		t.Errorf("SEK: %v, %d requests", err, len(calls))
	}
	if _, _, err := askPrice(t, Source{Name: Mempool}, "CHF", answer(body[1:]), ""); !errs.Is(err, errs.Rate) {
		t.Errorf("no time: %v", err)
	}
}

func TestOpenNode(t *testing.T) {
	body := jsonv.O("data", jsonv.O(
		"BTCCHF", jsonv.O("CHF", 68991.16, "BTC", 0.00001449, "currency", "CHF"),
		"BTCJPY", jsonv.O("JPY", 13185808.44, "BTC", 8e-8, "currency", "JPY"),
	))
	info, calls, err := askPrice(t, Source{Name: OpenNode}, "CHF", answer(body), "")
	if err != nil || info.Price != "68991.16" || calls[0].URL.String() != "https://api.opennode.com/v1/rates" {
		t.Errorf("%v, %v", info, err)
	}
	if _, _, err := askPrice(t, Source{Name: OpenNode}, "SEK", answer(body), ""); !errs.Is(err, errs.UnsupportedCurrency) {
		t.Errorf("SEK: %v", err)
	}
	if _, _, err := askPrice(t, Source{Name: OpenNode}, "CHF", answer(jsonv.O("data", []any{})), ""); !errs.Is(err, errs.Rate) {
		t.Errorf("data an array: %v", err)
	}
	if _, _, err := askPrice(t, Source{Name: OpenNode}, "CHF", answer(jsonv.O("data", jsonv.O("BTCCHF", jsonv.O()))), ""); !errs.Is(err, errs.UnsupportedCurrency) {
		t.Errorf("a pair without its price: %v", err)
	}
}

func TestCoinbase(t *testing.T) {
	rates := jsonv.O("CHF", "69025.1084416666694295", "USD", "83000.1")
	data := jsonv.O("currency", "BTC", "rates", rates)
	info, calls, err := askPrice(t, Source{Name: Coinbase}, "CHF", answer(jsonv.O("data", data)), "")
	if err != nil || info.Price != "69025.1084416666694295" || calls[0].URL.String() != "https://api.coinbase.com/v2/exchange-rates?currency=BTC" {
		t.Errorf("%v, %v", info, err)
	}
	if _, _, err := askPrice(t, Source{Name: Coinbase}, "SEK", answer(jsonv.O("data", data)), ""); !errs.Is(err, errs.UnsupportedCurrency) {
		t.Errorf("SEK: %v", err)
	}
	eth := jsonv.O("currency", "ETH", "rates", rates)
	if _, _, err := askPrice(t, Source{Name: Coinbase}, "CHF", answer(jsonv.O("data", eth)), ""); !errs.Is(err, errs.Rate) {
		t.Errorf("ETH: %v", err)
	}
}

func TestKraken(t *testing.T) {
	ticker := func(key string) func(*http.Request) testkit.Answer {
		return answer(jsonv.O("error", []any{}, "result", jsonv.O(key, jsonv.O("c", []any{"69050.00000", "0.1"}, "a", []any{"1"}))))
	}
	info, calls, err := askPrice(t, Source{Name: Kraken}, "CHF", ticker("XBTCHF"), "")
	if err != nil || info.Price != "69050" || calls[0].URL.String() != "https://api.kraken.com/0/public/Ticker?pair=XBTCHF" {
		t.Errorf("%v, %v", info, err)
	}
	if info, _, err := askPrice(t, Source{Name: Kraken}, "USD", ticker("XXBTZUSD"), ""); err != nil || info.Price != "69050" {
		t.Errorf("USD: %v, %v", info, err)
	}
	if _, _, err := askPrice(t, Source{Name: Kraken}, "ZZZ", answer(jsonv.O("error", []any{"EQuery:Unknown asset pair"})), ""); !errs.Is(err, errs.UnsupportedCurrency) {
		t.Errorf("ZZZ: %v", err)
	}
	if _, _, err := askPrice(t, Source{Name: Kraken}, "CHF", answer(jsonv.O("error", []any{"EService:Unavailable"})), ""); !errs.Is(err, errs.Rate) {
		t.Errorf("an error: %v", err)
	}
	two := jsonv.O("error", []any{}, "result", jsonv.O("A", jsonv.O("c", []any{"1"}), "B", jsonv.O("c", []any{"2"})))
	if _, _, err := askPrice(t, Source{Name: Kraken}, "CHF", answer(two), ""); !errs.Is(err, errs.Rate) {
		t.Errorf("two entries: %v", err)
	}
}

func TestUnknownNameAndHTTPFailures(t *testing.T) {
	if _, _, err := askPrice(t, Source{Name: "bitstamp"}, "CHF", answer(jsonv.O()), ""); !errs.Is(err, errs.Rate) {
		t.Errorf("bitstamp: %v", err)
	}
	if _, _, err := askPrice(t, Source{Name: Coinbase}, "CHF", func(*http.Request) testkit.Answer { return testkit.Status(503, jsonv.O()) }, ""); !errs.Is(err, errs.Network) {
		t.Errorf("503: %v", err)
	}
	if _, _, err := askPrice(t, Source{Name: Coinbase}, "CHF", func(*http.Request) testkit.Answer { return testkit.Answer{Err: testkit.ErrNoAnswer} }, ""); !errs.Is(err, errs.Network) {
		t.Errorf("no answer: %v", err)
	}
	if _, _, err := askPrice(t, Source{Name: Coinbase}, "CHF", func(*http.Request) testkit.Answer { return testkit.Status(404, jsonv.O()) }, ""); !errs.Is(err, errs.Rate) {
		t.Errorf("404: %v", err)
	}
}

// SPEC.md §26: a source's HTTP status is network for 500 to 599, 408 and 429, rate for any
// other; a redirect is network (SPEC.md §15: it is no answer).
// rate-sources.json, statuses: the error of a source that answers with an HTTP status other than
// 2xx (a redirect is no answer), each known source asked.
func TestRateSourceStatusVectors(t *testing.T) {
	statuses := tv.Objs(t, tv.Load(t, "rate-sources.json"), "statuses")
	for _, c := range statuses {
		status := int(tv.Int(t, c, "status"))
		for _, name := range []string{CoinGecko, Mempool, OpenNode, Coinbase, Kraken} {
			_, calls, err := askPrice(t, Source{Name: name}, "CHF", func(*http.Request) testkit.Answer { return testkit.Status(status, jsonv.O()) }, "")
			if len(calls) != 1 || err == nil {
				t.Errorf("HTTP %d, %s: %d requests, %v", status, name, len(calls), err)
				continue
			}
			same(t, fmt.Sprintf("HTTP %d, %s", status, name), outcome(err), tv.Get(c, "expected"))
		}
	}
	t.Logf("%d statuses", len(statuses))
}

func TestASourcesHTTPStatusIsNetworkOnlyFor5xx408And429(t *testing.T) {
	for status, want := range map[int]errs.Code{
		500: errs.Network, 503: errs.Network, 599: errs.Network, 408: errs.Network, 429: errs.Network,
		301: errs.Network, 302: errs.Network, 307: errs.Network, 308: errs.Network,
		600: errs.Rate, 999: errs.Rate, 499: errs.Rate, 404: errs.Rate, 400: errs.Rate, 403: errs.Rate, 410: errs.Rate,
	} {
		_, _, err := askPrice(t, Source{Name: Coinbase}, "CHF", func(*http.Request) testkit.Answer { return testkit.Status(status, jsonv.O()) }, "")
		if !errs.Is(err, want) {
			t.Errorf("HTTP %d: %v, want %s", status, err, want)
		}
	}
}

func custom(fn func(context.Context, string) (Quote, error)) Source {
	return Source{Name: CustomName, Custom: fn}
}

func TestCustomSources(t *testing.T) {
	none := func(*http.Request) testkit.Answer { return testkit.Status(404, jsonv.O()) }
	info, _, err := askPrice(t, custom(func(context.Context, string) (Quote, error) { return Quote{Price: "70000.5"}, nil }), "CHF", none, "")
	if err != nil || info != (model.RateInfo{Source: "custom", Currency: "CHF", Price: "70000.5", FetchedAt: now}) {
		t.Errorf("%v, %v", info, err)
	}
	if info, _, _ := askPrice(t, custom(func(context.Context, string) (Quote, error) { return Quote{Price: "70000", Time: i64(now - 5)}, nil }), "CHF", none, ""); *info.Time != now-5 {
		t.Errorf("dated: %v", info)
	}
	for _, bad := range []string{"0", "-5", "abc", "Infinity", ""} {
		bad := bad
		if _, _, err := askPrice(t, custom(func(context.Context, string) (Quote, error) { return Quote{Price: bad}, nil }), "CHF", none, ""); !errs.Is(err, errs.Rate) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	unsupported := errs.NewUnsupportedCurrency("CHF", "mine", "")
	if _, _, err := askPrice(t, custom(func(context.Context, string) (Quote, error) { return Quote{}, unsupported }), "CHF", none, ""); err != unsupported {
		t.Errorf("unsupported: %v", err)
	}
	broken := errors.New("broken")
	_, _, err = askPrice(t, custom(func(context.Context, string) (Quote, error) { return Quote{}, broken }), "CHF", none, "")
	if !errs.Is(err, errs.Rate) || !errors.Is(err, broken) {
		t.Errorf("broken: %v", err)
	}
	// A price older than an hour, or dated in the future, is refused.
	dated := func(at int64) Source {
		return custom(func(context.Context, string) (Quote, error) { return Quote{Price: "69000", Time: i64(at)}, nil })
	}
	if info, _, err := askPrice(t, dated(now-3600), "CHF", none, ""); err != nil || *info.Time != now-3600 {
		t.Errorf("an hour old: %v", err)
	}
	for _, at := range []int64{now - 3601, now + 3600, now + 301, 1 << 53} {
		if _, _, err := askPrice(t, dated(at), "CHF", none, ""); !errs.Is(err, errs.Rate) {
			t.Errorf("dated %d: %v", at, err)
		}
	}
	if _, _, err := askPrice(t, dated(now+300), "CHF", none, ""); err != nil {
		t.Errorf("300 s ahead: %v", err)
	}
}

func TestListsOfSources(t *testing.T) {
	mine := custom(func(context.Context, string) (Quote, error) { return Quote{Price: "1"}, nil })
	if s, ok := SourcesOf(mine); !ok || len(s) != 1 {
		t.Error("a function alone")
	}
	if s, ok := SourcesOf([]any{mine, "kraken", mine}); !ok || len(s) != 3 || s[1].Name != "kraken" {
		t.Error("functions and names")
	}
	if _, ok := SourcesOf([]any{"kraken", mine, "kraken"}); ok {
		t.Error("a name twice")
	}

	var asked []string
	source := func(name string, quote Quote, err error) Source {
		return custom(func(context.Context, string) (Quote, error) {
			asked = append(asked, name)
			return quote, err
		})
	}
	var warnings []string
	rate, err := FetchFirstRate(context.Background(), []Source{
		source("a", Quote{}, errs.NewNetwork("down", nil)),
		source("b", Quote{}, errors.New("broken")),
		source("c", Quote{Price: "69012.50"}, nil),
		source("d", Quote{Price: "1"}, nil),
	}, "CHF", Context{}, func() int64 { return now }, logx.Func(func(m string) { warnings = append(warnings, m) }), nil)
	if err != nil || rate.Info.Source != "custom" || rate.Info.Price != "69012.5" || strings.Join(asked, "") != "abc" {
		t.Errorf("%v, %v, %v", rate.Info, err, asked)
	}
	want := []string{
		"Rate source custom failed (network): down; trying custom",
		"Rate source custom failed (rate): The custom rate source failed; trying custom",
	}
	if strings.Join(warnings, "\n") != strings.Join(want, "\n") {
		t.Errorf("warnings %q", warnings)
	}

	// A cancellation stops the list, and an error of another kind.
	ctx, cancel := context.WithCancel(context.Background())
	second := false
	list := []Source{
		custom(func(context.Context, string) (Quote, error) { cancel(); return Quote{}, errs.NewNetwork("down", nil) }),
		custom(func(context.Context, string) (Quote, error) { second = true; return Quote{Price: "1"}, nil }),
	}
	if _, err := FetchFirstRate(ctx, list, "CHF", Context{}, func() int64 { return now }, logx.Nop, nil); !errors.Is(err, context.Canceled) || second {
		t.Errorf("cancelled: %v, %v", err, second)
	}
	invalid := errs.NewInvalidOrder("fiat", "not for this shop")
	stop := []Source{custom(func(context.Context, string) (Quote, error) { return Quote{}, invalid }), list[1]}
	if _, err := FetchFirstRate(context.Background(), stop, "CHF", Context{}, func() int64 { return now }, logx.Nop, nil); err != invalid || second {
		t.Errorf("another kind: %v", err)
	}
	own := errs.NewUnsupportedCurrency("XAF", "mine", "")
	if _, err := FetchFirstRate(context.Background(), []Source{custom(func(context.Context, string) (Quote, error) { return Quote{}, own })}, "XAF", Context{}, func() int64 { return now }, logx.Nop, nil); err != own {
		t.Errorf("a single source's own error: %v", err)
	}
	several := []Source{custom(func(context.Context, string) (Quote, error) { return Quote{}, own }), {Name: Mempool}}
	_, err = FetchFirstRate(context.Background(), several, "XAF", Context{}, func() int64 { return now }, logx.Nop, nil)
	if e, ok := errs.Of(err); !ok || e.Code != errs.UnsupportedCurrency || e.Message != "None of custom, mempool has a price in XAF" || e.Currency != "XAF" {
		t.Errorf("none has it: %v", err)
	}
}
