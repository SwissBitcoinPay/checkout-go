package checkout

import (
	"errors"
	"regexp"
	"strconv"

	"github.com/sattleme/checkout-go/internal/jsonv"
)

// Reading the data model back (UnmarshalJSON): JSON is read as every implementation reads it
// (SPEC.md §2: of members with one name the last counts, a number is its value whatever its
// spelling, so 1791378900.0 is 1791378900), each field of the type SPEC.md §3 gives it. An
// absent value is "" (a string) or nil; a string that is "" is no value of the data model, and is
// refused where null is allowed (an invoice's bolt11 "" is no invoice). JSON's null itself is
// read as nothing, as encoding/json's Unmarshalers do: the value is left as it is (a stored row
// whose invoice is null, an order without one yet); a payment check refuses a zero Invoice.

// isNull tells whether data is JSON's null.
func isNull(data []byte) bool {
	v, err := jsonv.Parse(data)
	return err == nil && v == nil
}

var digitsPattern = regexp.MustCompile(`^[0-9]{1,18}$`)

// storedSatsPattern is the sats of a stored invoice, as every implementation reads them.
var storedSatsPattern = regexp.MustCompile(`^[0-9]{1,16}$`)

// object reads the members of a JSON object, recording the first one that is not of its type.
type object struct {
	o   *jsonv.Object
	bad bool
}

func readObject(data []byte) (*object, bool) {
	v, err := jsonv.Parse(data)
	if err != nil {
		return nil, false
	}
	return objectOf(v)
}

func objectOf(v any) (*object, bool) {
	o, ok := jsonv.AsObject(v)
	if !ok {
		return nil, false
	}
	return &object{o: o}, true
}

// text is a string member; absent: "" unless required.
func (r *object) text(name string, required bool) string {
	v, ok := r.o.Get(name)
	if !ok {
		r.bad = r.bad || required
		return ""
	}
	s, isText := v.(string)
	if !isText {
		r.bad = true
	}
	return s
}

// nullable is a member that is null or a string other than ""; absent: "" unless required.
func (r *object) nullable(name string, required bool) string {
	v, ok := r.o.Get(name)
	if !ok {
		r.bad = r.bad || required
		return ""
	}
	if v == nil {
		return ""
	}
	s, isText := v.(string)
	if !isText || s == "" {
		r.bad = true
	}
	return s
}

// amount is a string of digits that fits pattern.
func (r *object) amount(name string, pattern *regexp.Regexp) int64 {
	v, _ := r.o.Get(name)
	s, isText := v.(string)
	if !isText || !pattern.MatchString(s) {
		r.bad = true
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		r.bad = true
	}
	return n
}

// integer is a safe integer member; absent: 0 unless required.
func (r *object) integer(name string, required bool) int64 {
	v, ok := r.o.Get(name)
	if !ok {
		r.bad = r.bad || required
		return 0
	}
	n, isInteger := jsonv.AsSafeInteger(v)
	if !isInteger {
		r.bad = true
	}
	return n
}

// texts is an array of strings.
func (r *object) texts(name string) []string {
	v, _ := r.o.Get(name)
	items, isArray := v.([]any)
	if !isArray {
		r.bad = true
		return nil
	}
	out := make([]string, len(items))
	for i, item := range items {
		s, isText := item.(string)
		if !isText {
			r.bad = true
		}
		out[i] = s
	}
	return out
}

// fiat is a Fiat member, or null; absent: nil.
func (r *object) fiat(name string) *Fiat {
	v, ok := r.o.Get(name)
	if !ok || v == nil {
		return nil
	}
	f, ok := objectOf(v)
	if !ok {
		r.bad = true
		return nil
	}
	fiat := f.readFiat()
	r.bad = r.bad || f.bad
	return fiat
}

func (r *object) readFiat() *Fiat {
	fiat := &Fiat{Amount: r.text("amount", true), Currency: r.text("currency", true)}
	fiat.Minor = r.amount("minor", storedSatsPattern)
	digits := r.integer("digits", true)
	if digits < 0 || digits > 8 {
		r.bad = true
	}
	fiat.Digits = int(digits)
	return fiat
}

// rate is a RateInfo member, or null; absent: nil.
func (r *object) rate(name string) *RateInfo {
	v, ok := r.o.Get(name)
	if !ok || v == nil {
		return nil
	}
	o, ok := objectOf(v)
	if !ok {
		r.bad = true
		return nil
	}
	info := o.readRate()
	r.bad = r.bad || o.bad
	return info
}

