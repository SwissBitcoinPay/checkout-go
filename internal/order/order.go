// Package order checks what a caller gives, before any request (SPEC.md §17, §22): an order, a
// stored invoice. Refused, never repaired.
package order

import (
	"strconv"

	"github.com/sattleme/checkout-go/internal/address"
	"github.com/sattleme/checkout-go/internal/bolt11"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/lnurl"
	"github.com/sattleme/checkout-go/internal/model"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/rates"
	"github.com/sattleme/checkout-go/internal/text"
	"github.com/sattleme/checkout-go/internal/urls"
)

// Order is an order as the caller gives it, each optional parameter with whether it was given.
type Order struct {
	Ref string
	// Sats is the amount in sats (HasSats), or Fiat the price in a currency (not nil).
	Sats    int64
	HasSats bool
	Fiat    *Price
	// RateSources is the order's own rateSource, as rates.SourcesOf reads it (HasRateSources).
	RateSources    any
	HasRateSources bool
	// DurationSeconds is the invoice's duration (HasDuration), 900 when absent.
	DurationSeconds int64
	HasDuration     bool
	// Note is what the merchant's app shows (HasNote).
	Note    string
	HasNote bool
	// Checkout asks for a checkout link.
	Checkout bool
	// RedirectURL is where the checkout link sends the customer once it is paid (HasRedirectURL).
	RedirectURL    string
	HasRedirectURL bool
}

// Price is a price in a currency, as the order gives it: { amount: "24.50", currency: "CHF" }.
type Price struct {
	Amount   string
	Currency string
}

// Checked is an order, checked.
type Checked struct {
	Ref string
	// Sats is the amount in sats, 0 for an order in a currency (Fiat then set).
	Sats int64
	Fiat *model.Fiat
	// RateSources are the sources of an order in a currency, in the order they are asked; nil in
	// sats.
	RateSources     []rates.Source
	DurationSeconds int
	// Label is the note, else the reference: what the declaration and the invoice's comment carry.
	Label    string
	Checkout bool
	// RedirectURL is "" without one.
	RedirectURL string
}

// RateSourceRule says what a rateSource is.
const RateSourceRule = `"coingecko", "mempool", "opennode", "coinbase", "kraken" or a custom source, or a non-empty list of them without the same name twice`

// CheckRef checks a reference: invalid_order (ref) unless it is 1 to 64 characters
// A–Z a–z 0–9 . _ : -.
func CheckRef(ref string) error {
	if !proto.IsRef(ref) {
		return errs.NewInvalidOrder("ref", proto.RefRule)
	}
	return nil
}

// Check checks the order's parameters, in the order of SPEC.md §17. clientSources are the
// client's: an order in a currency without its own rateSource uses them.
func Check(o Order, clientSources []rates.Source) (*Checked, error) {
	if err := CheckRef(o.Ref); err != nil {
		return nil, err
	}
	if o.HasSats == (o.Fiat != nil) {
		return nil, errs.NewInvalidOrder("sats", "give either sats or fiat")
	}
	checked := &Checked{Ref: o.Ref}
	if o.HasSats {
		if o.Sats < 1 || o.Sats > proto.MaxSats {
			return nil, errs.NewInvalidOrder("sats", "a whole number of sats, from 1 to "+strconv.FormatInt(proto.MaxSats, 10))
		}
		checked.Sats = o.Sats
	}
	if o.Fiat != nil {
		fiat, err := rates.ParseFiat(o.Fiat.Amount, o.Fiat.Currency)
		if err != nil {
			return nil, err
		}
		checked.Fiat = &fiat
	}
	if o.HasRateSources {
		if o.Fiat == nil {
			return nil, errs.NewInvalidOrder("rateSource", "only for an order priced in a currency (fiat)")
		}
		sources, ok := rates.SourcesOf(o.RateSources)
		if !ok {
			return nil, errs.NewInvalidOrder("rateSource", RateSourceRule)
		}
		checked.RateSources = sources
	} else if o.Fiat != nil {
		checked.RateSources = clientSources
	}
	checked.DurationSeconds = proto.DefaultDurationSeconds
	if o.HasDuration {
		if o.DurationSeconds < proto.MinDurationSeconds || o.DurationSeconds > proto.MaxDurationSeconds {
			return nil, errs.NewInvalidOrder("durationSeconds", strconv.Itoa(proto.MinDurationSeconds)+" to "+strconv.Itoa(proto.MaxDurationSeconds)+" seconds")
		}
		checked.DurationSeconds = int(o.DurationSeconds)
	}
	label, err := Label(o.Note, o.HasNote, o.Ref)
	if err != nil {
		return nil, err
	}
	checked.Label = label
	checked.Checkout = o.Checkout
	if o.HasRedirectURL {
		if !o.Checkout {
			return nil, errs.NewInvalidOrder("redirectUrl", "only with checkout: true")
		}
		if _, ok := urls.RedirectShown(o.RedirectURL); !ok {
			return nil, errs.NewInvalidOrder("redirectUrl", "an https URL (lowercase host, no user name nor password) or an app's link "+
				"(scheme://…), 2048 characters at most, without spaces nor other characters to escape")
		}
		checked.RedirectURL = o.RedirectURL
	}
	return checked, nil
}

