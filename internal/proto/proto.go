// Package proto holds every constant of the protocol, in one place: SPEC.md §28 gives each of
// them, and every implementation, in any language, uses these values. Those marked "never
// changes" are fixed by data already published (keys, channels, declarations): changing one loses
// merchants' sales or stops their integrations.
package proto

import "strconv"

// IntegrationKeyPrefix begins every integration key (sattleme: sattle.me, ck: checkout key).
// Never changes: a later format of the key takes another prefix (sattleme_ck2_).
const IntegrationKeyPrefix = "sattleme_ck_"

// KeyBytes is the length of the team's public key and of the integration's key.
const KeyBytes = 32

// HKDF-SHA256 salts (UTF-8). Never change.
const (
	IntegrationSalt = "sattle.me/integration"
	SaleSalt        = "sattle.me/sale"
	DeclarationSalt = "sattle.me/registry-declaration"
)

// HKDF-SHA256 info strings (UTF-8). Never change.
const (
	ChannelAddressInfo    = "channel/address/v1"
	ChannelEncryptionInfo = "channel/encryption-key/v1"
	TakerInfo             = "taker/v1"
	SaleNostrSecretInfo   = "nostr-secret-key/v1"
)

// SaleInfo is the info of the sale key of the order ref.
func SaleInfo(ref string) string { return "sale/" + ref + "/v1" }

// OuterInfo is the info of the encryption key of a declaration at address by author.
func OuterInfo(address, author string) string { return "outer/v1/" + address + "/" + author }

// TakerTagBytes is the length of HMAC-SHA256(taker key, author) kept as the taker tag.
const TakerTagBytes = 16

// RecordKind is NIP-78 application data: every record of the app.
const RecordKind = 30078

// AuthKind is NIP-42 authentication.
const AuthKind = 22242

// RecordBlockBytes: a record's plaintext is padded with spaces to a multiple of this many bytes.
const RecordBlockBytes = 256

// The channel.
const (
	ChannelRecordType = "team-channel"
	ChannelVersion    = 1
	// MaxRelayMoves: at most this many moves of the merchant's relays are followed.
	MaxRelayMoves = 3
	// MaxBusinessNameLength is the longest business name, in Unicode code points.
	MaxBusinessNameLength = 200
)

// The declaration.
const (
	DeclarationRecordType = "registry-declaration"
	DeclarationVersion    = 1
	// SatsCurrency is the currency of a sale declared in sats: its minor is the sats, its
	// digits 0.
	SatsCurrency = "SAT"
	// MaxDeclaredDigits is the most decimals a declared price may have.
	MaxDeclaredDigits = 8
	// MaxMinorUnits is the largest amount in minor units (2^53 − 1: a JSON number every reader
	// holds exactly).
	MaxMinorUnits = 9_007_199_254_740_991
)

// Checkout links (SPEC.md §30): a URL of the merchant's web app that shows the customer the
// invoice, checks its payment and sends the customer back once it is paid. The salt and info
// strings never change: the merchant's web app derives the same keys.
const (
	CheckoutSalt           = "sattle.me/checkout"
	CheckoutAddressInfo    = "address/v1"
	CheckoutEncryptionInfo = "encryption-key/v1"
	CheckoutRecordType     = "checkout"
	CheckoutVersion        = 1
	// CheckoutOnchainVersion is the version of a checkout link's record for an invoice with an
	// address.
	CheckoutOnchainVersion = 2
	// CheckoutMarker is the first part of a checkout link's fragment: a link of this version.
	// Never changes.
	CheckoutMarker = "c"
	// CheckoutPath is the page of the merchant's web app that reads checkout links.
	CheckoutPath = "/pay"
	// CheckoutLifeSeconds: a checkout link's record lives this long after its invoice's end
	// (NIP-40): a week.
	CheckoutLifeSeconds = 604_800
	// MaxRedirectURLLength is the longest redirect URL, in characters (ASCII only).
	MaxRedirectURLLength = 2048
	// MaxRedirectSchemeLength is the longest scheme of an app's redirect URL.
	MaxRedirectSchemeLength = 32
)

