package checkout_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	checkout "github.com/sattleme/checkout-go"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/testkit"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// paymentSetup is a merchant, its client (relay requests of 10 s), and the invoice of an order
// of 2 100 sats lasting 600 s.
func paymentSetup(t *testing.T, opts ...checkout.Option) (*testkit.Merchant, *checkout.Client, *checkout.Invoice) {
	t.Helper()
	m := testkit.NewMerchant()
	m.Start(nil, nil)
	t.Cleanup(m.Stop)
	m.PublishChannel(m.ChannelValue(100), 100)
	c := clientOf(t, m, append([]checkout.Option{checkout.WithRelayTimeout(10 * time.Second)}, opts...)...)
	invoice, err := c.CreateInvoice(ctx, checkout.Order{Ref: "P-1", Sats: 2100, DurationSeconds: 600})
	must(t, err)
	return m, c, invoice
}

// verifyAnswers replaces breez.tips's verify answer.
func verifyAnswers(m *testkit.Merchant, a testkit.Answer) {
	m.SetOverride(answerAt("/verify/", a))
}

func TestGetStatusIsOpenThenPaidWithThePreimage(t *testing.T) {
	m, c, invoice := paymentSetup(t)
	status, err := c.GetStatus(ctx, invoice)
	must(t, err)
	same(t, "open", jsonOf(t, status), jsonv.O("state", "open", "expiresAt", invoice.ExpiresAt, "received", "0", "rest", nil))
	m.PayInvoice(invoice.PaymentHash)
	status, err = c.GetStatus(ctx, invoice)
	must(t, err)
	same(t, "paid", jsonOf(t, status), jsonv.O("state", "paid", "method", "lightning", "preimage", m.Invoice(invoice.PaymentHash).Preimage))
	// A look, without state: from the stored invoice, as JSON.
	data, err := json.Marshal(invoice)
	must(t, err)
	var stored checkout.Invoice
	must(t, json.Unmarshal(data, &stored))
	status, err = c.GetStatus(ctx, stored)
	must(t, err)
	if status.State != checkout.StatePaid {
		t.Errorf("state %s", status.State)
	}
	// Its status, stored and read back.
	data, err = json.Marshal(status)
	must(t, err)
	var back checkout.PaymentStatus
	must(t, json.Unmarshal(data, &back))
	same(t, "read back", jsonOf(t, back), jsonOf(t, status))
}

func TestTheInvoiceIsReadWhateverItsCaseAndAnUpperCasePreimage(t *testing.T) {
	m, c, invoice := paymentSetup(t)
	made := m.Invoice(invoice.PaymentHash)
	verifyAnswers(m, testkit.JSON(jsonv.O("status", "OK", "settled", true, "preimage", strings.ToUpper(made.Preimage), "pr", strings.ToUpper(made.PR))))
	status, err := c.GetStatus(ctx, invoice)
	must(t, err)
	same(t, "paid", jsonOf(t, status), jsonv.O("state", "paid", "method", "lightning", "preimage", made.Preimage))
}

func TestAnAnswerAboutAnotherInvoiceOrAWrongPreimageIsRefused(t *testing.T) {
	m, c, invoice := paymentSetup(t)
	made := m.Invoice(invoice.PaymentHash)
	other, err := c.CreateInvoice(ctx, checkout.Order{Ref: "P-2", Sats: 2100, DurationSeconds: 600})
	must(t, err)
	for _, answer := range []jsonv.Members{
		jsonv.O("status", "OK", "settled", true, "preimage", made.Preimage, "pr", other.Bolt11),
		jsonv.O("status", "OK", "settled", true, "preimage", strings.Repeat("11", 32), "pr", made.PR),
		jsonv.O("status", "OK", "settled", true, "preimage", nil, "pr", made.PR),
		jsonv.O("status", "OK", "settled", true, "preimage", made.Preimage),
	} {
		verifyAnswers(m, testkit.JSON(answer))
		_, err := c.GetStatus(ctx, invoice)
		wantCode(t, err, checkout.CodeInvalidInvoice)
	}
}

func TestExpired120SecondsAfterTheInvoicesEndNotBefore(t *testing.T) {
	m, c, invoice := paymentSetup(t)
	m.SetNow(invoice.ExpiresAt + 119)
	status, err := c.GetStatus(ctx, invoice)
	must(t, err)
	if status.State != checkout.StateOpen {
		t.Errorf("state %s", status.State)
	}
	m.SetNow(invoice.ExpiresAt + 120)
	status, err = c.GetStatus(ctx, invoice)
	must(t, err)
	same(t, "expired", jsonOf(t, status), jsonv.O("state", "expired", "received", "0"))
}

