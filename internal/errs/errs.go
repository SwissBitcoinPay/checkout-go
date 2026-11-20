// Package errs holds the errors of the package (SPEC.md §5), as the engine raises them: a stable
// code, whether the same call may succeed later unchanged, and the fields each code carries. A
// message never holds a secret: neither the integration's key, nor a sale's key, nor any key
// derived from them; nor the merchant's Lightning address. The public package checkout gives
// them to the integrator as *checkout.Error.
package errs

import (
	"context"
	"errors"
	"net/url"

	"github.com/sattleme/checkout-go/internal/model"
)

// Code is the stable code of an error.
type Code string

// The codes of SPEC.md §5.
const (
	InvalidKey          Code = "invalid_key"
	InvalidOption       Code = "invalid_option"
	InvalidOrder        Code = "invalid_order"
	Revoked             Code = "revoked"
	ChannelNotFound     Code = "channel_not_found"
	Network             Code = "network"
	InvalidInvoice      Code = "invalid_invoice"
	InvoicePending      Code = "invoice_pending"
	OrderPaid           Code = "order_paid"
	UnsupportedCurrency Code = "unsupported_currency"
	Rate                Code = "rate"
	CheckoutUnavailable Code = "checkout_unavailable"
	OnchainUnavailable  Code = "onchain_unavailable"
)

// Error is an error of the package.
type Error struct {
	Code    Code
	Message string
	// Param names the option or the parameter (invalid_option, invalid_order).
	Param string
	// NewerFormat: channel_not_found of a channel of a newer format only.
	NewerFormat bool
	// Invoice: invoice_pending, order_paid.
	Invoice *model.DeclaredInvoice
	// CheckoutURL: invoice_pending, "" for none.
	CheckoutURL string
	// Status: order_paid, its paid status.
	Status *model.PaymentStatus
	// Preimage: order_paid paid by its own Lightning invoice, else "".
	Preimage string
	// Currency: unsupported_currency.
	Currency string
	// Check names the check of SPEC.md §18 an invalid_invoice failed, when it is one of them.
	Check string
	// Cause is what failed below, never holding a secret.
	Cause error
	// Original is an error of the package the integrator's own code returned (a custom rate
	// source's): given back as it is.
	Original error
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Unwrap() error { return e.Cause }

// Retryable tells whether the same call may succeed later, unchanged.
func (e *Error) Retryable() bool {
	switch e.Code {
	case Network, Rate, OnchainUnavailable:
		return true
	case ChannelNotFound:
		return !e.NewerFormat
	}
	return false
}

// plain is a failure's message without what it wraps: a context's error in a cause would make an
// error of the package read as the call's cancellation (errors.Is).
type plain string

func (p plain) Error() string { return string(p) }

// Sanitize makes what failed below fit a cause: a URL's error loses its URL (a Lightning address's
// name is in its path), and a failure that is or wraps a context's error keeps its message alone
// (a request that ran out of its own time is no cancellation of the call).
func Sanitize(err error) error {
	if err == nil {
		return nil
	}
	var u *url.Error
	if errors.As(err, &u) {
		err = u.Err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return plain(err.Error())
	}
	return err
}

// Of returns err as an Error of the package, if it is one.
func Of(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// Is tells whether err is an Error of the package with the code.
func Is(err error, code Code) bool {
	e, ok := Of(err)
	return ok && e.Code == code
}

// NewInvalidKey is not an integration's secret key, or a damaged one.
func NewInvalidKey(message string) *Error {
	if message == "" {
		message = "Not a sattle.me integration key"
	}
	return &Error{Code: InvalidKey, Message: message}
}

// NewInvalidOption is an option of the client or of a call that is invalid (a programming error,
// found before any request): param names it.
func NewInvalidOption(param, message string) *Error {
	return &Error{Code: InvalidOption, Param: param, Message: param + ": " + message}
}

// NewInvalidOrder is a parameter of the order, or a stored invoice, that is invalid: param names
// it.
func NewInvalidOrder(param, message string) *Error {
	return &Error{Code: InvalidOrder, Param: param, Message: param + ": " + message}
}

// NewNetwork is a relay, the Lightning address's server, a rate source or an explorer that did not
// answer (in time), or fewer relays than needed that did. Retry later.
func NewNetwork(message string, cause error) *Error {
	return &Error{Code: Network, Message: message, Cause: Sanitize(cause)}
}

// NewRevoked is an integration the merchant removed.
func NewRevoked() *Error {
	return &Error{Code: Revoked, Message: "The merchant removed this integration"}
}

// NewChannelNotFound is relays that answered, none holding the integration's channel (or only one
// of a newer format than this package reads).
func NewChannelNotFound(newerFormat bool) *Error {
	message := "The integration's channel was not found on its relays"
	if newerFormat {
		message = "The integration's channel is of a newer format: update github.com/sattleme/checkout-go"
	}
	return &Error{Code: ChannelNotFound, Message: message, NewerFormat: newerFormat}
}

// NewInvalidInvoice is an invoice, a pay request or a payment answer that fails the checks: never
// show such an invoice.
func NewInvalidInvoice(message string, cause error) *Error {
	return &Error{Code: InvalidInvoice, Message: message, Cause: Sanitize(cause)}
}

// NewUnsupportedCurrency is a rate source without a price in the currency; message "" says so of
// source.
func NewUnsupportedCurrency(currency, source, message string) *Error {
	if message == "" {
		message = source + " has no price in " + currency
	}
	return &Error{Code: UnsupportedCurrency, Message: message, Currency: currency}
}

// NewRate is no usable rate: a source answered something that is not a price, a price too old, or
// a custom source failed (cause). Retry later, or with another source.
func NewRate(message string, cause error) *Error {
	return &Error{Code: Rate, Message: message, Cause: Sanitize(cause)}
}

// NewInvoicePending is an order that still has an invoice that may be paid: show that one.
func NewInvoicePending(invoice *model.DeclaredInvoice, checkoutURL string) *Error {
	return &Error{
		Code:        InvoicePending,
		Message:     "This order still has an invoice that may be paid",
		Invoice:     invoice,
		CheckoutURL: checkoutURL,
	}
}

// NewCheckoutUnavailable is a checkout link asked of a merchant whose app gives no web app for it.
func NewCheckoutUnavailable() *Error {
	return &Error{
		Code:    CheckoutUnavailable,
		Message: "The merchant's app gives no web app for checkout links: show the invoice on the shop's page",
	}
}

// NewOrderPaid is an order already paid, with invoice: status is its paid status. Never charge it
// again.
func NewOrderPaid(invoice *model.DeclaredInvoice, status *model.PaymentStatus) *Error {
	e := &Error{Code: OrderPaid, Message: "This order was already paid", Invoice: invoice, Status: status}
	if status.Method == model.Lightning {
		e.Preimage = status.Preimage
	}
	return e
}

// NewOnchainUnavailable is an order that can only be paid on-chain, and no deposit address of the
// lot that could be leased. Retry later: the merchant's app fills the lot again whenever it is
// opened.
func NewOnchainUnavailable() *Error {
	return &Error{
		Code:    OnchainUnavailable,
		Message: "No deposit address of the integration's lot could be leased: try again later",
	}
}
