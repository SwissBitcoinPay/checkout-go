// Package lnurl talks to the merchant's Lightning address (LUD-16, LUD-06, LUD-12, LUD-21;
// SPEC.md §16, §17, §18, §22), as the merchant's point of sale does: plain HTTPS requests to the
// merchant's LNURL server (breez.tips), which makes invoices for the merchant's wallet even while
// it is offline. Never shown to the integrator: no message holds the address.
package lnurl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sattleme/checkout-go/internal/bolt11"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/httpx"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/text"
	"github.com/sattleme/checkout-go/internal/urls"
)

// answer is an LNURL answer: a JSON object, or the service's refusal.
type answer struct {
	json    *jsonv.Object
	refused bool
	reason  string
}

// isServiceError tells whether a body is an LNURL error answer (LUD-06): {"status": "ERROR", …}
// in any ASCII case.
func isServiceError(body any) bool {
	status, ok := jsonv.Field(body, "status")
	s, isText := status.(string)
	return ok && isText && text.LowerASCII(s) == "error"
}

// get asks for an LNURL answer (SPEC.md §16): a refusal ({"status": "ERROR"}) sent with a 2xx or
// a client error status (but 408 and 429); a server error, a timeout, a rate limit or anything that
// is not a JSON object is no answer: network.
func get(ctx context.Context, client *httpx.Client, rawURL string) (answer, error) {
	a, err := client.GetJSON(ctx, rawURL, nil)
	if err != nil {
		return answer{}, err
	}
	refusalStatus := a.Status >= 400 && a.Status < 500 && a.Status != 408 && a.Status != 429
	if (a.OK || refusalStatus) && isServiceError(a.Body) {
		reason, _ := jsonv.Field(a.Body, "reason")
		s, _ := reason.(string)
		return answer{refused: true, reason: s}, nil
	}
	o, isObject := jsonv.AsObject(a.Body)
	if !a.OK || !isObject {
		return answer{}, errs.NewNetwork(httpx.HostOf(rawURL)+" answered HTTP "+strconv.Itoa(a.Status)+" without an LNURL answer", nil)
	}
	return answer{json: o}, nil
}

// msat reads millisatoshis of a JSON number: a non-negative safe integer.
func msat(v any) (int64, bool) {
	n, ok := jsonv.AsSafeInteger(v)
	return n, ok && n >= 0
}

var lightningAddress = regexp.MustCompile(`^([a-z0-9\-_.+]+)@([a-z0-9-]+(?:\.[a-z0-9-]+)+)$`)

// AddressURL is the LNURL-pay endpoint of a Lightning address (LUD-16; its ASCII whitespace
// around it removed, its ASCII letters lowered, as the app reads it), false when it is not one.
// The endpoint is an https URL (SPEC.md §16): its domain is never an IP address, nor localhost.
func AddressURL(address string) (string, bool) {
	m := lightningAddress.FindStringSubmatch(text.LowerASCII(text.TrimASCII(address)))
	if m == nil {
		return "", false
	}
	u := "https://" + m[2] + "/.well-known/lnurlp/" + m[1]
	_, ok := urls.ParseHTTPS(u)
	return u, ok
}

// PayRequest is the LNURL-pay parameters of the merchant's Lightning address.
type PayRequest struct {
	// Callback is an https URL.
	Callback        string
	MinSendableMsat int64
	MaxSendableMsat int64
	// Metadata is the raw metadata: the invoices' description hash is its SHA-256 (LUD-06).
	Metadata string
	// CommentAllowed is the longest comment accepted (LUD-12), 0 when none.
	CommentAllowed int64
}

