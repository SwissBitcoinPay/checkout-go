package testkit

import (
	"strconv"

	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/model"
)

// StatusJSON is a status as SPEC.md §3 writes it (as the public package does), for the vectors
// of the internal packages.
func StatusJSON(s *model.PaymentStatus) jsonv.Members {
	digits := func(n int64) string { return strconv.FormatInt(n, 10) }
	list := func(items []string) []string {
		if items == nil {
			return []string{}
		}
		return items
	}
	switch {
	case s.State == model.Open:
		var rest any
		if s.Rest != nil {
			var bolt11, uri any
			if s.Rest.Bolt11 != "" {
				bolt11 = s.Rest.Bolt11
			}
			if s.Rest.PaymentURI != "" {
				uri = s.Rest.PaymentURI
			}
			rest = jsonv.O("sats", digits(s.Rest.Sats), "bolt11", bolt11, "paymentUri", uri)
		}
		return jsonv.O("state", s.State, "expiresAt", s.ExpiresAt, "received", digits(s.Received), "rest", rest)
	case s.State == model.Unconfirmed:
		return jsonv.O("state", s.State, "received", digits(s.Received), "confirmations", s.Confirmations, "txids", list(s.Txids))
	case s.State == model.Paid && s.Method == model.Lightning:
		return jsonv.O("state", s.State, "method", s.Method, "preimage", s.Preimage)
	case s.State == model.Paid && s.Method == model.Mixed:
		return jsonv.O("state", s.State, "method", s.Method, "received", digits(s.Received), "txids", list(s.Txids), "preimages", list(s.Preimages))
	case s.State == model.Paid:
		return jsonv.O("state", s.State, "method", s.Method, "received", digits(s.Received), "txids", list(s.Txids))
	}
	return jsonv.O("state", s.State, "received", digits(s.Received))
}
