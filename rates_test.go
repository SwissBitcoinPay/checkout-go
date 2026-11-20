package checkout_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	checkout "github.com/sattleme/checkout-go"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// The rate sources as the client takes them; each source's answers and failures, and lists of
// sources, are internal/rates' tests.

func TestTheRateSourcesTheClientKnows(t *testing.T) {
	var names []string
	for _, s := range checkout.DefaultRateSources() {
		names = append(names, s.String())
	}
	same(t, "default", names, []string{"coingecko", "mempool", "coinbase", "kraken", "opennode"})
	for _, s := range []checkout.RateSource{checkout.CoinGecko, checkout.Mempool, checkout.OpenNode, checkout.Coinbase, checkout.Kraken} {
		if s.String() == "" {
			t.Error("a source without a name")
		}
	}
	if checkout.CustomRateSource(func(context.Context, string) (checkout.RateQuote, error) { return checkout.RateQuote{}, nil }).String() != "custom" {
		t.Error("a custom source is custom")
	}
}

func TestExactConversionByTheClient(t *testing.T) {
	sats, err := checkout.FiatToSats(checkout.Price{Amount: "24.50", Currency: "CHF"}, "69012")
	must(t, err)
	if sats.Int64() != 35_502 {
		t.Errorf("%s sats", sats)
	}
	sats, err = checkout.FiatToSats(checkout.Price{Amount: "1.00", Currency: "CHF"}, "100000")
	must(t, err)
	if sats.Int64() != 1000 {
		t.Errorf("%s sats", sats)
	}
	_, err = checkout.FiatToSats(checkout.Price{Amount: "1", Currency: "CHF"}, "0")
	wantCode(t, err, checkout.CodeRate)
	_, err = checkout.FiatToSats(checkout.Price{Amount: "24.505", Currency: "CHF"}, "69012")
	wantCode(t, err, checkout.CodeInvalidOrder, "fiat")
	for code, digits := range map[string]int{"CHF": 2, "JPY": 0, "BHD": 3, "RSD": 0} {
		if got, ok := checkout.CurrencyDigits(code); !ok || got != digits {
			t.Errorf("%s: %d", code, got)
		}
	}
	for _, code := range []string{"ABC", "CHE", "XAU", "XXX", "BTC", "SAT", "chf"} {
		if _, ok := checkout.CurrencyDigits(code); ok {
			t.Errorf("%s is a currency", code)
		}
	}
}

