package checkout

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/sattleme/checkout-go/internal/errs"
)

// SPEC.md §5: every error a call fails with, but for a cancellation, is one of the package's.
// publicError, which every method's error goes through (a cancelled call's aside: done gives its
// context's error first), gives nothing else: what the package does not raise is network, what
// failed as its cause, a request's own end (a context's error that is not the call's) included.
func TestPublicErrorGivesOnlyErrorsOfThePackage(t *testing.T) {
	foreign := errors.New("nip44: invalid payload")
	for _, err := range []error{
		foreign,
		fmt.Errorf("sealing: %w", foreign),
		context.DeadlineExceeded,
		context.Canceled,
		fmt.Errorf("a request: %w", context.DeadlineExceeded),
		&url.Error{Op: "Get", URL: "https://breez.tips/.well-known/lnurlp/secretshop", Err: errors.New("dial failed")},
	} {
		got := publicError(err)
		var e *Error
		if !errors.As(got, &e) || e.Code != CodeNetwork || !e.Retryable() {
			t.Errorf("%v: %T %v", err, got, got)
			continue
		}
		// Never read as the call's cancellation, and never with a URL (a Lightning address's name
		// is in its path).
		if errors.Is(got, context.Canceled) || errors.Is(got, context.DeadlineExceeded) {
			t.Errorf("%v reads as a cancellation", err)
		}
		for cause := error(got); cause != nil; cause = errors.Unwrap(cause) {
			if strings.Contains(cause.Error(), "secretshop") {
				t.Errorf("%v: %T holds the URL", err, cause)
			}
		}
	}
	if got := publicError(errs.NewInvalidInvoice("Not this invoice", nil)); !isCode(got, CodeInvalidInvoice) {
		t.Errorf("an error of the package: %T %v", got, got)
	}
	// An Error of this package a custom rate source returned: as it is.
	own := NewRateError("rates.example.ch is down", nil)
	if got := publicError(fmt.Errorf("the source: %w", internalError(own))); got != own {
		t.Errorf("a custom rate source's own: %T %v", got, got)
	}
	if publicError(nil) != nil {
		t.Error("nil")
	}
}

func isCode(err error, code Code) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