// FetchPayRequest reads the LNURL-pay parameters of a Lightning address (LUD-16).
func FetchPayRequest(ctx context.Context, client *httpx.Client, address string) (*PayRequest, error) {
	u, ok := AddressURL(address)
	if !ok {
		return nil, errs.NewInvalidInvoice("The merchant's Lightning address is invalid", nil)
	}
	a, err := get(ctx, client, u)
	if err != nil {
		return nil, err
	}
	if a.refused {
		return nil, errs.NewNetwork(httpx.HostOf(u)+" refused the Lightning address: "+a.reason, nil)
	}
	tag, _ := a.json.Get("tag")
	callback, _ := a.json.Get("callback")
	callbackText, isText := callback.(string)
	minValue, _ := a.json.Get("minSendable")
	maxValue, _ := a.json.Get("maxSendable")
	metadata, _ := a.json.Get("metadata")
	metadataText, metadataIsText := metadata.(string)
	min, okMin := msat(minValue)
	max, okMax := msat(maxValue)
	// Metadata holding a lone surrogate (an escape in the JSON) is no text, and has no UTF-8 to
	// hash (SPEC.md §6, §16): never its WTF-8 bytes hashed instead.
	if tag != "payRequest" || !isText || !okMin || !okMax || min > max || !metadataIsText || !text.IsWellFormed(metadataText) {
		return nil, errs.NewInvalidInvoice(httpx.HostOf(u)+" gave no valid LNURL-pay request", nil)
	}
	if _, ok := urls.ParseHTTPS(callbackText); !ok {
		return nil, errs.NewInvalidInvoice(httpx.HostOf(u)+" gave no valid LNURL-pay request", nil)
	}
	commentAllowed := int64(0)
	if v, _ := a.json.Get("commentAllowed"); v != nil {
		if n, ok := jsonv.AsSafeInteger(v); ok && n > 0 {
			commentAllowed = n
		}
	}
	return &PayRequest{
		Callback:        callbackText,
		MinSendableMsat: min,
		MaxSendableMsat: max,
		Metadata:        metadataText,
		CommentAllowed:  commentAllowed,
	}, nil
}

// DescriptionHashOf is the description hash of the invoices of pay (hex): the SHA-256 of the UTF-8
// of its metadata, the string its JSON gives (LUD-06; SPEC.md §16).
func DescriptionHashOf(pay *PayRequest) string {
	sum := sha256.Sum256([]byte(pay.Metadata))
	return hex.EncodeToString(sum[:])
}

// SendableSats is the whole sats the Lightning address accepts: its millisatoshi bounds rounded
// inwards (the minimum up, the maximum down), so that a whole-sat amount within them is within the
// service's.
func SendableSats(pay *PayRequest) (min, max int64) {
	return (pay.MinSendableMsat + 999) / 1000, pay.MaxSendableMsat / 1000
}

// InvoiceAnswer is what the Lightning address answered: an invoice and its LUD-21 verify URL, not
// checked yet.
type InvoiceAnswer struct {
	PR     string
	Verify string
}

// CallbackURL is the URL that asks the callback for an invoice of sats (LUD-06): amount in
// millisatoshis, expiry in seconds, and comment only when its UTF-8 bytes fit commentAllowed
// (LUD-12 does not say how a comment's length counts: bytes are never fewer than characters, so
// the service never refuses it). The callback's own query parameters are kept, set as
// URLSearchParams sets them; its fragment is dropped (the parameters would come after it, and
// never reach the server).
func CallbackURL(pay *PayRequest, sats int64, expirySeconds int, comment string) string {
	base := pay.Callback
	if i := strings.IndexByte(base, '#'); i >= 0 {
		base = base[:i]
	}
	query := ""
	if i := strings.IndexByte(base, '?'); i >= 0 {
		base, query = base[:i], base[i+1:]
	}
	params := parseQuery(query)
	params = setParam(params, "amount", strconv.FormatInt(sats*1000, 10))
	params = setParam(params, "expiry", strconv.Itoa(expirySeconds))
	if comment != "" && int64(len(comment)) <= pay.CommentAllowed {
		params = setParam(params, "comment", comment)
	}
	return base + "?" + serializeQuery(params)
}

// RequestInvoice asks the Lightning address for an invoice of sats. The answer is not checked:
// CheckInvoice does.
func RequestInvoice(ctx context.Context, client *httpx.Client, pay *PayRequest, sats int64, expirySeconds int, comment string) (InvoiceAnswer, error) {
	a, err := get(ctx, client, CallbackURL(pay, sats, expirySeconds, comment))
	if err != nil {
		return InvoiceAnswer{}, err
	}
	if a.refused {
		return InvoiceAnswer{}, errs.NewNetwork("The Lightning address refused the invoice: "+a.reason, nil)
	}
	pr, _ := a.json.Get("pr")
	verify, _ := a.json.Get("verify")
	prText, ok1 := pr.(string)
	verifyText, ok2 := verify.(string)
	if !ok1 || !ok2 {
		return InvoiceAnswer{}, errs.NewInvalidInvoice("The Lightning address gave no invoice or no verify URL (LUD-21)", nil)
	}
	return InvoiceAnswer{PR: prText, Verify: verifyText}, nil
}