func TestExpiredOnNotFoundAfterTheGraceOnly(t *testing.T) {
	m, c, invoice := paymentSetup(t)
	verifyAnswers(m, testkit.JSON(jsonv.O("status", "ERROR", "reason", "Not found")))
	m.SetNow(invoice.ExpiresAt + 119)
	status, err := c.GetStatus(ctx, invoice)
	must(t, err)
	if status.State != checkout.StateOpen {
		t.Errorf("state %s", status.State)
	}
	m.SetNow(invoice.ExpiresAt + 120)
	status, err = c.GetStatus(ctx, invoice)
	must(t, err)
	same(t, "expired", jsonOf(t, status), jsonv.O("state", "expired", "received", "0"))
	verifyAnswers(m, testkit.Status(404, jsonv.O("status", "ERROR", "reason", "Not found")))
	status, err = c.GetStatus(ctx, invoice)
	must(t, err)
	same(t, "expired on a 404", jsonOf(t, status), jsonv.O("state", "expired", "received", "0"))
}

func TestAnyOtherErrorKeepsTheInvoiceOpenBeforeTheGraceAndIsRetriedAfter(t *testing.T) {
	m, c, invoice := paymentSetup(t)
	verifyAnswers(m, testkit.JSON(jsonv.O("status", "ERROR", "reason", "Internal server error")))
	status, err := c.GetStatus(ctx, invoice)
	must(t, err)
	if status.State != checkout.StateOpen {
		t.Errorf("state %s", status.State)
	}
	m.SetNow(invoice.ExpiresAt + 600)
	_, err = c.GetStatus(ctx, invoice)
	wantCode(t, err, checkout.CodeNetwork)
}

func TestNetworkRetryableWhenBreezTipsDoesNotAnswer(t *testing.T) {
	m, c, invoice := paymentSetup(t)
	for _, status := range []int{500, 502, 429} {
		verifyAnswers(m, testkit.Status(status, jsonv.O("status", "ERROR", "reason", "Not found")))
		_, err := c.GetStatus(ctx, invoice)
		if e := wantCode(t, err, checkout.CodeNetwork); !e.Retryable() {
			t.Error("network is retryable")
		}
	}
	verifyAnswers(m, testkit.Answer{Err: errors.New("fetch failed")})
	_, err := c.GetStatus(ctx, invoice)
	wantCode(t, err, checkout.CodeNetwork)
	verifyAnswers(m, testkit.Text(200, "<html>"))
	_, err = c.GetStatus(ctx, invoice)
	wantCode(t, err, checkout.CodeNetwork)
}

func TestAStoredInvoiceThatIsNotWhatCreateInvoiceGaveIsRefused(t *testing.T) {
	_, c, invoice := paymentSetup(t)
	changes := []func(i *checkout.Invoice){
		func(i *checkout.Invoice) { i.PaymentHash = strings.Repeat("00", 32) },
		func(i *checkout.Invoice) { i.ExpiresAt += 3600 },
		func(i *checkout.Invoice) { i.VerifyURL = "http://breez.tips/x" },
		func(i *checkout.Invoice) { i.Bolt11 = "lnbc1" },
		func(i *checkout.Invoice) { i.Sats++ },
		func(i *checkout.Invoice) { i.OnchainAddress = "bc1qnotadepositaddress" },
	}
	for _, change := range changes {
		changed := *invoice
		change(&changed)
		_, err := c.GetStatus(ctx, changed)
		wantCode(t, err, checkout.CodeInvalidOrder, "invoice")
	}
	var none *checkout.Invoice
	_, err := c.GetStatus(ctx, none)
	wantCode(t, err, checkout.CodeInvalidOrder, "invoice")
	_, err = c.GetStatus(ctx, nil)
	wantCode(t, err, checkout.CodeInvalidOrder, "invoice")
}