// refusedRedirectSchemes are the schemes a redirect URL never has: the browser's own, files and
// scripts; other browsers (they open any web page given in the rest of the link, whose host the
// page would not show: a web page is an https URL); other transports; any Android activity;
// payments (a second invoice right after the first); calls and messages; installing software or
// running system tools. https follows its own rule.
var refusedRedirectSchemes = map[string]bool{
	"about": true, "blob": true, "chrome": true, "chrome-extension": true, "content": true,
	"data": true, "file": true, "filesystem": true, "javascript": true, "moz-extension": true,
	"resource": true, "safari-extension": true, "vbscript": true, "view-source": true,
	"brave": true, "com-apple-mobilesafari-tab": true, "ddgquicklink": true, "firefox": true,
	"firefox-focus": true, "googlechrome": true, "googlechrome-x-callback": true,
	"googlechromes": true, "microsoft-edge": true, "microsoft-edge-http": true,
	"microsoft-edge-https": true, "onionbrowser": true, "onionbrowsers": true, "opera-http": true,
	"opera-https": true, "x-safari-http": true, "x-safari-https": true,
	"ftp": true, "http": true, "ws": true, "wss": true,
	"android-app": true, "intent": true,
	"bitcoin": true, "keyauth": true, "lightning": true, "liquidnetwork": true, "lnurl": true,
	"lnurlc": true, "lnurlp": true, "lnurlw": true,
	"callto": true, "facetime": true, "facetime-audio": true, "mailto": true, "sip": true,
	"sms": true, "tel": true,
	"itms-services": true, "ms-msdt": true, "search-ms": true,
}

// IsRefusedRedirectScheme tells whether a redirect URL never has the (lowercase) scheme.
func IsRefusedRedirectScheme(scheme string) bool { return refusedRedirectSchemes[scheme] }

// RefusedRedirectSchemeCount is the number of refused redirect schemes (for tests).
func RefusedRedirectSchemeCount() int { return len(refusedRedirectSchemes) }

// Relay lists and URLs.
const (
	// MaxRelays is the most relays a list holds.
	MaxRelays = 10
	// MaxRelayURLLength is the longest relay address (wss:// included).
	MaxRelayURLLength = 120
	// MaxHostLength is the longest host of a relay address, a checkout origin, a redirect URL or
	// an https URL.
	MaxHostLength = 253
	// MaxHTTPSURLLength is the longest https URL (a pay URL, a callback, a verify URL), in
	// characters (ASCII only).
	MaxHTTPSURLLength = 2048
)

// The order.
const (
	// RefRule says what an order's reference is (one order, one sale).
	RefRule            = "1 to 64 characters A-Z a-z 0-9 . _ : -"
	MaxRefLength       = 64
	MinDurationSeconds = 60
	MaxDurationSeconds = 3600
	// DefaultDurationSeconds is an invoice's duration when the order gives none.
	DefaultDurationSeconds = 900
	// MaxNoteLength is the longest note, in Unicode code points.
	MaxNoteLength = 100
)

// IsRef tells whether s is an order's reference: 1 to 64 of A–Z a–z 0–9 . _ : -.
func IsRef(s string) bool {
	if len(s) < 1 || len(s) > MaxRefLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-') {
			return false
		}
	}
	return true
}

const (
	// GraceSeconds: after an invoice's end, a payment of its last seconds may still be reported
	// (99 % within 103 s over 575 payments measured, SPEC.md §22): until then, "not paid" is not
	// final.
	GraceSeconds = 120
	// ClockToleranceSeconds: an invoice's date may be this far from the server's clock
	// (synchronized by NTP).
	ClockToleranceSeconds = 300
	// NotFoundReason is the LUD-21 refusal that means "never paid" once the grace period is over.
	NotFoundReason = "Not found"
)

// Rates.
const (
	// MaxRateAgeSeconds: a dated price older than this is refused; one dated further than the
	// clock tolerance ahead too.
	MaxRateAgeSeconds = 3600
	// MaxPriceLength is the longest price text a rate source may give.
	MaxPriceLength = 64
	// MaxPriceExponent is the largest exponent (either sign) of a price written in exponent
	// notation.
	MaxPriceExponent = 64
)

