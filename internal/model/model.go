// Package model is the data model of SPEC.md §3 as the engine handles it: amounts as integers
// (sats, minor units), dates as Unix seconds, absent values as nil pointers or empty strings. The
// public package checkout gives the same values to the integrator, and writes them as the JSON of
// §3.
package model

// Fiat is a price in a currency: Minor units with Digits decimals ("24.50" CHF is 2450, 2).
type Fiat struct {
	// Amount is the price as written, with the currency's decimals: "24.50".
	Amount string
	// Currency is ISO 4217, upper case.
	Currency string
	// Minor is the price in the currency's minor units.
	Minor int64
	// Digits is the currency's decimals (ISO 4217 minor units).
	Digits int
}

// RateInfo is the rate an amount in a currency was converted at.
type RateInfo struct {
	// Source is "coingecko", "mempool", "opennode", "coinbase", "kraken" or "custom".
	Source   string
	Currency string
	// Price is the price of one bitcoin in Currency, an exact decimal, shortest.
	Price string
	// FetchedAt is when the package read it.
	FetchedAt int64
	// Time is when the source says the price was set; nil when it does not say.
	Time *int64
}

// Invoice is what createInvoice returns.
type Invoice struct {
	Ref string
	// Bolt11 is "" for an order paid on-chain only; PaymentHash and VerifyURL too.
	Bolt11      string
	PaymentHash string
	VerifyURL   string
	// OnchainAddress is "" without one.
	OnchainAddress string
	PaymentURI     string
	Sats           int64
	Fiat           *Fiat
	Rate           *RateInfo
	Note           string
	CreatedAt      int64
	ExpiresAt      int64
	// CheckoutURL and RedirectURL are "" without them.
	CheckoutURL string
	RedirectURL string
}

// DeclaredInvoice is an invoice as the merchant's registry holds it: the fields of an Invoice but
// Rate, CheckoutURL and RedirectURL, and DeclaredAt.
type DeclaredInvoice struct {
	Ref            string
	Bolt11         string
	PaymentHash    string
	VerifyURL      string
	OnchainAddress string
	PaymentURI     string
	Sats           int64
	Fiat           *Fiat
	Note           string
	CreatedAt      int64
	ExpiresAt      int64
	// DeclaredAt is the created_at of its declaration, else of its lease.
	DeclaredAt int64
}

// ToCheck is what a payment check needs of a declared invoice.
func (d *DeclaredInvoice) ToCheck() InvoiceToCheck {
	return InvoiceToCheck{
		Ref:            d.Ref,
		Bolt11:         d.Bolt11,
		PaymentHash:    d.PaymentHash,
		VerifyURL:      d.VerifyURL,
		OnchainAddress: d.OnchainAddress,
		Sats:           d.Sats,
		ExpiresAt:      d.ExpiresAt,
	}
}

// InvoiceToCheck is what a payment check needs, checked (SPEC.md §22).
type InvoiceToCheck struct {
	// Ref is needed with an address: the witnesses of the payment are its order's.
	Ref            string
	Bolt11         string
	PaymentHash    string
	VerifyURL      string
	OnchainAddress string
	Sats           int64
	ExpiresAt      int64
}

// Rest is what is left to pay of an invoice underpaid on-chain (PaymentStatus open).
type Rest struct {
	Sats int64
	// Bolt11 and PaymentURI are "" while there are none.
	Bolt11     string
	PaymentURI string
}

// The states of a payment.
const (
	Open        = "open"
	Unconfirmed = "unconfirmed"
	Paid        = "paid"
	Expired     = "expired"
)

// The methods of a paid invoice.
const (
	Lightning = "lightning"
	Onchain   = "onchain"
	Mixed     = "mixed"
)

// PaymentStatus is where the payment of an invoice is (SPEC.md §3): the fields of its State (and
// Method) alone are meaningful.
type PaymentStatus struct {
	State  string
	Method string
	// ExpiresAt: open.
	ExpiresAt int64
	// Received: open, unconfirmed, paid on-chain or mixed, expired.
	Received int64
	// Rest: open; nil when there is none.
	Rest *Rest
	// Confirmations: unconfirmed.
	Confirmations int
	// Txids: unconfirmed, paid on-chain or mixed.
	Txids []string
	// Preimage: paid in Lightning.
	Preimage string
	// Preimages: paid mixed.
	Preimages []string
}

// Merchant is the merchant, as the integration's channel, Lightning address and lot give it.
type Merchant struct {
	BusinessName     string
	MinSats          int64
	MaxSats          int64
	OnchainAddresses int
}
