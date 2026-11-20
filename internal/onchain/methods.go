// Package onchain holds on-chain payments (SPEC.md §31): the methods of an order by its amount,
// the lot, the leases of its addresses, the explorers, replacements, witnesses, the on-chain part
// of a payment check, the rest of an underpayment, acceptance records, and the explorer's
// WebSocket while a payment is waited for.
package onchain

import (
	"strconv"

	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/proto"
)

// Methods are the methods an order may be paid with.
type Methods struct {
	Lightning bool
	Onchain   bool
}

// PaymentMethods is the methods sats may be paid with, before an address is leased (SPEC.md §31,
// "Methods by amount"): Lightning within the sendable sats, on-chain from 1 000 sats. invalid_order
// (param: "sats" or "fiat") below 1 000 sats and outside the sendable sats.
func PaymentMethods(sats, min, max int64, param string) (Methods, error) {
	lightning := sats >= min && sats <= max
	onchain := sats >= proto.OnchainFloorSats
	if !lightning && !onchain {
		return Methods{}, errs.NewInvalidOrder(param, strconv.FormatInt(sats, 10)+" sats is outside the merchant's bounds ("+
			strconv.FormatInt(min, 10)+" to "+strconv.FormatInt(max, 10)+" sats)")
	}
	return Methods{Lightning: lightning, Onchain: onchain}, nil
}

// The outcomes of leasing an address.
const (
	Leased         = "leased"
	NoAddress      = "none"
	ExplorerFailed = "explorer_failed"
)

// LeaseOutcome is what leasing an address gave: one, none, or the explorer's failure (Err).
type LeaseOutcome struct {
	Status string
	Err    error
}

// MethodsWith is the methods once leasing an address gave outcome: without an address,
// Lightning only; an order that can only be paid on-chain then fails with onchain_unavailable, or
// the explorer's network error.
func MethodsWith(m Methods, outcome LeaseOutcome) (Methods, error) {
	if !m.Onchain || outcome.Status == Leased {
		return m, nil
	}
	if !m.Lightning {
		if outcome.Status == ExplorerFailed {
			return Methods{}, outcome.Err
		}
		return Methods{}, errs.NewOnchainUnavailable()
	}
	return Methods{Lightning: true}, nil
}