// SPEC.md §22: a stored invoice may lack its onchainAddress alone. Without its bolt11, paymentHash
// or verifyUrl member (absent is not null), or with an expiresAt that is no integer (1e400 is
// Infinity), it is no invoice: invalid_order (invoice), however it is read.
func TestAStoredInvoiceLackingAMemberIsRefused(t *testing.T) {
	_, c, invoice := paymentSetup(t)
	data, err := json.Marshal(invoice)
	must(t, err)
	without := func(name string) []byte {
		v, err := jsonv.Parse(data)
		must(t, err)
		kept := jsonv.NewObject()
		for _, k := range v.(*jsonv.Object).Keys() {
			if k != name {
				kept.Set(k, fieldOf(v, k))
			}
		}
		return jsonv.Marshal(kept)
	}
	// Without its address (nor its payment URI): Lightning only, checked.
	var lightning checkout.Invoice
	must(t, json.Unmarshal(without("onchainAddress"), &lightning))
	if _, err := c.GetStatus(ctx, lightning); err != nil {
		t.Errorf("without onchainAddress: %v", err)
	}
	bad := map[string][]byte{
		"bolt11":      without("bolt11"),
		"paymentHash": without("paymentHash"),
		"verifyUrl":   without("verifyUrl"),
		"sats":        without("sats"),
		"expiresAt":   without("expiresAt"),
		"1e400":       []byte(strings.Replace(string(data), `"expiresAt":`+itoa(invoice.ExpiresAt), `"expiresAt":1e400`, 1)),
	}
	for name, text := range bad {
		if name == "1e400" && !strings.Contains(string(text), "1e400") {
			t.Fatal("expiresAt not replaced")
		}
		var asInvoice checkout.Invoice
		wantCode(t, json.Unmarshal(text, &asInvoice), checkout.CodeInvalidOrder, "invoice")
		var toCheck checkout.InvoiceToCheck
		wantCode(t, json.Unmarshal(text, &toCheck), checkout.CodeInvalidOrder, "invoice")
		var declared checkout.DeclaredInvoice
		wantCode(t, json.Unmarshal(text, &declared), checkout.CodeInvalidOrder, "invoice")
	}
}

// encoding/json's convention: an Unmarshaler takes null as a no-op. A stored row whose invoice or
// status is null (an order without one yet) is read, each value of the data model left as it is;
// a payment check then refuses the zero Invoice, before anything is asked. Anything else that is
// not one is still refused.
func TestANullValueOfTheDataModelIsReadAsNothing(t *testing.T) {
	_, c, invoice := paymentSetup(t)
	var row struct {
		Ref      string                   `json:"ref"`
		Invoice  checkout.Invoice         `json:"invoice"`
		Declared checkout.DeclaredInvoice `json:"declared"`
		ToCheck  checkout.InvoiceToCheck  `json:"toCheck"`
		Status   checkout.PaymentStatus   `json:"status"`
		Fiat     checkout.Fiat            `json:"fiat"`
		Rate     checkout.RateInfo        `json:"rate"`
		Rest     checkout.Rest            `json:"rest"`
		Merchant checkout.Merchant        `json:"merchant"`
	}
	text := `{"ref":"ORD-1","invoice":null,"declared":null,"toCheck":null,"status":null,"fiat":null,"rate":null,"rest":null,"merchant":null}`
	must(t, json.Unmarshal([]byte(text), &row))
	if row.Ref != "ORD-1" || !reflect.ValueOf(row.Invoice).IsZero() || !reflect.ValueOf(row.Declared).IsZero() ||
		!reflect.ValueOf(row.ToCheck).IsZero() || !reflect.ValueOf(row.Status).IsZero() || !reflect.ValueOf(row.Fiat).IsZero() ||
		!reflect.ValueOf(row.Rate).IsZero() || !reflect.ValueOf(row.Rest).IsZero() || !reflect.ValueOf(row.Merchant).IsZero() {
		t.Errorf("%+v", row)
	}
	// Null leaves a value as it is.
	kept := *invoice
	must(t, json.Unmarshal([]byte(" null "), &kept))
	same(t, "kept", jsonOf(t, kept), jsonOf(t, invoice))
	_, err := c.GetStatus(ctx, row.Invoice)
	wantCode(t, err, checkout.CodeInvalidOrder, "invoice")
	for _, text := range []string{`"null"`, `0`, `[]`, `false`, `{}`} {
		var asInvoice checkout.Invoice
		wantCode(t, json.Unmarshal([]byte(text), &asInvoice), checkout.CodeInvalidOrder, "invoice")
		var status checkout.PaymentStatus
		if err := json.Unmarshal([]byte(text), &status); err == nil {
			t.Errorf("a status of %s", text)
		}
	}
}

// unreachable is an HTTP transport and a WebSocket dialer that fail whatever they are asked, and
// count it.
type unreachable struct{ asked atomic.Int64 }

func (u *unreachable) RoundTrip(*http.Request) (*http.Response, error) {
	u.asked.Add(1)
	return nil, errors.New("unreachable")
}

func (u *unreachable) Dial(context.Context, string, http.Header) (checkout.WebSocketConn, error) {
	u.asked.Add(1)
	return nil, errors.New("unreachable")
}