// The checks of SPEC.md §18, in their order, as the vectors name them.
const (
	CheckBolt11          = "bolt11"
	CheckNetwork         = "network"
	CheckAmount          = "amount"
	CheckDescriptionHash = "description_hash"
	CheckVerifyURL       = "verify_url"
	CheckDuration        = "duration"
	CheckExpired         = "expired"
	CheckClock           = "clock"
)

func failed(check, message string) error {
	e := errs.NewInvalidInvoice(message, nil)
	e.Check = check
	return e
}

// CheckInvoice checks an invoice before it is declared or shown (SPEC.md §18): Bitcoin mainnet,
// exactly sats, the merchant's description hash (breez.tips serves the invoices of all its users),
// a verify URL in https on the callback's host, an expiry no longer than asked, a date within 5
// minutes of the clock, time left before its end. The error names the first check failed.
func CheckInvoice(a InvoiceAnswer, pay *PayRequest, sats int64, durationSeconds int, now int64) (*bolt11.Invoice, error) {
	decoded, ok := bolt11.Decode(a.PR)
	if !ok {
		return nil, failed(CheckBolt11, "Not a BOLT11 invoice")
	}
	if decoded.Network != "bc" {
		return nil, failed(CheckNetwork, "Not a Bitcoin mainnet invoice")
	}
	if !decoded.HasAmount || decoded.AmountMsat != sats*1000 {
		return nil, failed(CheckAmount, "The invoice's amount differs from the order's")
	}
	if decoded.DescriptionHash != DescriptionHashOf(pay) {
		return nil, failed(CheckDescriptionHash, "The invoice does not pay the merchant's Lightning address")
	}
	verify, ok := urls.ParseHTTPS(a.Verify)
	callback, _ := urls.ParseHTTPS(pay.Callback)
	if !ok || !urls.SameHost(verify, callback) {
		return nil, failed(CheckVerifyURL, "No verify URL (LUD-21) on the Lightning address's host")
	}
	if decoded.ExpirySeconds > int64(durationSeconds) {
		return nil, failed(CheckDuration, "The invoice lasts longer than asked")
	}
	if decoded.ExpirySeconds <= 0 {
		return nil, failed(CheckExpired, "The invoice has no time left")
	}
	if diff := decoded.Timestamp - now; diff > proto.ClockToleranceSeconds || diff < -proto.ClockToleranceSeconds {
		return nil, failed(CheckClock, "The invoice's date is too far from this server's clock")
	}
	// Dated up to 5 minutes back, a short invoice may already be over: never declared nor shown.
	if decoded.Timestamp+decoded.ExpirySeconds <= now {
		return nil, failed(CheckExpired, "The invoice has no time left")
	}
	return decoded, nil
}

// Invoice is what the Lightning part of a payment check needs of an invoice.
type Invoice struct {
	Bolt11      string
	PaymentHash string
	VerifyURL   string
	ExpiresAt   int64
}

// Part is the Lightning part of an invoice's payment: open, expired, or paid with its preimage.
type Part struct {
	State    string
	Preimage string
}

// The states of a Lightning part.
const (
	Open    = "open"
	Expired = "expired"
	Paid    = "paid"
)