// fiat.json's `digits` (SPEC.md §4, Functions): CurrencyDigits gives the decimals of the currency
// whose code is exactly the text, of the package's table, and none (false) for any other text: in
// lower case, around spaces, a fund, a metal, a test code, none at all, a name an object holds in
// some languages.
func TestCurrencyDigitsVectors(t *testing.T) {
	cases := tv.Objs(t, tv.Load(t, "fiat.json"), "digits")
	for _, c := range cases {
		code := tv.Str(t, c, "code")
		var got any
		if digits, ok := checkout.CurrencyDigits(code); ok {
			got = digits
		}
		same(t, strconv.Quote(code), got, tv.Get(c, "expected"))
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	t.Logf("%d cases", len(cases))
}

func customSource(f func(ctx context.Context, currency string) (checkout.RateQuote, error)) checkout.RateSource {
	return checkout.CustomRateSource(f)
}

func TestACustomSourcesOwnErrorIsGivenBackAsItIs(t *testing.T) {
	m := newMerchant(t)
	own := checkout.NewUnsupportedCurrencyError("XAF", "mine")
	_, err := clientOf(t, m, checkout.WithRateSource(customSource(func(context.Context, string) (checkout.RateQuote, error) {
		return checkout.RateQuote{}, own
	}))).CreateInvoice(ctx, checkout.Order{Ref: "R-1", Fiat: &checkout.Price{Amount: "1", Currency: "XAF"}})
	if err != own {
		t.Fatalf("got %v", err)
	}
	if own.Currency != "XAF" || own.Message != "mine has no price in XAF" || own.Retryable() {
		t.Errorf("%+v", own)
	}
	// An error of another kind stops the list, and goes through as it is.
	invalid := &checkout.Error{Code: checkout.CodeInvalidOrder, Param: "fiat", Message: "fiat: not for this shop"}
	asked := false
	_, err = clientOf(t, m, checkout.WithRateSource(
		customSource(func(context.Context, string) (checkout.RateQuote, error) { return checkout.RateQuote{}, invalid }),
		customSource(func(context.Context, string) (checkout.RateQuote, error) {
			asked = true
			return checkout.RateQuote{Price: "1"}, nil
		}),
	)).CreateInvoice(ctx, checkout.Order{Ref: "R-2", Fiat: &checkout.Price{Amount: "1", Currency: "CHF"}})
	if err != invalid || asked {
		t.Fatalf("got %v, the next asked %v", err, asked)
	}
	// Any other error of yours becomes a rate error, yours as its cause.
	broken := errors.New("broken")
	_, err = clientOf(t, m, checkout.WithRateSource(customSource(func(context.Context, string) (checkout.RateQuote, error) {
		return checkout.RateQuote{}, broken
	}))).CreateInvoice(ctx, checkout.Order{Ref: "R-3", Fiat: &checkout.Price{Amount: "1", Currency: "CHF"}})
	e := wantCode(t, err, checkout.CodeRate)
	if !errors.Is(err, broken) || e.Message != "The custom rate source failed" || !e.Retryable() {
		t.Errorf("%v", err)
	}
	// Network and rate errors of yours: the next source is asked.
	logger := quiet()
	invoice, err := clientOf(t, m, checkout.WithLogger(logger), checkout.WithRateSource(
		customSource(func(context.Context, string) (checkout.RateQuote, error) {
			return checkout.RateQuote{}, checkout.NewNetworkError("down", nil)
		}),
		customSource(func(context.Context, string) (checkout.RateQuote, error) {
			return checkout.RateQuote{}, checkout.NewRateError("no price now", nil)
		}),
		customSource(func(context.Context, string) (checkout.RateQuote, error) {
			return checkout.RateQuote{Price: "69012.50", Time: time.Unix(m.Now()-5, 0)}, nil
		}),
	)).CreateInvoice(ctx, checkout.Order{Ref: "R-4", Fiat: &checkout.Price{Amount: "24.50", Currency: "CHF"}})
	must(t, err)
	if invoice.Rate.Source != "custom" || invoice.Rate.Price != "69012.5" || invoice.Rate.Time == nil || *invoice.Rate.Time != m.Now()-5 {
		t.Errorf("%+v", invoice.Rate)
	}
	same(t, "warnings", logger.Messages(), []string{
		"Rate source custom failed (network): down; trying custom",
		"Rate source custom failed (rate): no price now; trying custom",
	})
}

func TestACustomSourceGetsTheCallsContext(t *testing.T) {
	m := newMerchant(t)
	call, cancel := context.WithCancel(ctx)
	_, err := clientOf(t, m, checkout.WithRateSource(customSource(func(ctx context.Context, string2 string) (checkout.RateQuote, error) {
		cancel()
		<-ctx.Done()
		return checkout.RateQuote{}, ctx.Err()
	}))).CreateInvoice(call, checkout.Order{Ref: "R-5", Fiat: &checkout.Price{Amount: "1", Currency: "CHF"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	// A dated price too old, or from the future: refused.
	for _, at := range []int64{m.Now() - 3601, m.Now() + 301} {
		at := at
		_, err := clientOf(t, m, checkout.WithRateSource(customSource(func(context.Context, string) (checkout.RateQuote, error) {
			return checkout.RateQuote{Price: "69000", Time: time.Unix(at, 0)}, nil
		}))).CreateInvoice(ctx, checkout.Order{Ref: "R-6", Fiat: &checkout.Price{Amount: "1", Currency: "CHF"}})
		wantCode(t, err, checkout.CodeRate)
	}
}