// payments.json, stored: invoices as a shop stores them, checked before a payment check asks
// anything (SPEC.md §22). Each is read back as each type the package reads a stored invoice as,
// then checked by GetStatus: a valid one goes on to ask (here of services that fail: network), one
// that is not an invoice is refused, invalid_order (invoice), before anything is asked.
func TestStoredInvoiceVectors(t *testing.T) {
	m := newMerchant(t)
	cases := tv.Objs(t, tv.Load(t, "payments.json"), "stored")
	for _, c := range cases {
		name := tv.Str(t, c, "name")
		data := jsonv.Marshal(tv.Get(c, "invoice"))
		reads := []struct {
			as   string
			read func() (checkout.Checkable, error)
		}{
			{"Invoice", func() (checkout.Checkable, error) {
				var v checkout.Invoice
				err := json.Unmarshal(data, &v)
				return &v, err
			}},
			{"InvoiceToCheck", func() (checkout.Checkable, error) {
				var v checkout.InvoiceToCheck
				err := json.Unmarshal(data, &v)
				return v, err
			}},
			{"DeclaredInvoice", func() (checkout.Checkable, error) {
				var v checkout.DeclaredInvoice
				err := json.Unmarshal(data, &v)
				return v, err
			}},
		}
		for _, r := range reads {
			services := &unreachable{}
			client, err := checkout.New(m.Key(), checkout.WithHTTPTransport(services), checkout.WithWebSocketDialer(services),
				checkout.WithRelayTimeout(time.Second), checkout.WithLogger(quiet()))
			must(t, err)
			invoice, err := r.read()
			if err == nil {
				_, err = client.GetStatus(ctx, invoice)
			}
			got := jsonv.O("valid", true)
			var e *checkout.Error
			switch {
			case errors.As(err, &e) && e.Code == checkout.CodeInvalidOrder:
				got = jsonv.O("error", string(e.Code), "param", e.Param)
				if services.asked.Load() != 0 {
					t.Errorf("%s, as %s: refused once asked", name, r.as)
				}
			case !errors.As(err, &e) || e.Code != checkout.CodeNetwork || services.asked.Load() == 0:
				t.Errorf("%s, as %s: %v, %d asked", name, r.as, err, services.asked.Load())
			}
			tv.Equal(t, name+", as "+r.as, got, tv.Get(c, "expected"))
		}
	}
	t.Logf("%d cases", len(cases))
}

// heedless is an HTTP transport that ignores its requests' contexts: each request waits for
// release, then gets what the merchant's fakes answer.
type heedless struct {
	m       *testkit.Merchant
	release chan struct{}
	asked   chan struct{}
}

func (h *heedless) RoundTrip(req *http.Request) (*http.Response, error) {
	h.asked <- struct{}{}
	<-h.release
	return h.m.HTTP.RoundTrip(req.WithContext(context.Background()))
}

// SPEC.md §4: a cancelled call fails with the platform's cancellation, even when a step that does
// not heed it (here an HTTP transport of the integrator's) delayed it and then answered: never
// with what that step gave.
func TestACancelledCallFailsWithItsCancellationWhateverABlockingStepAnswers(t *testing.T) {
	m, _, invoice := paymentSetup(t)
	for name, call := range map[string]func(c *checkout.Client, ctx context.Context) (any, error){
		// Its last step the Lightning address's pay request: the relays read meanwhile.
		"Merchant": func(c *checkout.Client, ctx context.Context) (any, error) { return c.Merchant(ctx) },
		// Its one step the verify URL.
		"GetStatus": func(c *checkout.Client, ctx context.Context) (any, error) { return c.GetStatus(ctx, invoice) },
	} {
		t.Run(name, func(t *testing.T) {
			h := &heedless{m: m, release: make(chan struct{}), asked: make(chan struct{}, 10)}
			c := clientOf(t, m, checkout.WithHTTPTransport(h))
			ctx, cancel := context.WithCancel(ctx)
			type outcome struct {
				v   any
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				v, err := call(c, ctx)
				done <- outcome{v, err}
			}()
			<-h.asked
			// What else the call asks (the relays) answers meanwhile; then it is cancelled, and the
			// request answers, valid.
			time.Sleep(200 * time.Millisecond)
			cancel()
			close(h.release)
			got := <-done
			if !errors.Is(got.err, context.Canceled) {
				t.Errorf("got %+v, %v", got.v, got.err)
			}
			var e *checkout.Error
			if errors.As(got.err, &e) {
				t.Error("a cancellation is no error of the package")
			}
		})
	}
}

