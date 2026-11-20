// Package checkout takes Lightning and on-chain payments for a sattle.me merchant from an online
// shop's server: the invoice of each order (a Lightning invoice, a deposit address of the
// merchant's wallet, or both), its declaration in the merchant's registry, and the check of its
// payment.
//
// Everything runs on the shop's server, which holds the integration's secret key: the customer's
// browser only ever gets the invoice and its address, on the shop's page or on the merchant's web
// app through a checkout link. Payments go straight to the merchant's wallet: sattle.me runs no
// server and no API for integrations. The package talks to the merchant's Lightning address
// (breez.tips), to the merchant's Nostr relays and, for on-chain payments, to a block explorer
// (mempool.space, or the merchant's own Bitcoin node when its app names one, with the fallback it
// sets) and BlockCypher; nothing else but the rate source chosen for prices in a currency. Amounts
// are exact: integers of sats or of a currency's minor units, which the data model's JSON writes as
// strings of digits.
//
// The protocol is specified for every implementation in https://github.com/sattleme/checkout-spec
// (SPEC.md), with conformance vectors; this version implements its version 1.0.0 (spec/PIN.json).
//
//	client, err := checkout.New(os.Getenv("SATTLEME_INTEGRATION_KEY"))
//	if err != nil { … }
//	invoice, err := client.CreateInvoice(ctx, checkout.Order{Ref: "ORD-1042", Sats: 49_000})
//	var e *checkout.Error
//	if errors.As(err, &e) && e.Code == checkout.CodeInvoicePending {
//		// An invoice of this order may still be paid: keep that one.
//	}
//	…
//	status, err := client.GetStatus(ctx, invoice)
//
// The public API is this package: everything under internal/ may change in any version.
package checkout
