package checkout

import (
	"strconv"

	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/model"
)

// The data model (SPEC.md §3): what the client gives and takes. Every value an integrator stores
// is written as the JSON of the specification by its MarshalJSON, with its field names, amounts as
// strings of digits, dates as integers of Unix seconds and absent values as null, and read back by
// its UnmarshalJSON: an invoice stored by another implementation (checkout-node, checkout-php…)
// is checked by this one, and the other way round. In Go, an absent string is "", an absent
// object nil.

// Fiat is a price in a currency: Minor units with Digits decimals ("2450", 2, "CHF" is 24.50 CHF).
type Fiat struct {
	// Amount is the price as written, with the currency's decimals: "24.50".
	Amount string
	// Currency is ISO 4217, upper case: "CHF".
	Currency string
	// Minor is the price in the currency's minor units: 2450 (JSON: "2450").
	Minor int64
	// Digits is the currency's decimals (ISO 4217 minor units): 2.
	Digits int
}

// RateInfo is the rate an amount in a currency was converted at: for the audit, and to answer a
// customer.
type RateInfo struct {
	// Source is "coingecko", "mempool", "opennode", "coinbase", "kraken" or "custom".
	Source   string
	Currency string
	// Price is the price of one bitcoin in Currency, an exact decimal, shortest: "69012.5".
	Price string
	// FetchedAt is when the package read it (Unix seconds).
	FetchedAt int64
	// Time is when the source says the price was set (Unix seconds), nil when it does not say.
	Time *int64
}

// Invoice is the invoice of an order (CreateInvoice): store it with the order. The shop's page
// needs PaymentURI (or Bolt11 and OnchainAddress) and ExpiresAt only. An order is paid with a
// Lightning invoice, a deposit address, or both: at least one of Bolt11 and OnchainAddress is set.
type Invoice struct {
	// Ref is the order's reference.
	Ref string
	// Bolt11 is the BOLT11 invoice, as the Lightning address wrote it: what the customer's wallet
	// pays. "" for an order paid on-chain only, as PaymentHash and VerifyURL.
	Bolt11 string
	// PaymentHash is its payment hash (hex).
	PaymentHash string
	// VerifyURL is its LUD-21 verify URL (https).
	VerifyURL string
	// OnchainAddress is the deposit address of the merchant's wallet the order leased (bc1p…),
	// "" without one.
	OnchainAddress string
	// PaymentURI is what the shop's page shows as one QR code: bitcoin:<address>?amount=…&lightning=…
	// with an address, else lightning:<bolt11>.
	PaymentURI string
	// Sats is the amount: the invoice's, and what the address must receive (JSON: a string of
	// digits).
	Sats int64
	// Fiat is the order's price when it was given in a currency, else nil.
	Fiat *Fiat
	// Rate is the rate Fiat was converted at, else nil.
	Rate *RateInfo
	// Note is what the merchant's app shows: the note, else the reference.
	Note string
	// CreatedAt is the invoice's date (Unix seconds): its BOLT11 timestamp; without Bolt11, when
	// the address was leased.
	CreatedAt int64
	// ExpiresAt is when it can no longer be paid (Unix seconds).
	ExpiresAt int64
	// CheckoutURL is the URL of its checkout link, where the shop sends its customer to pay (the
	// order asked for one: Checkout), else "".
	CheckoutURL string
	// RedirectURL is where the checkout link sends the customer once the invoice is paid, else "".
	RedirectURL string
}

// DeclaredInvoice is an invoice declared for an order, as the merchant's registry holds it
// (FindInvoices, and the errors invoice_pending and order_paid): the fields of an Invoice but
// Rate, CheckoutURL and RedirectURL, which only the shop keeps, and DeclaredAt.
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
	// DeclaredAt is when it was declared (Unix seconds); without Bolt11, when its address was
	// leased (its mark).
	DeclaredAt int64
}

// InvoiceToCheck is what a payment check needs of an invoice: an Invoice or a DeclaredInvoice is
// one (ToCheck). Ref is needed with an address: the witnesses of the payment are its order's.
type InvoiceToCheck struct {
	Ref            string
	Bolt11         string
	PaymentHash    string
	VerifyURL      string
	OnchainAddress string
	Sats           int64
	ExpiresAt      int64
}

// Checkable is what GetStatus and WaitForPayment take: an Invoice, a DeclaredInvoice or an
// InvoiceToCheck, or a pointer to one.
type Checkable interface {
	toCheck() InvoiceToCheck
}

func (i Invoice) toCheck() InvoiceToCheck { return i.ToCheck() }

func (d DeclaredInvoice) toCheck() InvoiceToCheck { return d.ToCheck() }