// VerifyPayment checks the Lightning part of an invoice's payment (LUD-21, SPEC.md §22): the
// verify answer names this very invoice (pr, whatever the case of its ASCII letters), settled is
// true and the preimage's SHA-256 is the payment hash. After the invoice's end plus the grace
// period, "not settled" and "Not found" are final (expired); before, any refusal of the service
// keeps it open. Network when the service gives no answer (or another refusal past the grace:
// breez.tips sometimes answers "Internal server error" with a 200), invalid_invoice when its answer
// is about another invoice or its preimage is wrong.
func VerifyPayment(ctx context.Context, client *httpx.Client, inv Invoice, now int64) (Part, error) {
	final := now >= inv.ExpiresAt+proto.GraceSeconds
	a, err := get(ctx, client, inv.VerifyURL)
	if err != nil {
		return Part{}, err
	}
	if a.refused {
		if !final {
			return Part{State: Open}, nil
		}
		if a.reason == proto.NotFoundReason {
			return Part{State: Expired}, nil
		}
		return Part{}, errs.NewNetwork("The payment check was refused: "+a.reason, nil)
	}
	pr, _ := a.json.Get("pr")
	prText, isText := pr.(string)
	if !isText {
		return Part{}, errs.NewInvalidInvoice("The payment check names no invoice (LUD-21 pr)", nil)
	}
	// BOLT11 is case-insensitive (bech32): the same invoice may be written in upper case.
	if text.LowerASCII(prText) != text.LowerASCII(inv.Bolt11) {
		return Part{}, errs.NewInvalidInvoice("The payment check is about another invoice", nil)
	}
	if settled, _ := a.json.Get("settled"); settled != true {
		if final {
			return Part{State: Expired}, nil
		}
		return Part{State: Open}, nil
	}
	preimage, _ := a.json.Get("preimage")
	preimageText, isText := preimage.(string)
	proof := text.LowerASCII(preimageText)
	if !isText || !text.IsHex32(proof) {
		return Part{}, errs.NewInvalidInvoice("Paid without a valid preimage", nil)
	}
	raw, _ := hex.DecodeString(proof)
	hash := sha256.Sum256(raw)
	if hex.EncodeToString(hash[:]) != inv.PaymentHash {
		return Part{}, errs.NewInvalidInvoice("The preimage does not match the payment hash", nil)
	}
	return Part{State: Paid, Preimage: proof}, nil
}

// A query's name-value pairs, as URLSearchParams holds them.
type param struct{ name, value string }

// parseQuery reads a query as application/x-www-form-urlencoded (WHATWG URL): pairs split on "&",
// empty ones dropped, then on the first "=", "+" read as a space, percent-decoded, decoded as
// UTF-8 with U+FFFD for what is not.
func parseQuery(query string) []param {
	var params []param
	for _, pair := range strings.Split(query, "&") {
		if pair == "" {
			continue
		}
		name, value, _ := strings.Cut(pair, "=")
		params = append(params, param{formDecode(name), formDecode(value)})
	}
	return params
}

func formDecode(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHexDigit(s[i+1]) && isHexDigit(s[i+2]) {
			b, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			out = append(out, byte(b))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	if utf8.Valid(out) {
		return string(out)
	}
	repaired := make([]byte, 0, len(out))
	for len(out) > 0 {
		r, size := utf8.DecodeRune(out)
		if r == utf8.RuneError && size <= 1 {
			repaired = append(repaired, "\xef\xbf\xbd"...)
		} else {
			repaired = append(repaired, out[:size]...)
		}
		out = out[size:]
	}
	return string(repaired)
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// setParam is URLSearchParams.set: the first pair of the name takes the value, the others are
// dropped; without one, the pair is appended.
func setParam(params []param, name, value string) []param {
	out := make([]param, 0, len(params)+1)
	set := false
	for _, p := range params {
		if p.name != name {
			out = append(out, p)
		} else if !set {
			out = append(out, param{name, value})
			set = true
		}
	}
	if !set {
		out = append(out, param{name, value})
	}
	return out
}

// serializeQuery writes pairs as application/x-www-form-urlencoded (WHATWG URL): A–Z a–z 0–9 and
// "*-._" as they are, a space as "+", every other byte of the UTF-8 percent-encoded.
func serializeQuery(params []param) string {
	var b strings.Builder
	for i, p := range params {
		if i > 0 {
			b.WriteByte('&')
		}
		formEncode(&b, p.name)
		b.WriteByte('=')
		formEncode(&b, p.value)
	}
	return b.String()
}

func formEncode(b *strings.Builder, s string) {
	const hexUpper = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '*' || c == '-' || c == '.' || c == '_':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(hexUpper[c>>4])
			b.WriteByte(hexUpper[c&15])
		}
	}
}