func (r *object) readRate() *RateInfo {
	info := &RateInfo{Source: r.text("source", true), Currency: r.text("currency", true), Price: r.text("price", true)}
	info.FetchedAt = r.integer("fetchedAt", true)
	if t, ok := r.o.Get("time"); !ok {
		r.bad = true
	} else if t != nil {
		n := r.integer("time", true)
		info.Time = &n
	}
	return info
}

// errNotInvoice is the error of an invoice that is not one: as a payment check refuses it.
func errNotInvoice() error {
	return &Error{Code: CodeInvalidOrder, Param: "invoice", Message: "invoice: not an invoice made by createInvoice"}
}

// readToCheck reads what a payment check needs, as every implementation reads a stored invoice
// (SPEC.md §22): bolt11, paymentHash and verifyUrl null or strings, sats a string of digits,
// expiresAt an integer; onchainAddress absent, null or a string; ref when it is a string.
func (r *object) readToCheck() InvoiceToCheck {
	c := InvoiceToCheck{
		Bolt11:         r.nullable("bolt11", true),
		PaymentHash:    r.nullable("paymentHash", true),
		VerifyURL:      r.nullable("verifyUrl", true),
		OnchainAddress: r.nullable("onchainAddress", false),
		Sats:           r.amount("sats", storedSatsPattern),
		ExpiresAt:      r.integer("expiresAt", true),
	}
	// Needed with an address only: the payment check refuses an address without one.
	if ref, ok := r.o.Get("ref"); ok {
		c.Ref, _ = ref.(string)
	}
	return c
}

// UnmarshalJSON reads what a payment check needs of a stored invoice (an Invoice, a
// DeclaredInvoice or an InvoiceToCheck, of any implementation): invalid_order (invoice) when it
// is not one; null leaves it as it is.
func (c *InvoiceToCheck) UnmarshalJSON(data []byte) error {
	if isNull(data) {
		return nil
	}
	r, ok := readObject(data)
	if !ok {
		return errNotInvoice()
	}
	read := r.readToCheck()
	if r.bad {
		return errNotInvoice()
	}
	*c = read
	return nil
}

// UnmarshalJSON reads an invoice stored as SPEC.md §3 writes it (by this package or another
// implementation): invalid_order (invoice) when it is not one; null leaves it as it is.
func (i *Invoice) UnmarshalJSON(data []byte) error {
	if isNull(data) {
		return nil
	}
	r, ok := readObject(data)
	if !ok {
		return errNotInvoice()
	}
	c := r.readToCheck()
	read := Invoice{
		Ref:            c.Ref,
		Bolt11:         c.Bolt11,
		PaymentHash:    c.PaymentHash,
		VerifyURL:      c.VerifyURL,
		OnchainAddress: c.OnchainAddress,
		PaymentURI:     r.text("paymentUri", false),
		Sats:           c.Sats,
		Fiat:           r.fiat("fiat"),
		Rate:           r.rate("rate"),
		Note:           r.text("note", false),
		CreatedAt:      r.integer("createdAt", false),
		ExpiresAt:      c.ExpiresAt,
		CheckoutURL:    r.nullable("checkoutUrl", false),
		RedirectURL:    r.nullable("redirectUrl", false),
	}
	if r.bad {
		return errNotInvoice()
	}
	*i = read
	return nil
}

// UnmarshalJSON reads a declared invoice as SPEC.md §3 writes it: invalid_order (invoice) when it
// is not one; null leaves it as it is.
func (d *DeclaredInvoice) UnmarshalJSON(data []byte) error {
	if isNull(data) {
		return nil
	}
	r, ok := readObject(data)
	if !ok {
		return errNotInvoice()
	}
	c := r.readToCheck()
	read := DeclaredInvoice{
		Ref:            c.Ref,
		Bolt11:         c.Bolt11,
		PaymentHash:    c.PaymentHash,
		VerifyURL:      c.VerifyURL,
		OnchainAddress: c.OnchainAddress,
		PaymentURI:     r.text("paymentUri", false),
		Sats:           c.Sats,
		Fiat:           r.fiat("fiat"),
		Note:           r.text("note", false),
		CreatedAt:      r.integer("createdAt", false),
		ExpiresAt:      c.ExpiresAt,
		DeclaredAt:     r.integer("declaredAt", false),
	}
	if r.bad {
		return errNotInvoice()
	}
	*d = read
	return nil
}