func TestWaitForPaymentChecksUntilPaidNetworkErrorsRetried(t *testing.T) {
	m, c, invoice := paymentSetup(t)
	var mu sync.Mutex
	calls := 0
	m.SetOverride(func(req *http.Request) (testkit.Answer, bool) {
		if !strings.Contains(req.URL.Path, "/verify/") {
			return testkit.Answer{}, false
		}
		mu.Lock()
		defer mu.Unlock()
		calls++
		switch calls {
		case 2:
			return testkit.Answer{Err: errors.New("fetch failed")}, true
		case 3:
			m.PayInvoice(invoice.PaymentHash)
		}
		return testkit.Answer{}, false
	})
	var seen []checkout.State
	final, err := c.WaitForPayment(ctx, invoice, checkout.WithInterval(250*time.Millisecond),
		checkout.WithOnStatus(func(s *checkout.PaymentStatus) { seen = append(seen, s.State) }))
	must(t, err)
	if final.State != checkout.StatePaid {
		t.Errorf("state %s", final.State)
	}
	same(t, "seen", seen, []checkout.State{checkout.StateOpen, checkout.StatePaid})
}

func TestAFailureEachCheckMeetsAgainIsLoggedOnceAndAgainOnlyAfterItStopped(t *testing.T) {
	logger := quiet()
	m, c, invoice := paymentSetup(t, checkout.WithLogger(logger))
	// Down for 3 checks, up for 1, down for 2 again, then paid.
	down := []bool{true, true, true, false, true, true}
	var mu sync.Mutex
	calls := 0
	m.SetOverride(func(req *http.Request) (testkit.Answer, bool) {
		if !strings.Contains(req.URL.Path, "/verify/") {
			return testkit.Answer{}, false
		}
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls > len(down) {
			m.PayInvoice(invoice.PaymentHash)
		}
		if calls <= len(down) && down[calls-1] {
			return testkit.Text(503, "down"), true
		}
		return testkit.Answer{}, false
	})
	final, err := c.WaitForPayment(ctx, invoice, checkout.WithInterval(250*time.Millisecond))
	must(t, err)
	if final.State != checkout.StatePaid || calls != len(down)+1 {
		t.Errorf("state %s after %d checks", final.State, calls)
	}
	warning := "github.com/sattleme/checkout-go: the payment check failed (breez.tips answered HTTP 503 without an LNURL answer); checked again"
	same(t, "warnings", logger.Messages(), []string{warning, warning})
}

func TestWaitForPaymentStopsAtExpired(t *testing.T) {
	m, c, invoice := paymentSetup(t)
	m.SetNow(invoice.ExpiresAt + 120)
	final, err := c.WaitForPayment(ctx, invoice, checkout.WithInterval(250*time.Millisecond))
	must(t, err)
	same(t, "expired", jsonOf(t, final), jsonv.O("state", "expired", "received", "0"))
}

func TestWaitForPaymentStopsWhenItsContextEnds(t *testing.T) {
	_, c, invoice := paymentSetup(t)
	call, cancel := context.WithCancel(ctx)
	time.AfterFunc(300*time.Millisecond, cancel)
	_, err := c.WaitForPayment(call, invoice, checkout.WithInterval(250*time.Millisecond))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestWaitForPaymentStopsOnAnAnswerThatIsNotAboutThisInvoice(t *testing.T) {
	m, c, invoice := paymentSetup(t)
	verifyAnswers(m, testkit.JSON(jsonv.O("status", "OK", "settled", false, "pr", "lnbc1other")))
	_, err := c.WaitForPayment(ctx, invoice, checkout.WithInterval(250*time.Millisecond))
	wantCode(t, err, checkout.CodeInvalidInvoice)
}

func TestWaitForPaymentRefusesInvalidOptionsBeforeAnyRequest(t *testing.T) {
	m, c, invoice := paymentSetup(t)
	before := len(m.HTTP.Calls())
	for _, opts := range [][]checkout.WaitOption{
		{checkout.WithInterval(249 * time.Millisecond)},
		{checkout.WithInterval(60*time.Second + time.Millisecond)},
		{checkout.WithInterval(1500 * time.Microsecond)},
		{checkout.WithInterval(0)},
	} {
		_, err := c.WaitForPayment(ctx, invoice, opts...)
		wantCode(t, err, checkout.CodeInvalidOption, "intervalMs")
	}
	_, err := c.WaitForPayment(ctx, invoice, checkout.WithOnStatus(nil))
	wantCode(t, err, checkout.CodeInvalidOption, "onStatus")
	_, err = c.WaitForPayment(ctx, invoice, nil)
	wantCode(t, err, checkout.CodeInvalidOption, "options")
	if after := len(m.HTTP.Calls()); after != before {
		t.Errorf("%d requests", after-before)
	}
}