func (c InvoiceToCheck) toCheck() InvoiceToCheck { return c }

// ToCheck is what a payment check needs of the invoice.
func (i Invoice) ToCheck() InvoiceToCheck {
	return InvoiceToCheck{Ref: i.Ref, Bolt11: i.Bolt11, PaymentHash: i.PaymentHash, VerifyURL: i.VerifyURL,
		OnchainAddress: i.OnchainAddress, Sats: i.Sats, ExpiresAt: i.ExpiresAt}
}

// ToCheck is what a payment check needs of the invoice.
func (d DeclaredInvoice) ToCheck() InvoiceToCheck {
	return InvoiceToCheck{Ref: d.Ref, Bolt11: d.Bolt11, PaymentHash: d.PaymentHash, VerifyURL: d.VerifyURL,
		OnchainAddress: d.OnchainAddress, Sats: d.Sats, ExpiresAt: d.ExpiresAt}
}

// AsInvoice is the declared invoice as an Invoice, to store with its order (the pending invoice of
// invoice_pending, say): the same fields, without Rate, CheckoutURL and RedirectURL, which the
// registry does not hold (Error.CheckoutURL gives a new checkout link to it, when one was asked).
func (d DeclaredInvoice) AsInvoice() *Invoice {
	var fiat *Fiat
	if d.Fiat != nil {
		f := *d.Fiat
		fiat = &f
	}
	return &Invoice{
		Ref:            d.Ref,
		Bolt11:         d.Bolt11,
		PaymentHash:    d.PaymentHash,
		VerifyURL:      d.VerifyURL,
		OnchainAddress: d.OnchainAddress,
		PaymentURI:     d.PaymentURI,
		Sats:           d.Sats,
		Fiat:           fiat,
		Note:           d.Note,
		CreatedAt:      d.CreatedAt,
		ExpiresAt:      d.ExpiresAt,
	}
}

// State is where the payment of an invoice is.
type State string

// The states of a payment.
const (
	// StateOpen is not paid (yet): it may still be, until ExpiresAt, and is reported up to 120 s
	// later. Received above 0: an on-chain underpayment, whose Rest is still payable until
	// ExpiresAt.
	StateOpen State = "open"
	// StateUnconfirmed is enough received in time (the amount, or less within the merchant's
	// tolerance or down to what the merchant accepted), on-chain by the transactions Txids (and by
	// invoices of the rest), not deep enough yet (3 confirmations): not paid yet, it may still fail
	// (a transaction replaced or dropped).
	StateUnconfirmed State = "unconfirmed"
	// StatePaid is paid: by Method.
	StatePaid State = "paid"
	// StateExpired is never paid, for good: the order may get a new invoice. Received above 0:
	// the merchant received that much, less than the amount (settle it with the customer).
	StateExpired State = "expired"
)

// Method is how a paid invoice was paid.
type Method string

// The methods of a paid invoice.
const (
	// MethodLightning is paid in Lightning: Preimage (hex) is the proof of payment, to keep with
	// the order.
	MethodLightning Method = "lightning"
	// MethodOnchain is paid on-chain by the transactions Txids (3 confirmations on the explorer
	// read).
	MethodOnchain Method = "onchain"
	// MethodMixed is paid on-chain by Txids (3 confirmations on the explorer read; none when the
	// invoices of the rest alone pay it) and in Lightning by invoices of the rest of that
	// underpayment, whose Preimages (hex) are the proofs of those payments, to keep with the order.
	MethodMixed Method = "mixed"
)

// Rest is what is left to pay of an invoice underpaid on-chain (PaymentStatus open).
type Rest struct {
	// Sats is what is left: the amount less what was received, whatever the merchant's tolerance.
	Sats int64
	// Bolt11 is a Lightning invoice of exactly that, declared for the order; "" while there is
	// none (asked again at the next check, or outside the Lightning address's bounds).
	Bolt11 string
	// PaymentURI is what to show as one QR code: bitcoin:<address>?amount=…&lightning=… from 1 000
	// sats (on-chain to the same address too), lightning:<bolt11> below; "" with neither.
	PaymentURI string
}

// PaymentStatus is where the payment of an invoice is. Only the fields of its State (and Method)
// mean something:
//
//	open:            ExpiresAt, Received, Rest
//	unconfirmed:     Received, Confirmations, Txids
//	paid, lightning: Preimage
//	paid, onchain:   Received, Txids
//	paid, mixed:     Received, Txids, Preimages
//	expired:         Received
//
// Received is what the order received in time, in sats: on-chain to its address, and by the
// Lightning invoices of the rest of an on-chain underpayment. Below the amount in unconfirmed and
// paid (on-chain or mixed) when the merchant's tolerance of on-chain underpayments, or the
// merchant's acceptance of what came (in its app), accepted it: the order is paid all the same.
type PaymentStatus struct {
	State  State
	Method Method
	// ExpiresAt is the invoice's end (Unix seconds).
	ExpiresAt int64
	Received  int64
	// Rest is nil unless Received is above 0 before the invoice's end.
	Rest          *Rest
	Confirmations int
	Txids         []string
	Preimage      string
	Preimages     []string
}

