# checkout-go

Take Lightning and on-chain payments for a sattle.me merchant from an online shop's
**server**, in Go: the invoice of each order (a Lightning invoice, a deposit address of the
merchant's wallet, or both), its declaration in the merchant's registry, and the check of its
payment.

- Everything runs on your server, which holds the integration's secret key. The customer's
  browser only ever gets the invoice and its address: on your page, or on the merchant's web app
  through a [checkout link](#checkout-links), if you ask for one.
- Payments go straight to the merchant's wallet. sattle.me runs no server and no API for
  integrations: this package talks to the merchant's Lightning address (breez.tips), to the
  merchant's Nostr relays and, for [on-chain payments](#on-chain-payments), to a block explorer
  (mempool.space, or [the merchant's own Bitcoin node](#the-merchants-bitcoin-node) when its app
  names one) and BlockCypher; nothing else (and to the rate source you choose, for prices in a
  currency).
- Amounts are exact: integers of sats or of a currency's minor units, written as strings of digits
  in the JSON you store, never floating-point.

The protocol is specified, for every implementation, in
[sattleme/checkout-spec](https://github.com/sattleme/checkout-spec) (`SPEC.md`), with
conformance vectors every implementation passes. This version implements its version 1.0.0
(`spec/PIN.json`). Homepage: [sattle.me](https://sattle.me).

## Requirements

- Go 1.26.10 or later (the two release lines Go supports, 1.26 from the patch release
  `btcec/v2` v2.5.1 requires), on a **server**.
- A clock synchronized by NTP: invoices more than 5 minutes off the server's clock are refused.
- An integration's secret key: the merchant creates the integration in the sattle.me app
  ("Integrations") and gives you its key, `sattleme_ck_…`.

## Installing

```sh
go get github.com/sattleme/checkout-go@v1.0.0
```

```go
import checkout "github.com/sattleme/checkout-go"
```

The module is `github.com/sattleme/checkout-go`, its package `checkout`. Its public API is that
package alone: everything under `internal/` may change in any version.

## The key is a secret

Keep the integration's key on the server only: in an environment variable or a secrets vault,
never in a web page, an app, a code repository or a log. With it, anyone can make invoices that
pay the merchant and mark sales as the integration's. It can never spend the merchant's funds nor
read their wallet. The `Client` never writes it: not in its string, its debug writing (`%v`,
`%#v`), its JSON, an error or a warning.

If it leaks, the merchant removes the integration in the app ("Remove the integration") and
creates another one: the old key then makes no invoice, finds none and reads no merchant
(`CreateInvoice`, `FindInvoices` and `Merchant` fail with `revoked`). A check of an invoice it made
still finds its Lightning payment: an invoice without an address is checked as before, and one with
an address is `revoked` unless its Lightning invoice was paid (`GetStatus`, `WaitForPayment`;
SPEC.md §14). The merchant settles by hand the orders still being paid on-chain.

**One key, one shop.** The merchant creates one integration for each shop (each system with its
own orders' references), and two shops never share a key: the sale of an order is derived from the
key and the order's reference, so two shops with one key would share their references, and an
order of one would be found paid, or pending, with an invoice of the other. The servers of one
shop share its key.

## Quick start

```go
// The client, made once when the server starts. Nothing to connect nor to close: each call reads
// the merchant, opens one connection per relay and closes it when it is done. Safe for concurrent
// use.
var client *checkout.Client

func start() (err error) {
	client, err = checkout.New(os.Getenv("SATTLEME_INTEGRATION_KEY"))
	return err // invalid_key, invalid_option
}

// When the customer checks out, or when the payment page asks again:
func invoiceOf(ctx context.Context, order *Order) (*checkout.Invoice, error) {
	if order.Invoice != nil {
		return order.Invoice, nil // one order, one invoice at a time
	}
	invoice, err := client.CreateInvoice(ctx, checkout.Order{
		Ref:             order.Ref,                          // "ORD-1042": its sale in the merchant's app
		Sats:            49_000,                             // or Fiat: &checkout.Price{Amount: "24.50", Currency: "CHF"}
		DurationSeconds: 900,                                // 60 to 3600
		Note:            fmt.Sprintf("Order %d", order.Number), // shown to the merchant, 100 characters at most
	})
	var e *checkout.Error
	switch {
	case errors.As(err, &e) && e.Code == checkout.CodeInvoicePending:
		// An invoice of this order may still be paid (say, after a crash): keep that one. It is
		// the registry's copy: the same fields, without Rate.
		invoice = e.Invoice.AsInvoice()
	case err != nil:
		return nil, err
	}
	order.Invoice = invoice
	return invoice, db.Save(ctx, order)
}

// The payment page gets the invoice alone: its PaymentURI ("bitcoin:bc1p…?amount=…&lightning=
// lnbc…") and ExpiresAt. When it asks for the order's status (every 1 to 2 seconds):
func statusOf(ctx context.Context, order *Order) (checkout.State, error) {
	status, err := client.GetStatus(ctx, order.Invoice)
	if err != nil {
		return "", err
	}
	switch status.State {
	case checkout.StatePaid:
		// In Lightning: keep the preimage, the proof of payment. On-chain: the transactions.
		// Mixed (on-chain, its rest in Lightning): both.
		return status.State, db.MarkPaid(ctx, order, status)
	case checkout.StateExpired:
		// Never paid: the order may get a new invoice. Received above 0: an on-chain
		// underpayment the merchant received, to settle with the customer.
		order.Invoice = nil
		return status.State, db.Save(ctx, order)
	}
	return status.State, nil // open, unconfirmed
}
```

`CreateInvoice` gives an `Invoice`; store it with the order as its JSON (`json.Marshal`), the
JSON every implementation stores (SPEC.md §3), which `json.Unmarshal` reads back, from this
package or another implementation:

```json
{
  "ref": "ORD-1042",
  "bolt11": "lnbc490u1p…",
  "paymentHash": "9c41…",
  "verifyUrl": "https://breez.tips/lnurlp/verify/9c41…",
  "onchainAddress": "bc1p…",
  "paymentUri": "bitcoin:bc1p…?amount=0.00049&lightning=lnbc490u1p…",
  "sats": "49000",
  "fiat": null,
  "rate": null,
  "note": "Order 1042",
  "createdAt": 1791378000,
  "expiresAt": 1791378900,
  "checkoutUrl": null,
  "redirectUrl": null
}
```

Every amount is a string of digits, every date an integer of Unix seconds, an absent value `null`
(`""` or `nil` in the Go types). `bolt11` is null for an order paid on-chain only, `onchainAddress`
for one paid in Lightning only; `fiat` is `{"amount": "24.50", "currency": "CHF", "minor": "2450",
"digits": 2}` for a price in a currency, `rate` the rate it was converted at.

**Stored invoices are your shop's.** Check only the invoices your shop stored itself with their
orders, where only it writes (its database), never one that comes from the customer (a form field,
a URL, a cookie): `GetStatus` checks that a stored invoice is well formed (SPEC.md §22), never that
this integration made it. An invoice someone else wrote, with a Lightning invoice of their own that
they paid, reads as `paid` (SPEC.md §27).

Instead of polling from your page's requests, a background job may wait for the payment:

```go
final, err := client.WaitForPayment(ctx, order.Invoice, // ctx: stop waiting
	checkout.WithInterval(2*time.Second),
	checkout.WithOnStatus(func(s *checkout.PaymentStatus) { log.Print(s.State) }))
// final.State: paid (Method lightning with Preimage; onchain with Received and Txids; mixed with
// Received, Txids and Preimages) or expired (with Received).
```

A network failure only delays the next check. One that each check meets again (an explorer or the
Lightning address that does not answer…) is reported to your logger once, and again only after
it stopped.

### One order, one invoice at a time

- An order keeps its invoice until it has **expired**: `GetStatus` says `expired` 120 seconds after
  the invoice's end (a payment of its last seconds may be reported that late). Only then may the
  order get a new invoice. `CreateInvoice` checks it in the merchant's registry and fails with
  `invoice_pending` (with that invoice in `Error.Invoice`) while an earlier invoice of the same
  `Ref` may still be paid (or its on-chain payment is being confirmed), or `order_paid` (with its
  `Status`, and the `Preimage` of a Lightning payment) when one was paid.
- Only the order that holds an invoice is marked paid with it, in the database transaction that
  marks it paid. Keep the preimage (the proof of a Lightning payment) or the txids (of an
  on-chain one) and the rate the amount was converted at, to answer a customer or an audit.
- Invoices of the same `Ref` are one sale in the merchant's app. One order, one `Ref`: never
  reused for another order by anything that holds the key (a test copy of the shop, a restored
  database included), and never taken from what the customer sends (make it on your server).
- After a failure, `FindInvoices(ctx, ref)` lists the invoices declared for an order, from the
  merchant's registry.
- "One at a time" holds within one `Client` and against the registry: when several processes may
  make the invoice of the same order at once, lock the order (in your database) around
  `CreateInvoice`. In a store whose reads may lag behind its writes (an eventually consistent one,
  as some hosted platforms give), read the lock and what it protects with consistent reads (SPEC.md
  §17).

### Showing the invoice

This package is for servers only. The shop's page shows the invoice with its own code:

- a QR code of `PaymentURI`, with any QR library: `bitcoin:<address>?amount=…&lightning=<bolt11>`
  with an address (BIP-21, the "unified QR" wallets read: they pay in Lightning or on-chain),
  `lightning:<bolt11>` without one;
- an "Open in wallet" link to the same URI;
- the time left until `ExpiresAt`, then the status your server gives (`open`, `unconfirmed`,
  `paid`, `expired`). With an on-chain underpayment, `open` carries what was received and its
  rest: show its `PaymentURI` as the QR code (the rest in Lightning, and on-chain to the same
  address from 1 000 sats), until `ExpiresAt`.

The page talks to your server only: never give it the key, nor ask it to reach breez.tips or the
relays. Or send the customer to a checkout link instead (below).

## Checkout links

Instead of showing the invoice on your page, you may send the customer to a page of the merchant's
sattle.me web app made for this invoice: its QR code, a button that opens the customer's wallet,
the time left, then "Paid" or "Expired". Once the invoice is paid, the page sends the customer
back to your redirect URL, 7 seconds later or at once with a tap.

```go
invoice, err := client.CreateInvoice(ctx, checkout.Order{
	Ref:         "ORD-1042",
	Fiat:        &checkout.Price{Amount: "24.50", Currency: "CHF"},
	Note:        "Order 1042", // shown to the customer too
	Checkout:    true,
	RedirectURL: "https://shop.example/orders/1042/thanks", // or "myshop://orders/1042"
})
// Store it with the order, then redirect the customer's browser to invoice.CheckoutURL:
// "https://…/pay#c.…" (nothing after "#" is ever sent to the web app's host).
```

- **Optional, per order.** Without `Checkout`, nothing changes. `RedirectURL` alone is refused
  (`invalid_order`, `Param` `redirectUrl`); without one, the page shows the receipt and sends the
  customer nowhere.
- **Not a proof of payment.** Anyone can open your redirect URL. When the customer arrives there,
  check the order with `GetStatus` (or `WaitForPayment`) on your server, and mark it paid only
  then, as without checkout.
- **What the page gets**: a record the package publishes on a majority of the merchant's relays,
  before the invoice is declared: the invoice, its verify URL, its deposit address (with its amount
  and dates), the business's name, the price, the note, your redirect URL, and the merchant's
  Bitcoin node when its app lets the payers read it
  ([The merchant's Bitcoin node](#the-merchants-bitcoin-node), `payers`). Never the key, the
  merchant's Lightning address nor its registry. It is signed by a key of that link alone,
  forgotten once it is published: nobody, you included, can change a link afterwards. Relays that
  honor NIP-40 drop it a week after the invoice's end.
- **The rest of an underpayment**: with an address, the record also carries the key and the public
  key with which the page reads the invoices of the rest your server asks for (On-chain payments,
  below): the page shows the rest's invoice as soon as a check of your server published it, so
  keep checking the payment while the customer pays (`WaitForPayment`, or `GetStatus` from your
  page's polling). Only the integration key signs them.
- **Who sees the customer**: the customer's browser loads the web app, reads the record from the
  merchant's relays and checks the payment with the merchant's Lightning address (breez.tips):
  they see its IP address, which they never do when your page shows the invoice.
- **Redirect URLs**: `https://` with a lowercase host name (no IP address, no `localhost` nor a
  name under it, no `user:password@`), or an app's link (`myshop://…`), 2048 characters at most,
  every character of RFC 3986 (anything else percent-encoded, host names in punycode). Refused:
  `http:`, browser and file schemes, other browsers' schemes (`x-safari-https:`,
  `googlechrome:`, `firefox:`…, which would open a web page whose host the page cannot show: a web
  page is an `https` URL), `intent:`, payment schemes (`lightning:`, `bitcoin:`…), calls and
  messages. The page shows "Redirecting to shop.example" (or "myshop://") on its button. Prefer
  universal links (iOS) and App Links (Android), which are `https` URLs: browsers do not open an
  app on their own after a delay (Chrome on Android refuses it), so for an app's link the customer
  taps the button.
- **One order, one invoice at a time** holds with checkout links too. When an invoice of the order
  may still be paid, `invoice_pending` carries, with `Checkout`, a new link to it:
  `Error.CheckoutURL`. Send the customer there.
- **Availability**: the merchant's app says in the integration's channel where its web app is. An
  app without a web app gives none: `CreateInvoice` with `Checkout` fails with
  `checkout_unavailable`; show the invoice on your page instead.

## On-chain payments

From 1 000 sats, an order may also be paid on-chain, to a deposit address of the merchant's
wallet. Nothing to ask for: `CreateInvoice` leases an address when the amount allows it, and the
invoice's `PaymentURI` shows both methods in one QR code.

| Amount                                          | Methods                                                     |
| ----------------------------------------------- | ----------------------------------------------------------- |
| below 1 000 sats                                | Lightning (outside its bounds: `invalid_order`)             |
| 1 000 sats or more, within the Lightning bounds | Lightning and on-chain (Lightning only without an address)  |
| 1 000 sats or more, outside them                | on-chain only (`onchain_unavailable` without an address)    |

- **The lot**: your server has no wallet and makes no address. The merchant's app gives the
  integration a lot of up to 100 taproot addresses (`bc1p…`) of its wallet, and fills it again
  whenever the merchant opens the app. `Merchant(ctx)` says how many it holds
  (`OnchainAddresses`, 0: no on-chain payment yet).
- **Leases: one address, one order**: an order leases an address in the merchant's registry until
  its invoice's end plus 120 seconds; two servers never keep the same one. The lease is published,
  read again on every relay of the merchant (it is confirmed when no other order holds the
  address), then marked confirmed: only then is the address the order's. A lease that was not
  confirmed is withdrawn. An address the explorer shows paid already, or that a witness (below)
  names, is skipped.
- **Unconfirmed, then paid**: `GetStatus` says `unconfirmed` once enough was sent in time, then
  `paid` (`Method` `onchain`) at 3 confirmations on the explorer (mempool.space, or the merchant's
  Bitcoin node): about 30 minutes. Ship the order only once it is `paid`. `WaitForPayment` watches
  the address on the explorer's WebSocket meanwhile; a server that cannot wait that long checks
  again later with `GetStatus` (`CreateInvoice` for the same `Ref` fails with `invoice_pending`
  while it is being confirmed).
- **In time**: a transaction counts for the order when it came while the lease ran. The package
  records each transaction it sees in the merchant's registry (a witness), so that the merchant's
  app tells the same order paid: check the payment while the customer pays (your page's polling,
  or `WaitForPayment`), until the invoice's end and its 120 seconds. A transaction no check saw by
  then is told by BlockCypher's time; one received after the end is late: it reaches the merchant,
  but pays no order. A replacement (RBF, a fee bump) of a payment in time counts for it. A
  transaction counts for one invoice at most: one that a witness of another invoice names counts
  for nothing here.
- **Underpaid**: `Received` is what came in time. Below the amount (and unless the merchant's
  tolerance or acceptance, below, accepted it), the invoice stays `open` until its end, then
  `expired` with `Received` above 0: the merchant received it, settle it with the customer. An
  overpayment is `paid`.
- **The merchant's tolerance**: the merchant may accept, in its app, an on-chain payment a little
  short of the amount (up to 10 %) as paying the order in full. An order keeps the tolerance of its
  creation, whatever the merchant sets later. A payment short by no more than it is
  `unconfirmed`, then `paid`, with `Received` below the amount: ship the order as for any `paid`
  status. One short by more is underpaid: its whole rest is asked. The tolerance is the
  merchant's: the package never gives it to your server, nor to your customer; the page of a
  checkout link learns only that this payment counts (an acceptance record), and sends the
  customer back to your shop.
- **The merchant's acceptance**: the merchant may also accept, by hand in its app, what an order
  underpaid on-chain received as paying it in full: one short by more than its tolerance, which
  the merchant would rather settle at what came. Nothing to configure, nothing to call: every
  check (`GetStatus`, `WaitForPayment`, and `CreateInvoice`'s check of an order's earlier
  invoices) reads it on every relay of the merchant with the order's witnesses, and counts it only
  when a majority of those relays hold it. The order is then `unconfirmed`, then `paid` (`Method`
  `onchain`, or `mixed` with invoices of the rest), with `Received` below the amount, as with the
  tolerance: ship it as for any `paid` status. `WaitForPayment` sees it at its next check of the
  on-chain part (within 60 s), the `Rest` still given until then. Only the merchant's team can
  make one: neither your server's key nor a customer can. The page of a checkout link learns of it
  as of the tolerance, through an acceptance record. The limit (SPEC.md §3, §27): a relay that
  holds the acceptance and fails at a check after the invoice's end may make that check give
  `expired`, and a later one `unconfirmed`, then `paid`: an order you cancelled then is settled by
  hand.
- **The rest, in Lightning**: the check that sees an underpayment (`GetStatus`, `WaitForPayment`)
  asks the merchant's Lightning address for an invoice of the rest, until the invoice's end,
  declares it in the merchant's registry and publishes it for the order's checkout links, whose
  page shows it at once. `open` carries it in `Rest`:

  ```json
  {
    "state": "open",
    "expiresAt": 1791378900,
    "received": "20000",
    "rest": {
      "sats": "29000",
      "bolt11": "lnbc290u1p…",
      "paymentUri": "bitcoin:bc1p…?amount=0.00029&lightning=lnbc290u1p…"
    }
  }
  ```

  `received` is what came in time, `rest.sats` what is left, `rest.bolt11` its invoice (null while
  there is none: it is asked at the next check). From 1 000 sats the rest is payable on-chain to
  the same address too; below, in Lightning only (`paymentUri` is `lightning:…`, or null while its
  invoice is asked). No invoice of the rest is asked with less than a minute left, nor outside the
  Lightning address's bounds; a failure is logged as a warning, never returned: the next check
  asks again. Once the rest is paid, the order is `unconfirmed` until its on-chain part is 3
  confirmations deep, then `paid` with `Method` `mixed`: keep its `Txids` and the `Preimages` of
  the invoices of the rest.

- **Who sees what**: the explorer (mempool.space, or the merchant's Bitcoin node, and
  mempool.space too when the node fails and its fallback allows it) sees your server's IP address
  and the addresses and transactions it asks about or watches; BlockCypher, a transaction no check
  saw in time. Never a key. A payment counts as made on what that one explorer shows.

### The merchant's Bitcoin node

The merchant may have the blockchain read from a node of its own that serves mempool.space's API
(an Umbrel, a server of its own) rather than from mempool.space. It sets it in its app, which
writes it in the integration's channel: nothing to configure on your server. Every call reads it
with the channel; every read of an address and of the chain's height (when an address is leased,
at each check), and `WaitForPayment`'s WebSocket, then go to the node: `http://192.168.1.10:3006`
is read at `http://192.168.1.10:3006/api` and `ws://192.168.1.10:3006/api/v1/ws` (`wss://` for an
`https://` node). BlockCypher and the `Mempool` rate source stay BlockCypher's and mempool.space's.

- **Its URL** follows a rule of its own (SPEC.md §31): `https`, or `http` for a host of a local
  network only (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, a name under `.local`);
  an IPv4 address or a name, never `localhost`, 127.0.0.0/8 nor 0.0.0.0/8; no user name, query nor
  fragment. A node the channel names in any other way, or with an invalid `fallback` or `payers`,
  is none: mempool.space is read, and the channel stays valid.
- **Its `fallback`**, when the node does not answer a read (no answer, an error status, a redirect,
  an answer out of format):
  - `"mempool"`: the same read of mempool.space, `network` when both fail. A node that failed less
    than 60 s ago in the same call (`CreateInvoice`, `GetStatus`, one `WaitForPayment`) is asked
    after mempool.space, so that a node that is down does not make each read wait for its timeout.
    The WebSocket goes to mempool.space's when the node's fails, or is lost before any message.
  - `"none"`: mempool.space is never asked. While the node does not answer, no address is leased
    (`CreateInvoice` gives a Lightning invoice only, or `network` for an order paid on-chain only)
    and every check of an order with an address fails with `network`: never `expired` nor final
    underpaid for want of an answer. The order waits until the node answers again.

  A check's two reads (the address's transactions and the chain's height) always come from one
  explorer: when the node fails either, both are asked of mempool.space.

- **A node on the merchant's network** (`http://192.168.1.10:3006`, `http://umbrel.local`) is
  reachable only by a server on that network: a shop the merchant hosts itself, or one that
  joined it (by Tailscale, for one). Any other server fails every read of it, and its fallback
  applies: with `"none"`, no on-chain payment at all.
- **`payers`**: with `true` (only for a node of the Internet: `https` and a name, not under
  `.local`), the checkout links' records name the node (its `url` and `fallback`, never `payers`),
  and their page reads the chain from it too, with the same fallback; the node then sees the
  customer's IP address and the address the page follows, and whoever holds a link reads the
  node's URL. With `false`, only your server reads the node, and the page reads mempool.space.
- **Security** (SPEC.md §27): a node's URL is the merchant's choice, and the package follows a
  valid one: GET requests at the paths of mempool.space's API under `/api`, and the WebSocket's two
  messages, in clear HTTP too to a host of a local network, and to an IPv4 address. Nothing is sent
  but deposit addresses, no redirect is followed, and every answer is bounded and checked. But if
  your server holds the keys of several merchants (a shared host), one merchant could so have it
  ask machines of your own network: restrict what your server can reach to public addresses (see
  [Outbound requests](#outbound-requests)); such a merchant's node is then unreachable, and its
  fallback applies. The node is trusted as mempool.space is: what it shows decides whether an
  order is paid. Over `http`, whoever is on the network between your server and the node can
  change its answers: it is as safe as that network.

## Prices in a currency

Give `Fiat` instead of `Sats`: the package converts the price to sats on your server, at the rate
of a price source, and declares the price to the merchant's app, which shows it. Nothing to set
up: without `WithRateSource`, it asks the default list of sources (below).

```go
invoice, err := client.CreateInvoice(ctx, checkout.Order{
	Ref:  "ORD-1043",
	Fiat: &checkout.Price{Amount: "24.50", Currency: "CHF"}, // a string: never a float
})
// 24.50 CHF at 69 012 CHF/BTC: ⌈2450 × 10^8 / 6 901 200⌉ = 35 502 sats
```

- `Currency` is an ISO 4217 code in upper case, of the ISO 4217 currencies (List One, without
  funds, metals and test codes). `Amount` is a string of digits with at most the currency's
  decimals (2 for CHF, 0 for JPY, 3 for KWD): those the merchant's app uses, from the package's own
  table, never from the platform's locale data. `checkout.CurrencyDigits("CHF")` gives them, with
  no client and asking nothing: whether your shop may offer its customers to pay in its currency,
  and in how many decimals. It gives none (false) for any other text, even `"chf"` or `" CHF"`.
  `checkout.FiatToSats` gives the conversion.
- The conversion is exact, on integers: the price of one bitcoin is read as the decimal its source
  wrote, and the sats are **rounded up** to the next whole sat, so that the merchant never receives
  less than the price.
- The sats must fit the bounds of the merchant's Lightning address (`Merchant(ctx)`), as for an
  amount in sats.

### Rate sources

Without `WithRateSource`, the package asks these sources **one after the other**, the next one only
when the previous one does not answer, gives no usable price, or has no price in the currency
(`checkout.DefaultRateSources()`): `CoinGecko`, then `Mempool`, `Coinbase`, `Kraken` and
`OpenNode`. The dated sources come first (a dated price is checked for its age), then the undated
ones. The first usable price is the rate, and `Invoice.Rate.Source` names the source that gave it.
Each source that failed before it is reported to your logger as a warning.

To choose, give sources to the client or to an order (an order's win):

```go
checkout.New(key, checkout.WithRateSource(checkout.Kraken))                     // this source only
checkout.New(key, checkout.WithRateSource(checkout.Kraken, checkout.CoinGecko)) // asked in this order
client.CreateInvoice(ctx, checkout.Order{Ref: ref, Fiat: fiat,
	RateSources: []checkout.RateSource{checkout.Mempool, checkout.Coinbase}}) // this order only
```

A list holds at least one source, no name twice; anything else is `invalid_option`
(`invalid_order` for an order). Each source asked may take up to the HTTP timeout: a list of 5
sources that all fail to answer may take 5 times as long.

| Source       | Currencies                  | Dated | Notes                                                                                                         |
| ------------ | --------------------------- | ----- | ------------------------------------------------------------------------------------------------------------- |
| `CoinGecko`  | many                        | yes   | No key needed. Optional: a Demo key (`WithCoinGeckoAPIKey`), sent to CoinGecko only. Without one, limited by IP. |
| `Mempool`    | USD EUR GBP CAD CHF AUD JPY | yes   | mempool.space.                                                                                                |
| `Coinbase`   | many                        | no    | Prices are decimal strings, read as written.                                                                  |
| `Kraken`     | Kraken's pairs              | no    | The last trade's price.                                                                                       |
| `OpenNode`   | ~140                        | no    |                                                                                                               |
| yours        | yours                       | maybe | `checkout.CustomRateSource(func(ctx, currency) (checkout.RateQuote, error))`; `Rate.Source` is `custom`.      |

Any other source (a CoinGecko Pro key, your own mempool instance, several sources checked against
each other) is a function. It gives the price as a **string**, as its source wrote it: a number
may already have lost digits.

```go
rates := checkout.CustomRateSource(func(ctx context.Context, currency string) (checkout.RateQuote, error) {
	// Your service answers {"price": "69012.50", "time": 1791378000}: the price as a string.
	price, at, found, err := myRates.Get(ctx, currency)
	switch {
	case err != nil:
		return checkout.RateQuote{}, checkout.NewNetworkError("rates.example.ch did not answer", err)
	case !found:
		return checkout.RateQuote{}, checkout.NewUnsupportedCurrencyError(currency, "rates.example.ch")
	}
	return checkout.RateQuote{Price: price, Time: at}, nil
})
client, err := checkout.New(key, checkout.WithRateSource(rates))
```

A price its source dates is refused when it is more than an hour old, or more than 5 minutes
ahead: the next source of the list is then asked. When no source gives a price, `CreateInvoice`
fails with `unsupported_currency` if none of them has the currency, else the last `rate` (no usable
price) or `network` (no answer) error, both retryable. Your function returns
`NewUnsupportedCurrencyError` when it has no price in a currency, `NewNetworkError` or
`NewRateError` when it cannot give one now: the next source is then asked. Any other error of yours
becomes a `rate` error (yours as its cause, `errors.Unwrap`); an `Error` of this package of
another code stops the list and is returned as it is.

## Errors

Every error of the package is a `*checkout.Error`, read with `errors.As`, with a stable `Code`
(a typed constant) and `Retryable()`; but for a cancelled call, which fails with its context's
error (`errors.Is(err, context.Canceled)`, or `context.DeadlineExceeded`), whatever it was doing.
None ever holds a secret: neither the key, nor a sale's key, nor any key derived from them. What
failed below (a relay's refusal, your rate function's error) is its `Unwrap`.

| Code                       | Constant                  | When                                                                                                                                       | Retry?           |
| -------------------------- | ------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ | ---------------- |
| `invalid_key`              | `CodeInvalidKey`          | The key is not an integration key (another prefix, damaged, an old `https://…/pay#…` link).                                                | no               |
| `invalid_option`           | `CodeInvalidOption`       | An option of the client or of `WaitForPayment` is invalid: `Param` names it.                                                               | no               |
| `invalid_order`            | `CodeInvalidOrder`        | A parameter is invalid: `Param` names it (`ref`, `sats`, `fiat`, `rateSource`, `durationSeconds`, `note`, `checkout`, `redirectUrl`, `invoice`). | no          |
| `revoked`                  | `CodeRevoked`             | The merchant removed the integration.                                                                                                      | no               |
| `channel_not_found`        | `CodeChannelNotFound`     | The relays answered without the integration's channel (`NewerFormat`: update the package).                                                 | when not newer   |
| `network`                  | `CodeNetwork`             | A relay, breez.tips, a rate source or an explorer did not answer, or fewer than a majority of the merchant's relays took a record.         | yes              |
| `invalid_invoice`          | `CodeInvalidInvoice`      | An invoice or a payment answer failed the checks. Never show such an invoice.                                                              | no               |
| `invoice_pending`          | `CodeInvoicePending`      | An earlier invoice of this order may still be paid, or is being confirmed on-chain: `Invoice` (and `CheckoutURL` with `Checkout`).         | after it expires |
| `order_paid`               | `CodeOrderPaid`           | An earlier invoice of this order was paid: `Invoice`, `Status`, `Preimage` ("" on-chain).                                                  | no               |
| `unsupported_currency`     | `CodeUnsupportedCurrency` | The rate source has no price in this currency (`Currency`).                                                                                | no               |
| `rate`                     | `CodeRate`                | No usable price: invalid, too old, or your rate function failed.                                                                           | yes              |
| `checkout_unavailable`     | `CodeCheckoutUnavailable` | `Checkout`, and the merchant's app gives no web app for checkout links.                                                                    | no               |
| `onchain_unavailable`      | `CodeOnchainUnavailable`  | The order can only be paid on-chain, and no address of the integration's lot could be leased.                                              | yes              |

## What `CreateInvoice` does

1. Reads the integration's channel on the Nostr relays of the key (signed by the merchant's team,
   following up to 3 moves of the merchant's relays): the merchant's Lightning address, registry
   and relays, or `removed`. At every call: a removal stops the next invoice.
2. Checks that no earlier invoice of the order may still be paid (its payment checked: one already
   paid is never handed out again, `order_paid`; one underpaid on-chain gets the invoice of its
   rest, as with `GetStatus`, before `invoice_pending`). Meanwhile, reads the Lightning address
   and, for a price in a currency, the rate.
3. Converts the price to sats, when it is in a currency; its methods follow from the amount.
4. With Lightning, asks the merchant's Lightning address for the invoice (LUD-06/16), and checks
   it: Bitcoin mainnet, exactly the amount, the merchant's description hash, a verify URL (LUD-21)
   on the same host and port, no longer than asked, dated within 5 minutes.
5. With on-chain, leases an address of the integration's lot that the explorer shows unpaid:
   publishes the lease on a majority of the merchant's relays, reads the address's leases again on
   every relay to confirm it, then marks it confirmed. A lease that was not confirmed is withdrawn,
   and the next address is tried. Without an address, Lightning only (or `onchain_unavailable`,
   or `network` when the explorer did not answer).
6. With `Checkout`, publishes its checkout link on a majority of the merchant's relays. Without a
   majority, no invoice (nothing is declared).
7. With Lightning, declares it in the merchant's registry, on a majority of the merchant's relays:
   so that its payment counts for this order's sale and for no other. Without a majority, no
   invoice.

A confirmed lease is never withdrawn, whatever fails after it (its mark, the link, the
declaration): it holds its address until its end, and the order's next `CreateInvoice` may hand
its invoice out (`invoice_pending`).

`GetStatus` asks the invoice's verify URL (LUD-21) and accepts a Lightning payment only with the
preimage of its payment hash, in an answer about this very invoice; with an address, it asks the
explorer at the same time and reads the witnesses of the order (see
[On-chain payments](#on-chain-payments)).

A call opens one connection per relay, used for all it reads and writes there, and closes them
when it is done. Reads and writes go on as soon as a majority of the relays answered (each
declaration is on a majority of them, and any two majorities share a relay): a slow or down relay
slows nothing. Three reads decide that a record is nobody else's and wait for every relay instead,
up to the relay timeout after a majority answered: a lease's confirmation, the witnesses read
again after one is published, and the choice among several invoices of an order that may still be
paid. A relay that failed earlier in the call is not waited for again. A write to the relays past
the majority goes on in the background once the call returned, for the relay timeout at most: it
waits for their answers, authenticating and writing again once when a relay asks for it (NIP-42),
then its connections close. Nothing else a call started runs on once it returned: a read returns
once the relays it went on without stopped (they ask nothing more), and `WaitForPayment` once its
checks did.

HTTP requests (the merchant's Bitcoin node's included) follow no redirect, read at most 1 MiB of
an answer's body and 10 MiB of its head (status line and headers), and carry
`User-Agent: sattleme-Checkout/1`, the same in every implementation of the specification. The
WebSocket handshake (of the relays and of the explorer) follows no redirect either,
carries the same User-Agent (relays behind a bot protection refuse a handshake without one), and
offers no compression. Of what a relay sends, a read keeps only what opens with its keys and, for a
signed record, whose signature checks out, once per event and without the members a relay may add to
it (of a lease, only its id, author, date, signature and end; of any other event, its id and its
date): a relay cannot make the server's memory grow.

## Outbound requests

Besides fixed hosts (the rate sources, mempool.space, BlockCypher), your server asks hosts that
others choose: the merchant's Lightning address and its provider (breez.tips), the merchant's
relays, and the merchant's Bitcoin node when its app names one. Their URLs are checked first:
`https` or `wss` only, no IP address, no `localhost` nor a name under it, no redirect followed;
but for the node, whose URL allows `http` (and `ws`) to a host of a local network and an IPv4
address ([The merchant's Bitcoin node](#the-merchants-bitcoin-node)), never `localhost`. Plain
http goes nowhere else: the package refuses it for any other URL (Lightning callbacks, verify
URLs, stored invoices), whatever the transport. A name may still lead into your own network (an
internal name, a public name resolving to a private address), and the package does not resolve
names itself (a name that resolves again would bypass such a check). If your server holds the
keys of several merchants, or sits in a network with services of its own, restrict what it can
reach to public addresses: in your network (an egress proxy, a firewall), or in the HTTP transport
and WebSocket dialer you give the client (`WithHTTPTransport`, `WithWebSocketDialer`); a
merchant's node on a private address is then unreachable, and its fallback applies.

The transport carries the node's requests as they are: an `http://` URL for a node of a local
network, which an `http.Transport` (the package's own, or yours) sends in clear over its
`DialContext`, never its `DialTLSContext`, and through its `Proxy`: the package's own takes the
environment's, as `http.DefaultTransport` does, so `HTTP_PROXY` would get the node's requests
(set `NO_PROXY` for the node's network). The WebSocket dialer gets the node's `ws://` URL the same
way; `NewWebSocketDialer(transport)` dials it over the transport. A transport or a dialer of yours
that refuses plain connections, or private addresses, leaves such a node unreachable: its
fallback applies.

Each request is bounded by its timeout, the wait for the connection included: the name's DNS
lookup runs within the request's context, and the call's cancellation stops it.

## Limits

- One hour at most per invoice; on-chain, about 30 minutes more to be `paid` (3 confirmations).
- Not measured: the limits of breez.tips, of mempool.space and of the relays for a shop that makes
  many invoices from one IP address. BlockCypher takes 100 requests an hour from an IP address
  without a token: it is asked only about a transaction no check saw while its lease ran.

Not in this version: refunds, webhooks, and showing open (unpaid) invoices in the merchant's app.

## Options

```go
client, err := checkout.New(key, // required
	checkout.WithRateSource(checkout.CoinGecko),          // a source or a list of them; DefaultRateSources()
	checkout.WithCoinGeckoAPIKey(demoKey),                // CoinGecko's Demo key (visible ASCII), sent to CoinGecko only
	checkout.WithHTTPTransport(transport),                // an http.RoundTripper; the package's own by default
	checkout.WithWebSocketDialer(dialer),                 // the relays' and the explorer's WebSocket; NewWebSocketDialer(nil)
	checkout.WithRelayTimeout(10*time.Second),            // per relay request, connection included: 100 ms to 60 s
	checkout.WithHTTPTimeout(10*time.Second),             // per HTTP request, body included: 100 ms to 60 s
	checkout.WithLogger(checkout.LoggerFunc(func(m string) { log.Print(m) })), // warnings only, never a secret; slog by default
	checkout.WithClock(time.Now),                         // the server's clock (tests)
	checkout.WithRandom(rand.Reader),                     // the CSPRNG (tests)
)
```

Every method takes a `context.Context` first: its cancellation or deadline stops the call's
requests, and the call fails with the context's error. An invalid option is `invalid_option` at
once, `Param` naming it (the names of SPEC.md §4: `relayTimeoutMs`, `httpTimeoutMs`, `logger`…).

What you give the client is called from several goroutines within one call (a call reads every
relay of a list at once, checks several invoices at once), and by calls at once: the HTTP
transport, the WebSocket dialer and its connections, the logger, the clock, the random source and
your rate sources must be safe for concurrent use. Once a call returned, nothing it started calls
them but to close what it opened (its WebSocket connections, an HTTP answer that comes once its
request was let go), and a write's relays past the majority, for the relay timeout at most: they
use their WebSocket connections, and the clock and the random source for their one authentication
(SPEC.md §13).

The default WebSocket dialer (`NewWebSocketDialer(nil)`) follows no redirect, offers no
compression, caps a message at 1 MiB and bounds the handshake by the relay timeout. A dialer of
yours should do the same.

## Other languages

The package's public API is the package `checkout`: `Client`, its types and its errors; everything
under `internal/` is internal. Implementations in other languages follow
[`SPEC.md`](https://github.com/sattleme/checkout-spec/blob/main/SPEC.md) and pass its conformance
vectors; the invoices they store are the same JSON.

## Dependencies

| Module                                                          | Version  | Why                                                                                                                                         |
| --------------------------------------------------------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `github.com/btcsuite/btcd/btcec/v2` (with its `schnorr` package) | v2.5.1   | secp256k1: BIP-340 signatures of Nostr events, x-only keys, and the ECDH of NIP-44 and of the declarations. The standard library has no secp256k1. |
| `golang.org/x/crypto` (`chacha20`)                              | v0.59.0  | ChaCha20 of NIP-44 v2, which the standard library does not export.                                                                          |
| `github.com/coder/websocket`                                    | v1.8.15  | The relays' and the explorer's WebSocket (the standard library has none): no redirect followed (a test proves it), no compression offered, a read limit of 1 MiB, its handshake bounded by the relay timeout and carrying the User-Agent, over an injectable HTTP transport. |

`btcec/v2` brings its own dependencies, pinned in `go.sum` like the others:
`github.com/decred/dcrd/dcrec/secp256k1/v4` v4.4.1, the pure Go secp256k1 arithmetic that
`btcec/v2` wraps (the Decred project's, used by btcd and dcrd; not constant-time where it signs
and computes an ECDH, see [Security](#security)), and two packages it imports,
`github.com/decred/dcrd/crypto/blake256` v1.1.0 and `github.com/btcsuite/btcd/chainhash/v2`
v2.0.0 (hashes this package never uses). The tests check `btcec/v2` against BIP-340's own test
vectors and the official NIP-44 vectors.

`go.mod` also names `golang.org/x/sys` v0.50.0, which `golang.org/x/crypto`'s own `go.mod`
requires. Its package `cpu` is built into this one on s390x and loong64 only, where ChaCha20 asks
it whether the processor has vector instructions (unless built with the tag `purego`); on every
other architecture, none of its packages is.

Everything else is the standard library (Go 1.26.10 or later): HKDF (`crypto/hkdf`),
HMAC-SHA256, the HTTP client (no redirect followed, an injectable transport, bounded bodies and
whole-request timeouts), and the package's own JSON reader and writer (as SPEC.md §2 reads JSON:
the last of members with one name, numbers as doubles, strict UTF-8). `go.sum` is committed, and `go mod verify` checks it
in the CI.

## Security

Report a vulnerability privately, never in a public issue: see [SECURITY.md](SECURITY.md).

**Signing and ECDH are not constant-time.** The package signs Nostr events (BIP-340) and computes
the ECDH of NIP-44 with `github.com/btcsuite/btcd/btcec/v2` v2.5.1 over
`github.com/decred/dcrd/dcrec/secp256k1/v4` v4.4.1, whose multiplications of a point by a secret are
variable-time: a signature computes its nonce's point with `ScalarBaseMultNonConst`
(`schnorr/signature.go`), a key's public key is computed the same way, and an ECDH multiplies with
`ScalarMultNonConst` (`ecdh.go`): the library's field operations are constant-time, as it says, not
these multiplications. So the time of a signature or of an ECDH may depend on the secret it uses:
each order's sale secret (its ECDH with the merchant's reading key too), each invoice's rest secret,
the integration's seen secret, a checkout link's author secret, all derived from the integration key
but the last. A process that shares the server's CPU (another tenant of a shared host or of the same
physical machine) could in principle time many of them and learn about those keys. Run the package
where no code you do not trust shares the CPU, as you would for any server that holds a key.

## Development

```sh
scripts/check.sh   # or: make check
```

runs every check, as the CI does: `spec` (the pinned copy of the specification), `format` (gofmt),
`vet` (here, and for 32-bit `386`: the tests build there too), `lint` (staticcheck, pinned), `mod`
(go.mod and go.sum tidy and verified), `test` (every test with the race detector, which needs cgo:
a C compiler), `build`, `vuln` (govulncheck, pinned). `scripts/check.sh test lint` runs those
named.

The tests run local Nostr relays and fakes of breez.tips, of the rate sources, of the explorers
(mempool.space and a merchant's Bitcoin node, REST and WebSocket) and of BlockCypher, reached
through the injected HTTP transport and WebSocket dialer under their real host names: no network.

`spec/` is a copy of [sattleme/checkout-spec](https://github.com/sattleme/checkout-spec) pinned to
one commit (`spec/PIN.json`, checked file by file by `go run ./internal/cmd/spec check` and the
tests), never edited here. Its vectors are what the tests check the package against: the
conformance vectors, the official NIP-44 vectors, the merchant's app's vectors
(`spec/vectors/app/app.json`, `onchain-lot.json`) and the vectors the merchant's apps read
(`spec/vectors/app/implementation-declaration.json`, `checkout-link.json`, `checkout-rest.json`),
which the package's own declarations, links and records match. A test checks that every vector
file is read. To take a newer version of the specification: in a clone of checkout-spec, check out
the commit (a tagged version when there is one), then here
`go run ./internal/cmd/spec import <clone>`, and run the checks.

## Releasing

A version is a tag `vMAJOR.MINOR.PATCH` on a commit of `main` whose `CHANGELOG.md` has its entry
(`## MAJOR.MINOR.PATCH - YYYY-MM-DD`, its release date). Go's module proxy serves the tagged commit to whoever asks for that
version: nothing is uploaded anywhere. The release workflow (`.github/workflows/release.yml`) runs
the CI again on the tagged commit and checks that the tag is a valid version, in the changelog,
and on `main`; then, once a maintainer approved it in the protected environment `release`
(Settings, Environments: required reviewers, deployments from `v*` tags only), it asks the module
proxy for the version, which fetches it (pkg.go.dev lists it from there). No credential is
stored anywhere.

Protect the `v*` tags with a ruleset of the repository (Settings, Rules: target tags `v*`, restrict
creations, updates and deletions to the maintainers): a version, once fetched by the proxy, is that
version for good, and a moved tag would serve another commit to nobody.

## License

[MIT](LICENSE)
