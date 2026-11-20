// Package rates converts amounts in a currency to sats on the integrator's server, at the rate of
// the source it chooses (SPEC.md §25, §26). Amounts are exact integers end to end: the price of one
// bitcoin is read as the exact decimal its source wrote, and the sats are rounded up to the next
// whole sat, as the merchant's point of sale does: the merchant never receives less than the price.
package rates

import (
	"context"
	"errors"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"github.com/sattleme/checkout-go/internal/currencies"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/httpx"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/logx"
	"github.com/sattleme/checkout-go/internal/model"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/text"
)

// Decimal is an exact positive decimal: Units × 10^-Scale, written shortest (Units has no
// trailing zero when Scale is above 0).
type Decimal struct {
	Units *big.Int
	Scale int
}

var decimalPattern = regexp.MustCompile(`^([0-9]+)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)

var (
	ten = big.NewInt(10)
	// satsPerBTC is 10^8.
	satsPerBTC = big.NewInt(proto.SatsPerBTC)
)

func pow10(n int) *big.Int { return new(big.Int).Exp(ten, big.NewInt(int64(n)), nil) }

// ParseDecimal reads a price as an exact decimal: digits, an optional fraction and an optional
// exponent ("69025.1084416666694295", "8e-8"), 64 characters at most, an exponent of 64 at most
// either way, nothing around it. False unless it is strictly positive.
func ParseDecimal(s string) (Decimal, bool) {
	if len(s) > proto.MaxPriceLength {
		return Decimal{}, false
	}
	m := decimalPattern.FindStringSubmatch(s)
	if m == nil {
		return Decimal{}, false
	}
	exponent := 0
	if m[3] != "" {
		// At most 64 characters: the exponent's digits fit an int.
		e, err := strconv.Atoi(m[3])
		if err != nil {
			return Decimal{}, false
		}
		exponent = e
	}
	if exponent > proto.MaxPriceExponent || exponent < -proto.MaxPriceExponent {
		return Decimal{}, false
	}
	units, _ := new(big.Int).SetString(m[1]+m[2], 10)
	scale := len(m[2]) - exponent
	if scale < 0 {
		units.Mul(units, pow10(-scale))
		scale = 0
	}
	// The shortest exact writing: 69012.50 is 69012.5.
	remainder := new(big.Int)
	for scale > 0 {
		quotient, r := new(big.Int).QuoRem(units, ten, remainder)
		if r.Sign() != 0 {
			break
		}
		units = quotient
		scale--
	}
	if units.Sign() <= 0 {
		return Decimal{}, false
	}
	return Decimal{Units: units, Scale: scale}, true
}

// NumberText is the text of a JSON number a source answered: the shortest decimal that reads
// back as the same IEEE 754 double (ECMAScript's Number::toString). False when it is not finite.
func NumberText(f float64) (string, bool) {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return "", false
	}
	return jsonv.NumberString(f), true
}

// Format writes a decimal plainly: 69012.5, 0.00000008.
func (d Decimal) Format() string {
	digits := d.Units.String()
	if d.Scale == 0 {
		return digits
	}
	if len(digits) < d.Scale+1 {
		digits = strings.Repeat("0", d.Scale+1-len(digits)) + digits
	}
	return digits[:len(digits)-d.Scale] + "." + digits[len(digits)-d.Scale:]
}

var amountPattern = regexp.MustCompile(`^([0-9]{1,15})(?:\.([0-9]{1,15}))?$`)

// ParseFiat reads a price given as an amount and a currency as integer minor units of the
// currency (SPEC.md §25). The currency is an upper-case ISO 4217 code of the table; the amount a
// string of digits with at most the currency's decimals (trailing zeros aside), nothing around it:
// never a float, never rounded. invalid_order (fiat) otherwise.
func ParseFiat(amount, currency string) (model.Fiat, error) {
	digits, ok := currencies.Digits(currency)
	if !ok {
		return model.Fiat{}, errs.NewInvalidOrder("fiat", "currency: an ISO 4217 code in upper case, as CHF")
	}
	m := amountPattern.FindStringSubmatch(amount)
	if m == nil {
		return model.Fiat{}, errs.NewInvalidOrder("fiat", `amount: digits, as "24.50"`)
	}
	fraction := strings.TrimRight(m[2], "0")
	if len(fraction) > digits {
		return model.Fiat{}, errs.NewInvalidOrder("fiat", "amount: "+currency+" has "+strconv.Itoa(digits)+" decimals at most")
	}
	whole, _ := new(big.Int).SetString(m[1], 10)
	minor := whole.Mul(whole, pow10(digits))
	if part := fraction + strings.Repeat("0", digits-len(fraction)); part != "" {
		p, _ := new(big.Int).SetString(part, 10)
		minor.Add(minor, p)
	}
	if minor.Sign() < 1 || minor.Cmp(big.NewInt(proto.MaxMinorUnits)) > 0 {
		return model.Fiat{}, errs.NewInvalidOrder("fiat", "amount: more than 0, and not that large")
	}
	return model.Fiat{
		Amount:   Decimal{Units: minor, Scale: digits}.Format(),
		Currency: currency,
		Minor:    minor.Int64(),
		Digits:   digits,
	}, nil
}

// MinorToSats is the sats for minor units of a currency with digits decimals, at price (one
// bitcoin in that currency), rounded up to the next whole sat, on integers:
// ⌈minor × 10^8 × 10^scale / (units × 10^digits)⌉.
func MinorToSats(minor int64, digits int, price Decimal) *big.Int {
	dividend := new(big.Int).Mul(big.NewInt(minor), satsPerBTC)
	dividend.Mul(dividend, pow10(price.Scale))
	divisor := new(big.Int).Mul(price.Units, pow10(digits))
	dividend.Add(dividend, divisor)
	dividend.Sub(dividend, big.NewInt(1))
	return dividend.Quo(dividend, divisor)
}

// FiatToSats is the sats for a price in a currency at the given price of one bitcoin (a decimal
// string): exact, rounded up. invalid_order for an invalid price in a currency, rate for an
// invalid price of one bitcoin.
func FiatToSats(amount, currency, price string) (*big.Int, error) {
	fiat, err := ParseFiat(amount, currency)
	if err != nil {
		return nil, err
	}
	decimal, ok := ParseDecimal(price)
	if !ok {
		return nil, errs.NewRate("A price must be a decimal string above 0", nil)
	}
	return MinorToSats(fiat.Minor, fiat.Digits, decimal), nil
}

// Quote is a price of one bitcoin, as a source gives it: an exact decimal string, and when the
// source set it (nil when it does not say).
type Quote struct {
	Price string
	Time  *int64
}

// CustomFunc is any other source: the price of one bitcoin in currency (ISO 4217, upper case).
// unsupported_currency when it has no price in that currency (an error of the package goes
// through as it is); any other failure becomes rate.
type CustomFunc func(ctx context.Context, currency string) (Quote, error)

// Source is a rate source: a name the package knows, or a custom one (Custom set, Name "custom").
type Source struct {
	Name   string
	Custom CustomFunc
}

// The names of the sources the package knows.
const (
	CoinGecko = "coingecko"
	Mempool   = "mempool"
	OpenNode  = "opennode"
	Coinbase  = "coinbase"
	Kraken    = "kraken"
	// CustomName is the RateInfo source of a custom source: never a name of the list.
	CustomName = "custom"
)

// Names are the names of the sources the package knows.
var Names = []string{CoinGecko, Mempool, OpenNode, Coinbase, Kraken}

// Default is the list asked when the client is given none, in this order (SPEC.md §26): the dated
// ones first (their price's age is checked), CoinGecko for its currencies, then mempool.space for
// its 7; then the undated ones, Coinbase and Kraken (two exchanges), and OpenNode.
func Default() []Source {
	return []Source{{Name: CoinGecko}, {Name: Mempool}, {Name: Coinbase}, {Name: Kraken}, {Name: OpenNode}}
}

func isName(name string) bool {
	for _, n := range Names {
		if n == name {
			return true
		}
	}
	return false
}

// sourceOf reads a source: a Source, or a name.
func sourceOf(v any) (Source, bool) {
	switch v := v.(type) {
	case Source:
		if v.Custom != nil {
			return Source{Name: CustomName, Custom: v.Custom}, true
		}
		return v, isName(v.Name)
	case CustomFunc:
		return Source{Name: CustomName, Custom: v}, v != nil
	case string:
		return Source{Name: v}, isName(v)
	}
	return Source{}, false
}

// SourcesOf reads what a rateSource names, in the order its sources are asked: a source alone
// (a name, a custom source), or a non-empty list of sources without a name twice. False when it is
// neither (an empty list, an unknown name or one in another case, "custom", a name twice, a list
// inside a list).
func SourcesOf(v any) ([]Source, bool) {
	if s, ok := sourceOf(v); ok {
		return []Source{s}, true
	}
	var items []any
	switch list := v.(type) {
	case []any:
		items = list
	case []Source:
		for _, s := range list {
			items = append(items, s)
		}
	default:
		return nil, false
	}
	if len(items) == 0 {
		return nil, false
	}
	sources := make([]Source, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		s, ok := sourceOf(item)
		if !ok {
			return nil, false
		}
		if s.Custom == nil {
			if seen[s.Name] {
				return nil, false
			}
			seen[s.Name] = true
		}
		sources = append(sources, s)
	}
	return sources, true
}

// Context is what a source may use.
type Context struct {
	HTTP *httpx.Client
	// CoinGeckoAPIKey is CoinGecko's Demo key, for coingecko alone ("" for none).
	CoinGeckoAPIKey string
}

// Rate is a source's price of one bitcoin, checked, and what an Invoice says of it.
type Rate struct {
	Price Decimal
	Info  model.RateInfo
}

// FetchRate asks one source for the price of one bitcoin in currency, checked (SPEC.md §25): an
// exact positive decimal, not older than an hour, not further ahead than the clock tolerance.
// Undated sources (OpenNode, Coinbase, Kraken, most custom ones) cannot be checked for age.
// unsupported_currency when the source has no price in the currency, network when it does not
// answer, rate for anything else; ctx's error when ctx is done.
func FetchRate(ctx context.Context, source Source, currency string, c Context, now int64) (Rate, error) {
	quote, err := ask(ctx, source, currency, c)
	if err != nil {
		return Rate{}, err
	}
	price, ok := ParseDecimal(quote.Price)
	if !ok {
		return Rate{}, errs.NewRate(source.Name+" gave no valid price in "+currency, nil)
	}
	if quote.Time != nil {
		t := *quote.Time
		if t > jsonv.MaxSafeInteger || t < -jsonv.MaxSafeInteger || t > now+proto.ClockToleranceSeconds {
			return Rate{}, errs.NewRate(source.Name+" gave an invalid date for its price", nil)
		}
		if now-t > proto.MaxRateAgeSeconds {
			return Rate{}, errs.NewRate(source.Name+"'s price in "+currency+" is too old", nil)
		}
	}
	return Rate{
		Price: price,
		Info: model.RateInfo{
			Source:    source.Name,
			Currency:  currency,
			Price:     price.Format(),
			FetchedAt: now,
			Time:      quote.Time,
		},
	}, nil
}

func ask(ctx context.Context, source Source, currency string, c Context) (Quote, error) {
	if source.Custom != nil {
		quote, err := source.Custom(ctx, currency)
		if err != nil {
			if ctx.Err() != nil {
				return Quote{}, ctx.Err()
			}
			if _, ok := errs.Of(err); ok {
				return Quote{}, err
			}
			return Quote{}, errs.NewRate("The custom rate source failed", err)
		}
		return quote, nil
	}
	switch source.Name {
	case CoinGecko:
		return coingecko(ctx, currency, c)
	case Mempool:
		return mempool(ctx, currency, c)
	case OpenNode:
		return opennode(ctx, currency, c)
	case Coinbase:
		return coinbase(ctx, currency, c)
	case Kraken:
		return kraken(ctx, currency, c)
	}
	return Quote{}, errs.NewRate("No rate source named "+source.Name, nil)
}

// Ask asks one source: FetchRate, or a stand-in for the tests.
type Ask func(ctx context.Context, source Source, currency string, c Context, now int64) (Rate, error)

// FetchFirstRate is the first usable price of sources, asked one at a time in their order
// (SPEC.md §26): the next source only when the previous one gives no usable price (network, rate
// or unsupported_currency, each logged). A cancellation, or any other error a custom source
// raises, stops the list. With no price: unsupported_currency when no source has the currency,
// else the last network or rate error.
func FetchFirstRate(ctx context.Context, sources []Source, currency string, c Context, now func() int64, logger logx.Logger, askOne Ask) (Rate, error) {
	if askOne == nil {
		askOne = FetchRate
	}
	var failure, unsupported error
	for i, source := range sources {
		rate, err := askOne(ctx, source, currency, c, now())
		if err == nil {
			return rate, nil
		}
		if ctx.Err() != nil {
			return Rate{}, ctx.Err()
		}
		e, ok := errs.Of(err)
		switch {
		case ok && e.Code == errs.UnsupportedCurrency:
			unsupported = err
		case ok && (e.Code == errs.Network || e.Code == errs.Rate):
			failure = err
		default:
			return Rate{}, err
		}
		if i+1 < len(sources) {
			logger.Warn("Rate source " + source.Name + " failed (" + string(e.Code) + "): " + e.Message + "; trying " + sources[i+1].Name)
		}
	}
	if failure != nil {
		return Rate{}, failure
	}
	if len(sources) > 1 {
		names := make([]string, len(sources))
		for i, s := range sources {
			names[i] = s.Name
		}
		list := strings.Join(names, ", ")
		return Rate{}, errs.NewUnsupportedCurrency(currency, list, "None of "+list+" has a price in "+currency)
	}
	if unsupported == nil {
		return Rate{}, errors.New("rates: no source")
	}
	return Rate{}, unsupported
}

// sourceJSON asks for a source's JSON object: network without an answer, rate for anything else.
func sourceJSON(ctx context.Context, name, rawURL string, c Context, headers map[string]string) (*jsonv.Object, error) {
	a, err := c.HTTP.GetJSON(ctx, rawURL, headers)
	if err != nil {
		return nil, err
	}
	if !a.OK {
		// Only 500 to 599, 408 and 429 say "try again" (SPEC.md §26); 600 and above do not.
		if a.Status >= 500 && a.Status <= 599 || a.Status == 408 || a.Status == 429 {
			return nil, errs.NewNetwork(name+" answered HTTP "+strconv.Itoa(a.Status), nil)
		}
		return nil, errs.NewRate(name+" answered HTTP "+strconv.Itoa(a.Status), nil)
	}
	o, ok := jsonv.AsObject(a.Body)
	if !ok {
		return nil, errs.NewRate(name+" gave no JSON object", nil)
	}
	return o, nil
}

func objectAt(v any, present bool, name string) (*jsonv.Object, error) {
	o, ok := jsonv.AsObject(v)
	if !present || !ok {
		return nil, errs.NewRate(name+": unexpected answer", nil)
	}
	return o, nil
}

// priceOf is a source's price: a string as written, a JSON number as its shortest decimal
// (NumberText); absent, unsupported_currency.
func priceOf(v any, present bool, name, currency string) (string, error) {
	if !present {
		return "", errs.NewUnsupportedCurrency(currency, name, "")
	}
	switch v := v.(type) {
	case string:
		return v, nil
	case float64:
		if s, ok := NumberText(v); ok {
			return s, nil
		}
	}
	return "", errs.NewRate(name+": unexpected price", nil)
}

// timeOf reads a source's date: a JSON number as it is (FetchRate checks it is a safe integer).
func timeOf(v any) (*int64, bool) {
	f, ok := v.(float64)
	if !ok {
		return nil, false
	}
	if !jsonv.IsSafeIntegerFloat(f) {
		// Not a safe integer: an invalid date, which FetchRate refuses.
		t := int64(jsonv.MaxSafeInteger + 1)
		return &t, true
	}
	t := int64(f)
	return &t, true
}

// coingecko is CoinGecko (/simple/price, dated with include_last_updated_at). Without a key,
// limited by IP and, by CoinGecko's terms, not meant for production: give a Demo key.
func coingecko(ctx context.Context, currency string, c Context) (Quote, error) {
	code := text.LowerASCII(currency)
	u := "https://api.coingecko.com/api/v3/simple/price?ids=bitcoin&vs_currencies=" + code + "&include_last_updated_at=true"
	var headers map[string]string
	if c.CoinGeckoAPIKey != "" {
		headers = map[string]string{"x-cg-demo-api-key": c.CoinGeckoAPIKey}
	}
	body, err := sourceJSON(ctx, CoinGecko, u, c, headers)
	if err != nil {
		return Quote{}, err
	}
	bitcoinValue, has := body.Get("bitcoin")
	bitcoin, err := objectAt(bitcoinValue, has, CoinGecko)
	if err != nil {
		return Quote{}, err
	}
	priceValue, has := bitcoin.Get(code)
	price, err := priceOf(priceValue, has, CoinGecko, currency)
	if err != nil {
		return Quote{}, err
	}
	timeValue, _ := bitcoin.Get("last_updated_at")
	t, _ := timeOf(timeValue)
	return Quote{Price: price, Time: t}, nil
}

// MempoolCurrencies are the currencies of mempool.space's /api/v1/prices.
var MempoolCurrencies = []string{"USD", "EUR", "GBP", "CAD", "CHF", "AUD", "JPY"}

// mempool is mempool.space (/api/v1/prices, dated): 7 currencies.
func mempool(ctx context.Context, currency string, c Context) (Quote, error) {
	supported := false
	for _, code := range MempoolCurrencies {
		supported = supported || code == currency
	}
	if !supported {
		return Quote{}, errs.NewUnsupportedCurrency(currency, Mempool, "")
	}
	body, err := sourceJSON(ctx, Mempool, "https://mempool.space/api/v1/prices", c, nil)
	if err != nil {
		return Quote{}, err
	}
	timeValue, _ := body.Get("time")
	t, ok := timeOf(timeValue)
	if !ok {
		return Quote{}, errs.NewRate("mempool: no time", nil)
	}
	priceValue, has := body.Get(currency)
	price, err := priceOf(priceValue, has, Mempool, currency)
	if err != nil {
		return Quote{}, err
	}
	return Quote{Price: price, Time: t}, nil
}

// opennode is OpenNode (/v1/rates): some 140 currencies, undated.
func opennode(ctx context.Context, currency string, c Context) (Quote, error) {
	body, err := sourceJSON(ctx, OpenNode, "https://api.opennode.com/v1/rates", c, nil)
	if err != nil {
		return Quote{}, err
	}
	dataValue, has := body.Get("data")
	data, err := objectAt(dataValue, has, OpenNode)
	if err != nil {
		return Quote{}, err
	}
	pairValue, has := data.Get("BTC" + currency)
	if !has {
		return Quote{}, errs.NewUnsupportedCurrency(currency, OpenNode, "")
	}
	pair, err := objectAt(pairValue, true, OpenNode)
	if err != nil {
		return Quote{}, err
	}
	priceValue, has := pair.Get(currency)
	price, err := priceOf(priceValue, has, OpenNode, currency)
	return Quote{Price: price}, err
}

// coinbase is Coinbase (/v2/exchange-rates?currency=BTC): prices as decimal strings, read as
// written.
func coinbase(ctx context.Context, currency string, c Context) (Quote, error) {
	body, err := sourceJSON(ctx, Coinbase, "https://api.coinbase.com/v2/exchange-rates?currency=BTC", c, nil)
	if err != nil {
		return Quote{}, err
	}
	dataValue, has := body.Get("data")
	data, err := objectAt(dataValue, has, Coinbase)
	if err != nil {
		return Quote{}, err
	}
	if base, _ := data.Get("currency"); base != "BTC" {
		return Quote{}, errs.NewRate("coinbase: not the rates of BTC", nil)
	}
	ratesValue, has := data.Get("rates")
	rates, err := objectAt(ratesValue, has, Coinbase)
	if err != nil {
		return Quote{}, err
	}
	priceValue, has := rates.Get(currency)
	price, err := priceOf(priceValue, has, Coinbase, currency)
	return Quote{Price: price}, err
}

// kraken is Kraken (/0/public/Ticker?pair=XBT<currency>): the last trade's price (c[0]). The
// result's key is not always the pair asked (XBTUSD gives XXBTZUSD): its only entry is read.
func kraken(ctx context.Context, currency string, c Context) (Quote, error) {
	body, err := sourceJSON(ctx, Kraken, "https://api.kraken.com/0/public/Ticker?pair=XBT"+currency, c, nil)
	if err != nil {
		return Quote{}, err
	}
	errorsValue, _ := body.Get("error")
	list, ok := errorsValue.([]any)
	if !ok {
		return Quote{}, errs.NewRate("kraken: unexpected answer", nil)
	}
	for _, e := range list {
		if s, ok := e.(string); ok && strings.HasPrefix(s, "EQuery:Unknown asset pair") {
			return Quote{}, errs.NewUnsupportedCurrency(currency, Kraken, "")
		}
	}
	if len(list) > 0 {
		return Quote{}, errs.NewRate("kraken: "+jsString(list[0]), nil)
	}
	resultValue, has := body.Get("result")
	result, err := objectAt(resultValue, has, Kraken)
	if err != nil {
		return Quote{}, err
	}
	if result.Len() != 1 {
		return Quote{}, errs.NewRate("kraken: unexpected answer", nil)
	}
	entryValue, _ := result.Get(result.Keys()[0])
	entry, err := objectAt(entryValue, true, Kraken)
	if err != nil {
		return Quote{}, err
	}
	lastValue, _ := entry.Get("c")
	last, ok := lastValue.([]any)
	if !ok {
		return Quote{}, errs.NewRate("kraken: unexpected answer", nil)
	}
	if len(last) == 0 {
		return Quote{}, errs.NewUnsupportedCurrency(currency, Kraken, "")
	}
	price, err := priceOf(last[0], true, Kraken, currency)
	return Quote{Price: price}, err
}

// jsString writes a JSON value as JavaScript's String() does, for a message.
func jsString(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(v)
	case float64:
		if s, ok := NumberText(v); ok {
			return s
		}
		if math.IsNaN(v) {
			return "NaN"
		}
		if v > 0 {
			return "Infinity"
		}
		return "-Infinity"
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			if item != nil {
				parts[i] = jsString(item)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}