// Merchant is the merchant, as the integration's channel, Lightning address and lot give it.
// Never its Lightning address: the app never shows it to the integrator.
type Merchant struct {
	BusinessName string
	// MinSats and MaxSats are the smallest and largest invoices its Lightning address makes.
	MinSats int64
	MaxSats int64
	// OnchainAddresses is the number of deposit addresses of the integration's lot, leased or
	// not: 0 takes no on-chain payment.
	OnchainAddresses int
}

// Writing.

func digits(n int64) string { return strconv.FormatInt(n, 10) }

func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func list(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}

func (f *Fiat) members() any {
	if f == nil {
		return nil
	}
	return jsonv.O("amount", f.Amount, "currency", f.Currency, "minor", digits(f.Minor), "digits", f.Digits)
}

func (r *RateInfo) members() any {
	if r == nil {
		return nil
	}
	var t any
	if r.Time != nil {
		t = *r.Time
	}
	return jsonv.O("source", r.Source, "currency", r.Currency, "price", r.Price, "fetchedAt", r.FetchedAt, "time", t)
}

func (r *Rest) members() any {
	if r == nil {
		return nil
	}
	return jsonv.O("sats", digits(r.Sats), "bolt11", orNull(r.Bolt11), "paymentUri", orNull(r.PaymentURI))
}

func (s *PaymentStatus) members() any {
	if s == nil {
		return nil
	}
	state, method := string(s.State), string(s.Method)
	switch {
	case s.State == StateOpen:
		return jsonv.O("state", state, "expiresAt", s.ExpiresAt, "received", digits(s.Received), "rest", s.Rest.members())
	case s.State == StateUnconfirmed:
		return jsonv.O("state", state, "received", digits(s.Received), "confirmations", s.Confirmations, "txids", list(s.Txids))
	case s.State == StatePaid && s.Method == MethodLightning:
		return jsonv.O("state", state, "method", method, "preimage", s.Preimage)
	case s.State == StatePaid && s.Method == MethodMixed:
		return jsonv.O("state", state, "method", method, "received", digits(s.Received), "txids", list(s.Txids), "preimages", list(s.Preimages))
	case s.State == StatePaid:
		return jsonv.O("state", state, "method", method, "received", digits(s.Received), "txids", list(s.Txids))
	default:
		return jsonv.O("state", state, "received", digits(s.Received))
	}
}

// MarshalJSON writes the price as SPEC.md §3 does.
func (f Fiat) MarshalJSON() ([]byte, error) { return jsonv.Marshal(f.members()), nil }

// MarshalJSON writes the rate as SPEC.md §3 does.
func (r RateInfo) MarshalJSON() ([]byte, error) { return jsonv.Marshal(r.members()), nil }

// MarshalJSON writes the rest as SPEC.md §3 does.
func (r Rest) MarshalJSON() ([]byte, error) { return jsonv.Marshal(r.members()), nil }

// MarshalJSON writes the status as SPEC.md §3 does: the fields of its state.
func (s PaymentStatus) MarshalJSON() ([]byte, error) { return jsonv.Marshal(s.members()), nil }

// MarshalJSON writes the merchant as SPEC.md §3 does.
func (m Merchant) MarshalJSON() ([]byte, error) {
	return jsonv.Marshal(jsonv.O("businessName", m.BusinessName, "minSats", digits(m.MinSats), "maxSats", digits(m.MaxSats),
		"onchainAddresses", m.OnchainAddresses)), nil
}

// MarshalJSON writes the invoice as SPEC.md §3 does: what to store with the order.
func (i Invoice) MarshalJSON() ([]byte, error) {
	return jsonv.Marshal(jsonv.O(
		"ref", i.Ref,
		"bolt11", orNull(i.Bolt11),
		"paymentHash", orNull(i.PaymentHash),
		"verifyUrl", orNull(i.VerifyURL),
		"onchainAddress", orNull(i.OnchainAddress),
		"paymentUri", i.PaymentURI,
		"sats", digits(i.Sats),
		"fiat", i.Fiat.members(),
		"rate", i.Rate.members(),
		"note", i.Note,
		"createdAt", i.CreatedAt,
		"expiresAt", i.ExpiresAt,
		"checkoutUrl", orNull(i.CheckoutURL),
		"redirectUrl", orNull(i.RedirectURL),
	)), nil
}

