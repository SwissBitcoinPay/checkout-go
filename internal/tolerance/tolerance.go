// Package tolerance is the merchant's tolerance of on-chain underpayments (SPEC.md §31, "The
// merchant's tolerance"): the share of an order's amount its payment in time may miss and still
// pay it, in basis points. The channel gives it, an order's lease keeps the one of its creation:
// every check of the order judges its payment with that one, as the merchant's app does. It is
// never written where the shop's page or the customer reads (the Invoice, the PaymentStatus, a
// checkout link's record, an error, a log): a customer who knew it could pay that much less on
// purpose.
package tolerance

import (
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/proto"
)

// Of reads a tolerance as a channel or a lease writes it: an integer from 0 to 1000, else 0.
func Of(v any) int64 {
	f, ok := v.(float64)
	if !ok || !jsonv.IsInteger(v) || f < 0 || f > proto.MaxTolerance {
		return 0
	}
	return int64(f)
}

// LeastAccepted is the least accepted of sats with tolerance (basis points): sats less the share
// the tolerance allows, rounded down to the whole sat, so that the merchant never accepts more
// missing than its tolerance. On integers (sats × 1000 fits 64 bits: 2.1 × 10^18).
func LeastAccepted(sats, tolerance int64) int64 {
	if tolerance < 0 || tolerance > proto.MaxTolerance {
		tolerance = 0
	}
	return sats - sats*tolerance/proto.ToleranceScale
}

// DueOnchain is what is due on-chain of an invoice of sats with tolerance, its rest invoices
// having paid paidByRests, with the received of the merchant's acceptance that counts,
// merchantAccepted (0 without one: onchain.MerchantAcceptanceOf) (SPEC.md §31, "The on-chain
// payment", step 4): nothing (0) when they paid sats or more; else max(1, min(L, M) − R), L the
// least accepted, M the merchant's acceptance's received (sats without one), R what they paid. R
// is taken once from the lesser: M counts the rest invoices paid when the merchant accepted, as L
// counts every payment. At least 1 sat: something must count on-chain for the tolerance or the
// merchant's acceptance to apply.
func DueOnchain(sats, tolerance, paidByRests, merchantAccepted int64) int64 {
	if paidByRests >= sats {
		return 0
	}
	accepted := LeastAccepted(sats, tolerance)
	if merchantAccepted > 0 && merchantAccepted < accepted {
		accepted = merchantAccepted
	}
	if due := accepted - paidByRests; due > 0 {
		return due
	}
	return 1
}
