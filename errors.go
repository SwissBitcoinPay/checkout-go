package checkout

import (
	"context"
	"errors"

	"github.com/sattleme/checkout-go/internal/errs"
)

// Code is the stable code of an error (SPEC.md §5), for programs.
type Code string

// The codes of the errors.
const (
	// CodeInvalidKey is a key that is not an integration key (another prefix, damaged, an old
	// https://…/pay#… link).
	CodeInvalidKey Code = "invalid_key"
	// CodeInvalidOption is an option of the client or of WaitForPayment that is invalid: Param
	// names it.
	CodeInvalidOption Code = "invalid_option"
	// CodeInvalidOrder is a parameter of the order, or a stored invoice, that is invalid: Param
	// names it (ref, sats, fiat, rateSource, durationSeconds, note, checkout, redirectUrl,
	// invoice).
	CodeInvalidOrder Code = "invalid_order"
	// CodeRevoked is an integration the merchant removed.
	CodeRevoked Code = "revoked"
	// CodeChannelNotFound is relays that answered without the integration's channel (NewerFormat:
	// update the package).
	CodeChannelNotFound Code = "channel_not_found"
	// CodeNetwork is a relay, breez.tips, a rate source or an explorer that did not answer, or
	// fewer than a majority of the merchant's relays that took a record.
	CodeNetwork Code = "network"
	// CodeInvalidInvoice is an invoice or a payment answer that failed the checks. Never show such
	// an invoice.
	CodeInvalidInvoice Code = "invalid_invoice"
	// CodeInvoicePending is an earlier invoice of this order that may still be paid, or is being
	// confirmed on-chain: Invoice (and CheckoutURL with Checkout).
	CodeInvoicePending Code = "invoice_pending"
	// CodeOrderPaid is an earlier invoice of this order that was paid: Invoice, Status, Preimage
	// ("" on-chain).
	CodeOrderPaid Code = "order_paid"
	// CodeUnsupportedCurrency is a rate source without a price in the currency: Currency.
	CodeUnsupportedCurrency Code = "unsupported_currency"
	// CodeRate is no usable price: invalid, too old, or your rate function failed.
	CodeRate Code = "rate"
	// CodeCheckoutUnavailable is a checkout link asked of a merchant whose app gives no web app
	// for checkout links.
	CodeCheckoutUnavailable Code = "checkout_unavailable"
	// CodeOnchainUnavailable is an order that can only be paid on-chain, and no address of the
	// integration's lot that could be leased.
	CodeOnchainUnavailable Code = "onchain_unavailable"
)

// Error is every error of the package (SPEC.md §5) but for a cancelled call, which fails with its
// context's error (context.Canceled, context.DeadlineExceeded). Use errors.As to read it:
//
//	var e *checkout.Error
//	if errors.As(err, &e) && e.Code == checkout.CodeInvoicePending { … e.Invoice … }
//
// None ever holds a secret: neither the key, nor a sale's key, nor any key derived from them,
// nor the merchant's Lightning address.
type Error struct {
	Code    Code
	Message string
	// Param names the option or the parameter: invalid_option, invalid_order.
	Param string
	// NewerFormat is a channel of a newer format than this version reads: channel_not_found.
	NewerFormat bool
	// Invoice is the order's earlier invoice: invoice_pending, order_paid.
	Invoice *DeclaredInvoice
	// CheckoutURL is a new checkout link to the pending invoice, when the order asked for one and
	// that invoice may still be paid: invoice_pending ("" otherwise).
	CheckoutURL string
	// Status is the invoice's paid status: order_paid.
	Status *PaymentStatus
	// Preimage is the proof of payment of an invoice paid by its own Lightning invoice: order_paid
	// ("" when it was paid on-chain, or mixed).
	Preimage string
	// Currency is the currency: unsupported_currency.
	Currency string
	// Err is what failed below (a relay's refusal, your rate function's error), when there is
	// something: errors.Unwrap gives it. Never a secret.
	Err error
}

func (e *Error) Error() string { return e.Message }

// Unwrap gives what failed below.
func (e *Error) Unwrap() error { return e.Err }

// Retryable tells whether the same call may succeed later, unchanged.
func (e *Error) Retryable() bool {
	switch e.Code {
	case CodeNetwork, CodeRate, CodeOnchainUnavailable:
		return true
	case CodeChannelNotFound:
		return !e.NewerFormat
	}
	return false
}

// NewUnsupportedCurrencyError is the error a custom rate source returns when it has no price in
// currency: the next source of the list is asked. source names it in the message.
func NewUnsupportedCurrencyError(currency, source string) *Error {
	return &Error{Code: CodeUnsupportedCurrency, Message: source + " has no price in " + currency, Currency: currency}
}

// NewNetworkError is the error a custom rate source returns when its service does not answer:
// the next source of the list is asked. cause, if any, is what failed (never a secret).
func NewNetworkError(message string, cause error) *Error {
	return &Error{Code: CodeNetwork, Message: message, Err: cause}
}

// NewRateError is the error a custom rate source returns when it cannot give a usable price now:
// the next source of the list is asked. cause, if any, is what failed (never a secret).
func NewRateError(message string, cause error) *Error {
	return &Error{Code: CodeRate, Message: message, Err: cause}
}

// publicError is the error the integrator gets of err (SPEC.md §5: every error but a cancellation
// is one of the package's): the package's Error, or the Error of this package a custom rate source
// returned, as it is. Anything else, which the package does not raise, is network, what failed as
// its Err: a step that failed in a way the package does not classify, or a request's own end (a
// context's error here is no cancellation of the call: done gives a cancelled call's error first).
func publicError(err error) error {
	if err == nil {
		return nil
	}
	e, ok := errs.Of(err)
	if !ok {
		cause := errs.Sanitize(err)
		message := "The call failed: " + cause.Error()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			message = "No answer in time"
		}
		return &Error{Code: CodeNetwork, Message: message, Err: cause}
	}
	if e.Original != nil {
		return e.Original
	}
	return &Error{
		Code:        Code(e.Code),
		Message:     e.Message,
		Param:       e.Param,
		NewerFormat: e.NewerFormat,
		Invoice:     declaredOf(e.Invoice),
		CheckoutURL: e.CheckoutURL,
		Status:      statusOf(e.Status),
		Preimage:    e.Preimage,
		Currency:    e.Currency,
		Err:         e.Cause,
	}
}

// internalError is the engine's error of an error a custom rate source returned: an Error of
// this package keeps its code (and is given back as it is), anything else is left to the engine.
func internalError(err error) error {
	var e *Error
	if !errors.As(err, &e) {
		return err
	}
	return &errs.Error{Code: errs.Code(e.Code), Message: e.Message, Param: e.Param, Currency: e.Currency, NewerFormat: e.NewerFormat, Original: err}
}