// Relays and HTTP.
const (
	// MaxEventsPerQuery is the most events read from one relay for one query.
	MaxEventsPerQuery = 1000
	// MaxPages is the most pages of a paged read.
	MaxPages = 20
	// CompleteReadLimit is the limit of each page of a complete read (SPEC.md §13, the leases of
	// deposit addresses, which anybody may write at): a page of fewer events is the relay's last.
	CompleteReadLimit = 100
	// UserAgent is the User-Agent of every HTTP request and WebSocket handshake: the same in every
	// implementation and version (CoinGecko refuses a request without one; one naming a language
	// or a version would help single it out).
	UserAgent = "sattleme-Checkout/1"
	// MaxRelayMessageBytes is the largest message read from a relay.
	MaxRelayMessageBytes = 1 << 20
	// MaxHTTPBodyBytes is the largest HTTP answer body read.
	MaxHTTPBodyBytes = 1 << 20
	// MaxParallelPaymentChecks: earlier invoices of an order checked at once (createInvoice).
	MaxParallelPaymentChecks = 4
)

// Timeouts and intervals, in milliseconds: the defaults and the bounds a caller may choose.
const (
	DefaultTimeoutMs  = 10_000
	MinTimeoutMs      = 100
	MaxTimeoutMs      = 60_000
	DefaultIntervalMs = 2000
	MinIntervalMs     = 250
	MaxIntervalMs     = 60_000
)

// BOLT11.
const (
	// MaxBolt11ExpiryWords is the longest x (expiry) field of a BOLT11 invoice, in 5-bit words
	// (35 bits, as its date).
	MaxBolt11ExpiryWords = 7
	// MaxBolt11AmountMsat is the largest BOLT11 amount: 21 million bitcoins, in millisatoshis.
	MaxBolt11AmountMsat = 2_100_000_000_000_000_000
)

// On-chain payments (SPEC.md §31): an order also takes a Bitcoin transaction to a deposit address
// of the merchant's wallet, leased from the lot the merchant's app gives the integration. The
// types, salts and info strings never change: the merchant's app reads the same records.
const (
	// OnchainFloorSats: below this amount, an order is Lightning only.
	OnchainFloorSats = 1000
	// MaxSats is the largest amount of an order: 21 million bitcoins, in sats.
	MaxSats        = 2_100_000_000_000_000
	SatsPerBTC     = 100_000_000
	LotRecordType  = "integration-onchain-lot"
	LotVersion     = 1
	LotAddressInfo = "onchain-lot/address/v1"
	// MaxLotAddresses is the most addresses a lot holds.
	MaxLotAddresses = 100

	LeaseRecordType = "registry-onchain"
	LeaseVersion    = 1
	// LeaseAddressPrefix: a deposit address's registry address is the HMAC of this, then the
	// address (UTF-8).
	LeaseAddressPrefix = "onchain/v1/"
	// LeaseLookbackSeconds: leases are read back this far: a lease ends at most 4 320 s after its
	// created_at (a BOLT11 date up to 300 s ahead, 3600 s, the grace period, the clock tolerance).
	LeaseLookbackSeconds = 7200
	// MaxAddressTries is the number of free addresses tried for one order.
	MaxAddressTries = 3

	WitnessRecordType  = "integration-onchain-seen"
	WitnessVersion     = 1
	SeenSecretInfo     = "onchain-seen/nostr-secret-key/v1"
	SeenAddressKeyInfo = "onchain-seen/address-key/v1"
	// OrderTag is the tag of a witness that names its order (its address and sale).
	OrderTag = "o"
	// MaxWitnessInputs: the coins of a transaction a witness keeps, its first 100.
	MaxWitnessInputs = 100
	// NoSale is the sale a witness names when the transaction came at no lease's time.
	NoSale = "none"

	// MempoolURL is mempool.space (Esplora REST): the explorer, the only one asked what the
	// blockchain shows.
	MempoolURL = "https://mempool.space/api"
	// MempoolWebSocketURL is mempool.space's WebSocket, which waitForPayment watches an address
	// on.
	MempoolWebSocketURL = "wss://mempool.space/api/v1/ws"
	// BlockCypherURL: when a transaction was received, asked only for one no check saw while its
	// lease ran.
	BlockCypherURL = "https://api.blockcypher.com/v1/btc/main"
	// Confirmations of an on-chain payment, on the explorer read.
	Confirmations = 3
	// OnchainCheckIntervalMs: the on-chain part is checked at least this often while
	// waitForPayment waits.
	OnchainCheckIntervalMs = 60_000
	// ReconnectFirstMs: the explorer's WebSocket, lost, is connected again after this, doubled at
	// each failure up to ReconnectMaxMs.
	ReconnectFirstMs = 1000
	ReconnectMaxMs   = 30_000
)