func notJSONOf(what string) error {
	return errors.New("checkout: not the JSON of a " + what + " (SPEC.md §3)")
}

// UnmarshalJSON reads a price as SPEC.md §3 writes it.
func (f *Fiat) UnmarshalJSON(data []byte) error {
	if isNull(data) {
		return nil
	}
	r, ok := readObject(data)
	if !ok {
		return notJSONOf("Fiat")
	}
	read := r.readFiat()
	if r.bad {
		return notJSONOf("Fiat")
	}
	*f = *read
	return nil
}

// UnmarshalJSON reads a rate as SPEC.md §3 writes it.
func (ri *RateInfo) UnmarshalJSON(data []byte) error {
	if isNull(data) {
		return nil
	}
	r, ok := readObject(data)
	if !ok {
		return notJSONOf("RateInfo")
	}
	read := r.readRate()
	if r.bad {
		return notJSONOf("RateInfo")
	}
	*ri = *read
	return nil
}

func (r *object) readRest() *Rest {
	return &Rest{Sats: r.amount("sats", digitsPattern), Bolt11: r.nullable("bolt11", true), PaymentURI: r.nullable("paymentUri", true)}
}

// UnmarshalJSON reads a rest as SPEC.md §3 writes it.
func (rest *Rest) UnmarshalJSON(data []byte) error {
	if isNull(data) {
		return nil
	}
	r, ok := readObject(data)
	if !ok {
		return notJSONOf("Rest")
	}
	read := r.readRest()
	if r.bad {
		return notJSONOf("Rest")
	}
	*rest = *read
	return nil
}

// UnmarshalJSON reads a status as SPEC.md §3 writes it: the fields of its state.
func (s *PaymentStatus) UnmarshalJSON(data []byte) error {
	if isNull(data) {
		return nil
	}
	r, ok := readObject(data)
	if !ok {
		return notJSONOf("PaymentStatus")
	}
	read := PaymentStatus{State: State(r.text("state", true))}
	switch read.State {
	case StateOpen:
		read.ExpiresAt = r.integer("expiresAt", true)
		read.Received = r.amount("received", digitsPattern)
		if v, ok := r.o.Get("rest"); !ok {
			r.bad = true
		} else if v != nil {
			if rest, ok := objectOf(v); ok {
				read.Rest = rest.readRest()
				r.bad = r.bad || rest.bad
			} else {
				r.bad = true
			}
		}
	case StateUnconfirmed:
		read.Received = r.amount("received", digitsPattern)
		confirmations := r.integer("confirmations", true)
		if confirmations < 0 || confirmations > 1<<31-1 {
			r.bad = true
		}
		read.Confirmations = int(confirmations)
		read.Txids = r.texts("txids")
	case StatePaid:
		read.Method = Method(r.text("method", true))
		switch read.Method {
		case MethodLightning:
			read.Preimage = r.text("preimage", true)
		case MethodOnchain:
			read.Received = r.amount("received", digitsPattern)
			read.Txids = r.texts("txids")
		case MethodMixed:
			read.Received = r.amount("received", digitsPattern)
			read.Txids = r.texts("txids")
			read.Preimages = r.texts("preimages")
		default:
			r.bad = true
		}
	case StateExpired:
		read.Received = r.amount("received", digitsPattern)
	default:
		r.bad = true
	}
	if r.bad {
		return notJSONOf("PaymentStatus")
	}
	*s = read
	return nil
}

// UnmarshalJSON reads a merchant as SPEC.md §3 writes it.
func (m *Merchant) UnmarshalJSON(data []byte) error {
	if isNull(data) {
		return nil
	}
	r, ok := readObject(data)
	if !ok {
		return notJSONOf("Merchant")
	}
	read := Merchant{BusinessName: r.text("businessName", true), MinSats: r.amount("minSats", digitsPattern), MaxSats: r.amount("maxSats", digitsPattern)}
	addresses := r.integer("onchainAddresses", true)
	if addresses < 0 || addresses > 1<<31-1 {
		r.bad = true
	}
	read.OnchainAddresses = int(addresses)
	if r.bad {
		return notJSONOf("Merchant")
	}
	*m = read
	return nil
}