// Label is the note, without the spaces (U+0020) around it, else the reference: 100 code points
// at most, well-formed, without control characters.
func Label(note string, hasNote bool, ref string) (string, error) {
	if !hasNote {
		return ref, nil
	}
	if text.HasControl(note) {
		return "", errs.NewInvalidOrder("note", "no control characters")
	}
	if !text.IsWellFormed(note) {
		return "", errs.NewInvalidOrder("note", "well-formed Unicode")
	}
	label := text.TrimSpaces(note)
	if label == "" {
		label = ref
	}
	if text.CodePoints(label) > proto.MaxNoteLength {
		return "", errs.NewInvalidOrder("note", strconv.Itoa(proto.MaxNoteLength)+" characters at most")
	}
	return label, nil
}

// Stored is an invoice as the caller stored it ("" for null), before it is checked.
type Stored struct {
	Ref            string
	Bolt11         string
	PaymentHash    string
	VerifyURL      string
	OnchainAddress string
	Sats           int64
	ExpiresAt      int64
}

const notStored = "not an invoice made by createInvoice"

// CheckStored checks a stored invoice (SPEC.md §22): its sats and end; a mainnet invoice of
// exactly its sats, its own payment hash and end, an https verify URL, or none of the three; a
// deposit address, or none; at least one of the two. With an address, its reference: the
// witnesses of its payment are its order's. invalid_order (invoice) otherwise.
func CheckStored(s Stored) (*model.InvoiceToCheck, *lnurl.Invoice, error) {
	invalid := errs.NewInvalidOrder("invoice", notStored)
	if s.Sats < 1 || s.Sats > proto.MaxSats || s.ExpiresAt > proto.MaxMinorUnits || s.ExpiresAt < -proto.MaxMinorUnits {
		return nil, nil, invalid
	}
	var lightning *lnurl.Invoice
	if s.Bolt11 == "" {
		if s.PaymentHash != "" || s.VerifyURL != "" {
			return nil, nil, invalid
		}
	} else {
		decoded, ok := bolt11.Decode(s.Bolt11)
		if !ok || decoded.Network != "bc" || !decoded.HasAmount || decoded.AmountMsat != s.Sats*1000 ||
			!text.IsHex32(s.PaymentHash) || decoded.PaymentHash != s.PaymentHash ||
			decoded.Timestamp+decoded.ExpirySeconds != s.ExpiresAt {
			return nil, nil, invalid
		}
		if _, ok := urls.ParseHTTPS(s.VerifyURL); !ok {
			return nil, nil, invalid
		}
		lightning = &lnurl.Invoice{Bolt11: s.Bolt11, PaymentHash: s.PaymentHash, VerifyURL: s.VerifyURL, ExpiresAt: s.ExpiresAt}
	}
	if s.OnchainAddress != "" && !address.IsDeposit(s.OnchainAddress) {
		return nil, nil, invalid
	}
	if lightning == nil && s.OnchainAddress == "" {
		return nil, nil, invalid
	}
	ref := ""
	if s.OnchainAddress != "" {
		if !proto.IsRef(s.Ref) {
			return nil, nil, errs.NewInvalidOrder("invoice", notStored+": with an address, its ref ("+proto.RefRule+")")
		}
		ref = s.Ref
	}
	return &model.InvoiceToCheck{
		Ref:            ref,
		Bolt11:         s.Bolt11,
		PaymentHash:    s.PaymentHash,
		VerifyURL:      s.VerifyURL,
		OnchainAddress: s.OnchainAddress,
		Sats:           s.Sats,
		ExpiresAt:      s.ExpiresAt,
	}, lightning, nil
}
