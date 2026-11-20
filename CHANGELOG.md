# Changelog

All notable changes to `github.com/sattleme/checkout-go`. Versions follow
[Semantic Versioning](https://semver.org/); tags are `vX.Y.Z`.

A version is its tag on a commit of `main`: Go's module proxy serves that commit to whoever asks
for the version, and nothing is uploaded anywhere.

## 1.0.0 - 2026-11-20

First version. Follows version 1.0.0 of the specification
([sattleme/checkout-spec](https://github.com/sattleme/checkout-spec)) and passes its
conformance vectors. Go 1.26.10 or later.

- **The client**, `checkout.New(key, options...)`: reads the integration key and the merchant
  (`Merchant`), makes the invoice of an order and declares it in the merchant's registry
  (`CreateInvoice`), checks its payment (`GetStatus`, `WaitForPayment`), finds the invoices of an
  order again (`FindInvoices`). Every method takes a `context.Context` first, whose cancellation
  stops the call. Made for a server that lives for one call: nothing is kept between calls, and a
  `Client` is safe for concurrent use. Nothing a call started runs on once it returned but a write
  to the relays past the majority, for the relay timeout at most; what the client is given is
  called from several goroutines, and must be safe for concurrent use.
- **One order, one invoice at a time**, checked against the registry (`invoice_pending`,
  `order_paid`): the payment of an invoice that may still be paid is checked first, even when it is
  the only one, so that an invoice already paid is never handed out again; of several, the one a
  majority of the relays holds is handed out.
- **Orders in sats or in a currency** (the ISO 4217 currencies), converted exactly and rounded up
  to the sat, at the price of a list of rate sources asked in turn: CoinGecko, mempool.space,
  Coinbase, Kraken, OpenNode, or your own (`CustomRateSource`). `CurrencyDigits` gives a
  currency's decimals, and none for any other code.
- **Lightning payments**: an invoice from the merchant's Lightning address (LUD-16, LUD-21 verify
  URL).
- **On-chain payments**: from 1 000 sats, a taproot deposit address of the merchant's wallet leased
  for the order, with a single `bitcoin:` QR code (`PaymentURI`). A lease is published
  unconfirmed, read again on every relay of the merchant, then marked confirmed: only a confirmed
  lease gives the order its address, and no order leases an address another order holds or a
  witness names. Payment checked on the explorer, mempool.space or the merchant's Bitcoin node
  (3 confirmations), with fee bumps (RBF) and the `unconfirmed` state; `WaitForPayment` watches the
  address on the explorer's WebSocket.
- **The merchant's Bitcoin node**: when the merchant's app names one in the integration's channel
  (`bitcoinNode`), every read of an address and of the chain's height, and `WaitForPayment`'s
  WebSocket, go to the node (an Umbrel, a server of the merchant's), in clear `http` too for a node
  of a local network. Its `fallback`: `"mempool"` reads mempool.space when the node does not
  answer (the node asked after it for 60 s once it failed), `"none"` never does (no address leased,
  `network` and never `expired` while the node is down). With `payers`, the checkout links' records
  name it, and their page reads it too.
- **Witnesses** of the transactions each check sees, which name the invoice they are of: a
  transaction a witness of another invoice names counts for nothing.
- **The rest of an on-chain underpayment**, payable in Lightning (and on-chain from 1 000 sats),
  shown on the order's checkout links; an order paid both ways is `paid` with `Method` `mixed`.
- **The merchant's tolerance of on-chain underpayments** (0 to 10 %), read in the integration's
  channel and kept in the lease of each order's address.
- **The merchant's acceptance** of an order underpaid on-chain at what it received, made by the
  merchant in its app: read by every check on every relay of the merchant, and counted only when a
  majority of them hold it; the order is then paid with `Received` below its amount.
- **Checkout links**: `CreateInvoice` with `Checkout` (and `RedirectURL`) gives a `CheckoutURL`, a
  page of the merchant's web app where the customer pays, then is sent to `RedirectURL`.
- **Stored invoices**: an `Invoice` is stored as the JSON every implementation stores
  (`json.Marshal`), read back by `json.Unmarshal` into an `Invoice`, a `DeclaredInvoice` or an
  `InvoiceToCheck`, and checked before any payment check asks anything. The pending invoice of
  `invoice_pending` is kept as the order's `Invoice` with `DeclaredInvoice.AsInvoice`.
- **Outbound requests**: https URLs only (a lowercase host name, no IP address, no host under
  `localhost`), but for the merchant's Bitcoin node (`http` for a host of a local network, an IPv4
  address, never `localhost`); no redirect followed, at most 1 MiB read of an answer, each request
  bounded by its timeout with its DNS lookup, and `User-Agent: sattleme-Checkout/1`. The WebSocket
  handshake (of the relays and of the explorer) follows no redirect, carries the same User-Agent
  and offers no compression; a read keeps only what opens with its keys, once per event, so a relay
  cannot make the server's memory grow.
- **Typed errors**: every error is a `*checkout.Error` with a stable `Code`, `Retryable()` and what
  failed below as its `Unwrap`; a cancelled call fails with its context's error.
