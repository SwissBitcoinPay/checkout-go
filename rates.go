package checkout

import (
	"context"
	"math/big"
	"time"

	"github.com/sattleme/checkout-go/internal/currencies"
	"github.com/sattleme/checkout-go/internal/rates"
)

// RateSource is where an invoice priced in a currency gets its rate (SPEC.md §26): one of the
// sources the package knows (CoinGecko, Mempool, OpenNode, Coinbase, Kraken), or your own
// function (CustomRateSource). Its zero value is no source.
type RateSource struct {
	name   string
	custom RateFunc
}

// The rate sources the package knows.
var (
	// CoinGecko: many currencies, dated. No key needed; optional: a Demo key
	// (WithCoinGeckoAPIKey), sent to CoinGecko only. Without one, limited by IP.
	CoinGecko = RateSource{name: rates.CoinGecko}
	// Mempool is mempool.space: USD EUR GBP CAD CHF AUD JPY, dated.
	Mempool = RateSource{name: rates.Mempool}
	// OpenNode: about 140 currencies, undated.
	OpenNode = RateSource{name: rates.OpenNode}
	// Coinbase: many currencies, undated; prices are decimal strings, read as written.
	Coinbase = RateSource{name: rates.Coinbase}
	// Kraken: Kraken's pairs, undated: the last trade's price.
	Kraken = RateSource{name: rates.Kraken}
)

// DefaultRateSources is the list asked when the client is given none, in this order: the dated
// ones first (a dated price is checked for its age), then the undated ones. CoinGecko, Mempool,
// Coinbase, Kraken, OpenNode.
func DefaultRateSources() []RateSource {
	return []RateSource{CoinGecko, Mempool, Coinbase, Kraken, OpenNode}
}

// RateFunc gives the price of one bitcoin in currency (ISO 4217, upper case). It returns
// NewUnsupportedCurrencyError when it has no price in that currency, NewNetworkError or
// NewRateError when it cannot give one now: the next source of the list is then asked. Any other
// error becomes a rate error (yours as its cause); an Error of this package of another code stops
// the list and is given back as it is. ctx is the call's.
type RateFunc func(ctx context.Context, currency string) (RateQuote, error)

// RateQuote is a price of one bitcoin, as a source gives it.
type RateQuote struct {
	// Price is the price as an exact decimal string, as its source wrote it ("69012.5"): never
	// from a float, which may already have lost digits.
	Price string
	// Time is when the source set it; zero when it does not say. A dated price more than an hour
	// old, or more than 5 minutes ahead, is refused.
	Time time.Time
}

// CustomRateSource is a rate source of yours: any other source (a CoinGecko Pro key, your own
// mempool instance, several sources checked against each other). Its rate's Source is "custom".
func CustomRateSource(f RateFunc) RateSource {
	return RateSource{name: rates.CustomName, custom: f}
}

// String is the source's name: "coingecko"…, "custom" for a function of yours.
func (s RateSource) String() string { return s.name }

// internalSource is the engine's source of s; its custom function's errors keep their codes.
func (s RateSource) internalSource() rates.Source {
	if s.custom == nil {
		if s.name == rates.CustomName {
			return rates.Source{}
		}
		return rates.Source{Name: s.name}
	}
	f := s.custom
	return rates.Source{Name: rates.CustomName, Custom: func(ctx context.Context, currency string) (rates.Quote, error) {
		quote, err := f(ctx, currency)
		if err != nil {
			return rates.Quote{}, internalError(err)
		}
		q := rates.Quote{Price: quote.Price}
		if !quote.Time.IsZero() {
			t := quote.Time.Unix()
			q.Time = &t
		}
		return q, nil
	}}
}

// sourcesOf is what the engine reads of a list of sources.
func sourcesOf(list []RateSource) any {
	items := make([]any, len(list))
	for i, s := range list {
		items[i] = s.internalSource()
	}
	return items
}

// CurrencyDigits is the decimals of a currency's minor unit (2 for CHF, 0 for JPY, 3 for KWD),
// false when it is not a currency an order may be priced in (SPEC.md §4, currencyDigits): currency
// is exactly an upper-case ISO 4217 code of the package's own table (ISO 4217 List One, without
// funds, metals and test codes), the merchant's app's; any other text is none ("chf", " CHF").
// It needs no client and asks nothing: whether a shop may offer its customers to pay in its
// currency, and in how many decimals its prices go to CreateInvoice.
func CurrencyDigits(currency string) (int, bool) {
	return currencies.Digits(currency)
}

// FiatToSats is the sats for a price in a currency at the given price of one bitcoin (a decimal
// string): exact, rounded up to the next whole sat. FiatToSats(Price{Amount: "24.50", Currency:
// "CHF"}, "69012") is 35502 (the exact quotient is 35501.07…). invalid_order for an invalid price
// in a currency, rate for an invalid price of one bitcoin.
func FiatToSats(price Price, btcPrice string) (*big.Int, error) {
	sats, err := rates.FiatToSats(price.Amount, price.Currency, btcPrice)
	if err != nil {
		return nil, publicError(err)
	}
	return sats, nil
}