// The rest of an on-chain underpayment (SPEC.md §31): a Lightning invoice of what is left to pay,
// asked by a payment check, declared with rest and shown to the pages of the order's checkout
// links by a rest record. The type, the salts and the info strings never change: the merchant's
// web app reads the same records.
const (
	RestRecordType = "checkout-rest"
	RestVersion    = 1
	// RestEncryptionInfo is the HKDF info (salt CheckoutSalt) of a rest record's encryption key.
	RestEncryptionInfo = "rest/encryption-key/v1"
	// MinRestSeconds: a rest invoice is asked only with this long left before the invoice's end.
	MinRestSeconds = 60
)

// RestKeyInfo is the HKDF info of the rest key of the invoice of the order ref with the deposit
// address and the end expiresAt (Unix seconds, decimal): an invoice's own, never another invoice's
// of the order that leased the same address.
func RestKeyInfo(ref, address string, expiresAt int64) string {
	return "checkout-rest/" + ref + "/" + address + "/" + strconv.FormatInt(expiresAt, 10) + "/key/v1"
}

// RestSecretInfo is the HKDF info of the rest secret of that invoice.
func RestSecretInfo(ref, address string, expiresAt int64) string {
	return "checkout-rest/" + ref + "/" + address + "/" + strconv.FormatInt(expiresAt, 10) + "/nostr-secret-key/v1"
}

// RestAddressInfo is the HKDF info (salt CheckoutSalt) of the address of the rest record of the
// rest invoice with paymentHash.
func RestAddressInfo(paymentHash string) string { return "rest/address/" + paymentHash + "/v1" }

// The merchant's tolerance of on-chain underpayments (SPEC.md §31): an integer of basis points,
// from 0 to MaxTolerance (10 %); anything else is 0. An order whose payment in time reaches its
// amount less that share is paid.
const (
	MaxTolerance = 1000
	// ToleranceScale is the number of basis points in a whole.
	ToleranceScale = 10_000
)

// Acceptance records (SPEC.md §31): they tell the pages of an order's checkout links that the
// payment its tolerance accepted counts, never the tolerance. Sealed with the rest encryption key,
// signed by the rest secret, at an address the rest key derives (salt CheckoutSalt). The type and
// the info string never change: the merchant's web app reads them.
const (
	AcceptanceRecordType  = "checkout-accepted"
	AcceptanceVersion     = 1
	AcceptanceAddressInfo = "rest/accepted/v1"
)

// The merchant's acceptance (SPEC.md §31, "The merchant's acceptance"): the merchant accepts by
// hand, in its app, what an order underpaid on-chain received as paying it. A record of the team
// (never the integration's: it opens one but cannot make one), sealed with the channel's key, at
// an address the integration key derives (salt IntegrationSalt) from the order's sale author, the
// invoice's deposit address and its expiresAt (Unix seconds, decimal). The type and the info
// string never change: the merchant's app writes the same records.
const (
	MerchantAcceptanceRecordType = "integration-onchain-accepted"
	MerchantAcceptanceVersion    = 1
)

// MerchantAcceptanceInfo is the HKDF info (salt IntegrationSalt) of the address of the merchant's
// acceptance of the invoice of the sale author sale with the deposit address and the end
// expiresAt.
func MerchantAcceptanceInfo(sale, address string, expiresAt int64) string {
	return "onchain-accepted/" + sale + "/" + address + "/" + strconv.FormatInt(expiresAt, 10) + "/v1"
}

// The merchant's Bitcoin node (SPEC.md §31, "The merchant's Bitcoin node"): a node of the
// merchant's that serves mempool.space's API, which the channel names (bitcoinNode).
const (
	// MaxNodeURLLength is the longest node URL, in characters (ASCII).
	MaxNodeURLLength = 200
	// NodeFailedRecentlyMs: a node that failed a read less than this long ago, in the same call,
	// is asked after mempool.space (fallback "mempool").
	NodeFailedRecentlyMs = 60_000
)
