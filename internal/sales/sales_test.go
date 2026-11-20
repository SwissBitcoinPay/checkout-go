package sales

import (
	"strconv"
	"testing"

	"github.com/sattleme/checkout-go/internal/bolt11"
	"github.com/sattleme/checkout-go/internal/declaration"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/model"
	"github.com/sattleme/checkout-go/internal/nostr"
	"github.com/sattleme/checkout-go/internal/onchain"
	tv "github.com/sattleme/checkout-go/internal/testvec"
)

// earlier-invoices.json (SPEC.md §21): the invoices of a sale from its declarations and its
// records at registry addresses, those that may still be paid, and what is done of them.
func TestEarlierInvoiceVectors(t *testing.T) {
	file := tv.Load(t, "earlier-invoices.json")
	relays := tv.Int(t, file, "relays")
	const sats = 49_000
	price := declaration.Price{Minor: 49_000, Digits: 0, Currency: "SAT"}
	eventOf := func(e *jsonv.Object) *nostr.Event {
		return &nostr.Event{CreatedAt: tv.Int(t, e, "createdAt"), ID: tv.Str(t, e, "id")}
	}
	cases := tv.Objs(t, file, "cases")
	for _, c := range cases {
		name := tv.Str(t, c, "name")
		list := relays
		if n, ok := jsonv.AsSafeInteger(tv.Get(c, "relays")); ok {
			list = n
		}
		var declarations []*declaration.Opened
		for _, d := range tv.Objs(t, c, "declarations") {
			createdAt, expiresAt, declaredAt := tv.Int(t, d, "createdAt"), tv.Int(t, d, "expiresAt"), tv.Int(t, d, "declaredAt")
			opened := &declaration.Opened{
				Event:   &nostr.Event{CreatedAt: declaredAt, ID: tv.Str(t, d, "paymentHash")},
				Invoice: tv.Str(t, d, "invoice"),
				Decoded: &bolt11.Invoice{PaymentHash: tv.Str(t, d, "paymentHash"), AmountMsat: sats * 1000, HasAmount: true,
					Timestamp: createdAt, ExpirySeconds: expiresAt - createdAt},
				VerifyURL:  "https://example.com/verify",
				Price:      price,
				Note:       "order",
				DeclaredAt: declaredAt,
			}
			if tv.Get(d, "rest") == true {
				opened.Rest = &declaration.RestOf{}
			}
			declarations = append(declarations, opened)
		}
		var records []onchain.SaleLeaseRecord
		for _, r := range tv.Objs(t, c, "records") {
			address := tv.Str(t, r, "address")
			event := eventOf(tv.Obj(t, r, "event"))
			switch {
			case tv.Get(r, "withdrawn") == true:
				records = append(records, onchain.SaleLeaseRecord{Kind: onchain.SaleWithdrawn, Event: event, D: address})
			case tv.Get(r, "newer") == true:
				records = append(records, onchain.SaleLeaseRecord{Kind: onchain.SaleNewer, Event: event, D: address})
			default:
				invoice, _ := tv.Get(r, "invoice").(string)
				terms := onchain.LeaseTerms{Address: address, Sats: sats, Invoice: invoice, CreatedAt: tv.Int(t, r, "createdAt"),
					ExpiresAt: tv.Int(t, r, "expiresAt"), Price: price, Note: "order"}
				if invoice != "" {
					terms.VerifyURL = "https://example.com/verify"
				}
				if confirmed, has := r.Get("confirmed"); has {
					terms.Confirmed, terms.HasConfirmed = confirmed == true, true
				}
				lease := &onchain.OpenedLease{Event: event, D: address, Terms: terms, Inside: jsonv.NewObject()}
				records = append(records, onchain.SaleLeaseRecord{Kind: onchain.SaleLease, Lease: lease, Event: event, D: address})
			}
		}
		declared := InvoicesOf("order", declarations, NewestRecords(records))
		invoices := make([]*model.DeclaredInvoice, len(declared))
		got := make([]any, len(declared))
		for i, d := range declared {
			invoices[i] = d.Invoice
			var hash, address any
			if d.Invoice.PaymentHash != "" {
				hash = d.Invoice.PaymentHash
			}
			if d.Invoice.OnchainAddress != "" {
				address = d.Invoice.OnchainAddress
			}
			got[i] = jsonv.O("paymentHash", hash, "onchainAddress", address, "createdAt", d.Invoice.CreatedAt, "declaredAt", d.Invoice.DeclaredAt)
		}
		expected := tv.Obj(t, c, "expected")
		tv.Equal(t, name+": invoices", got, tv.Get(expected, "invoices"))
		mayBePaid := MayStillBePaid(invoices, tv.Int(t, c, "now"))
		tv.Equal(t, name+": mayBePaid", indices(mayBePaid), tv.Get(expected, "mayBePaid"))
		// Every one that may still be paid is checked, even alone.
		tv.Equal(t, name+": checked", tv.Get(expected, "checked"), indices(mayBePaid))
		if len(mayBePaid) == 0 {
			if _, has := c.Get("statuses"); has || tv.Get(expected, "outcome") != nil {
				t.Errorf("%s: an outcome without invoices that may be paid", name)
			}
			continue
		}
		// What a read on every relay sent: each event's sentBy, all the relays when absent;
		// nothing when fewer than a majority answered it.
		sentBy := map[string]int64{}
		for _, d := range tv.Objs(t, c, "declarations") {
			n, ok := jsonv.AsSafeInteger(tv.Get(d, "sentBy"))
			if !ok {
				n = list
			}
			sentBy[tv.Str(t, d, "paymentHash")] = n
		}
		for _, r := range tv.Objs(t, c, "records") {
			n, ok := jsonv.AsSafeInteger(tv.Get(r, "sentBy"))
			if !ok {
				n = list
			}
			sentBy[tv.Str(t, tv.Obj(t, r, "event"), "id")] = n
		}
		// Which ones a majority holds, as the call decides it.
		var held []bool
		if tv.Get(c, "readFails") != true {
			read := &Records{SentBy: map[string]int{}}
			for id, n := range sentBy {
				read.SentBy[id] = int(n)
			}
			var candidates []Declaration
			for _, index := range mayBePaid {
				candidates = append(candidates, declared[index])
			}
			held = Held(read, candidates, int(list))
		}
		statuses := tv.Objs(t, c, "statuses")
		settled := make([]Checked, len(mayBePaid))
		for i, index := range mayBePaid {
			s := statuses[index]
			if code, failed := s.Get("error"); failed {
				settled[i] = Checked{Err: &errs.Error{Code: errs.Code(code.(string)), Message: code.(string)}}
				continue
			}
			status := &model.PaymentStatus{State: tv.Str(t, s, "state")}
			if received, ok := tv.Get(s, "received").(string); ok {
				status.Received, _ = strconv.ParseInt(received, 10, 64)
			}
			settled[i] = Checked{Status: status}
		}
		outcome := PendingOutcome(settled, held)
		var gotOutcome any
		switch outcome.Kind {
		case OutcomePaid:
			gotOutcome = jsonv.O("orderPaid", mayBePaid[outcome.Index])
		case OutcomeFailed:
			e, _ := errs.Of(settled[outcome.Index].Err)
			gotOutcome = jsonv.O("error", string(e.Code))
		case OutcomeUnread:
			gotOutcome = jsonv.O("error", "network")
		default:
			gotOutcome = jsonv.O("handOut", mayBePaid[outcome.Index])
		}
		tv.Equal(t, name+": outcome", gotOutcome, tv.Get(expected, "outcome"))
	}
	t.Logf("%d cases", len(cases))
}

func indices(list []int) []any {
	out := make([]any, len(list))
	for i, n := range list {
		out[i] = n
	}
	return out
}

func TestTheRestKeyOfAnInvoice(t *testing.T) {
	if got := RestKeyOf("bc1pexample", 1_791_379_180); got != "bc1pexample/1791379180" {
		t.Errorf("rest key %s", got)
	}
	if FiatOf(declaration.Price{Minor: 49_000, Currency: "SAT"}) != nil {
		t.Error("a sale in sats has a price in a currency")
	}
	if got := *FiatOf(declaration.Price{Minor: 2450, Digits: 2, Currency: "CHF"}); got != (model.Fiat{Amount: "24.50", Currency: "CHF", Minor: 2450, Digits: 2}) {
		t.Errorf("fiat %+v", got)
	}
	if got := FiatOf(declaration.Price{Minor: 5, Digits: 3, Currency: "KWD"}).Amount; got != "0.005" {
		t.Errorf("amount %s", got)
	}
}