// MarshalJSON writes the invoice as SPEC.md §3 does.
func (d DeclaredInvoice) MarshalJSON() ([]byte, error) {
	return jsonv.Marshal(jsonv.O(
		"ref", d.Ref,
		"bolt11", orNull(d.Bolt11),
		"paymentHash", orNull(d.PaymentHash),
		"verifyUrl", orNull(d.VerifyURL),
		"onchainAddress", orNull(d.OnchainAddress),
		"paymentUri", d.PaymentURI,
		"sats", digits(d.Sats),
		"fiat", d.Fiat.members(),
		"note", d.Note,
		"createdAt", d.CreatedAt,
		"expiresAt", d.ExpiresAt,
		"declaredAt", d.DeclaredAt,
	)), nil
}

// MarshalJSON writes what a payment check needs as SPEC.md §3 does.
func (c InvoiceToCheck) MarshalJSON() ([]byte, error) {
	return jsonv.Marshal(jsonv.O(
		"ref", orNull(c.Ref),
		"bolt11", orNull(c.Bolt11),
		"paymentHash", orNull(c.PaymentHash),
		"verifyUrl", orNull(c.VerifyURL),
		"onchainAddress", orNull(c.OnchainAddress),
		"sats", digits(c.Sats),
		"expiresAt", c.ExpiresAt,
	)), nil
}

// Conversions from the engine's values.

func fiatOf(f *model.Fiat) *Fiat {
	if f == nil {
		return nil
	}
	return &Fiat{Amount: f.Amount, Currency: f.Currency, Minor: f.Minor, Digits: f.Digits}
}

func rateOf(r *model.RateInfo) *RateInfo {
	if r == nil {
		return nil
	}
	info := &RateInfo{Source: r.Source, Currency: r.Currency, Price: r.Price, FetchedAt: r.FetchedAt}
	if r.Time != nil {
		t := *r.Time
		info.Time = &t
	}
	return info
}

func invoiceOf(i *model.Invoice) *Invoice {
	return &Invoice{
		Ref:            i.Ref,
		Bolt11:         i.Bolt11,
		PaymentHash:    i.PaymentHash,
		VerifyURL:      i.VerifyURL,
		OnchainAddress: i.OnchainAddress,
		PaymentURI:     i.PaymentURI,
		Sats:           i.Sats,
		Fiat:           fiatOf(i.Fiat),
		Rate:           rateOf(i.Rate),
		Note:           i.Note,
		CreatedAt:      i.CreatedAt,
		ExpiresAt:      i.ExpiresAt,
		CheckoutURL:    i.CheckoutURL,
		RedirectURL:    i.RedirectURL,
	}
}

func declaredOf(d *model.DeclaredInvoice) *DeclaredInvoice {
	if d == nil {
		return nil
	}
	return &DeclaredInvoice{
		Ref:            d.Ref,
		Bolt11:         d.Bolt11,
		PaymentHash:    d.PaymentHash,
		VerifyURL:      d.VerifyURL,
		OnchainAddress: d.OnchainAddress,
		PaymentURI:     d.PaymentURI,
		Sats:           d.Sats,
		Fiat:           fiatOf(d.Fiat),
		Note:           d.Note,
		CreatedAt:      d.CreatedAt,
		ExpiresAt:      d.ExpiresAt,
		DeclaredAt:     d.DeclaredAt,
	}
}

func statusOf(s *model.PaymentStatus) *PaymentStatus {
	if s == nil {
		return nil
	}
	status := &PaymentStatus{
		State:         State(s.State),
		Method:        Method(s.Method),
		ExpiresAt:     s.ExpiresAt,
		Received:      s.Received,
		Confirmations: s.Confirmations,
		Txids:         append([]string(nil), s.Txids...),
		Preimage:      s.Preimage,
		Preimages:     append([]string(nil), s.Preimages...),
	}
	if s.Txids != nil && status.Txids == nil {
		status.Txids = []string{}
	}
	if s.Preimages != nil && status.Preimages == nil {
		status.Preimages = []string{}
	}
	if s.Rest != nil {
		status.Rest = &Rest{Sats: s.Rest.Sats, Bolt11: s.Rest.Bolt11, PaymentURI: s.Rest.PaymentURI}
	}
	return status
}

func merchantOf(m *model.Merchant) *Merchant {
	return &Merchant{BusinessName: m.BusinessName, MinSats: m.MinSats, MaxSats: m.MaxSats, OnchainAddresses: m.OnchainAddresses}
}
