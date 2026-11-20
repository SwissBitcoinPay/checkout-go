package engine

import (
	"context"
	"sync"
	"time"

	"github.com/sattleme/checkout-go/internal/bitcoinnode"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/lnurl"
	"github.com/sattleme/checkout-go/internal/logx"
	"github.com/sattleme/checkout-go/internal/model"
	"github.com/sattleme/checkout-go/internal/onchain"
	"github.com/sattleme/checkout-go/internal/proto"
)

// WaitForPayment checks the payment until it is paid or expired (SPEC.md §23): its Lightning
// part, and the invoices of the rest of an on-chain underpayment, every interval; its on-chain part
// (with an address) each time the explorer's WebSocket says something changed for the address,
// every 60 s at least, and at once when an invoice of the rest is paid. Network errors are retried
// (one that each check meets again logged once, and again only after it stopped); onStatus hears
// each status. ctx's error when it is done, and any error that is not a network one.
func (e *Engine) WaitForPayment(ctx context.Context, stored *Stored, interval time.Duration, onStatus func(*model.PaymentStatus)) (*model.PaymentStatus, error) {
	// A failure each check meets again is logged once, and again only after it stopped (§23).
	warnings := newOnceWarnings(e.logger)
	if stored.OnchainAddress != "" {
		return e.waitWithAddress(ctx, stored, interval, onStatus, warnings)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, err := e.check(ctx, stored, nil, warnings, nil)
		var status *model.PaymentStatus
		if err == nil {
			status, err = statusOf(stored, p, e.seconds())
		}
		if err == nil {
			if onStatus != nil {
				onStatus(status)
			}
			if status.State == model.Paid || status.State == model.Expired {
				return status, nil
			}
		} else {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !errs.Is(err, errs.Network) {
				return nil, err
			}
			warnings.failed(err)
		}
		warnings.checked()
		if err := sleep(ctx, interval); err != nil {
			return nil, err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type fullCheck struct {
	mine int
	p    *parts
	err  error
}

type lightningCheck struct {
	// own is the invoice's own Lightning part, when it was checked (checkedOwn).
	own        *onchain.LightningResult
	checkedOwn bool
	known      *onchain.RestPart
	results    []onchain.LightningResult
}

// waitWithAddress is WaitForPayment of an invoice with an address: each status combines the last
// result of each part. A full check (check: every part, an invoice of the rest asked when needed)
// at each change the explorer's WebSocket pushes and every 60 s; between them, the Lightning
// invoice and the rest invoices the last full check found, every interval. The checks run beside
// the wait, under a context of its own: when it returns (a status, an error, ctx done), every check
// it started is stopped and has ended, so that none publishes, asks or declares anything once the
// call is over (SPEC.md §1, §4).
func (e *Engine) waitWithAddress(ctx context.Context, stored *Stored, interval time.Duration, onStatus func(*model.PaymentStatus), warnings *onceWarnings) (*model.PaymentStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	checks, stopChecks := context.WithCancel(ctx)
	var running sync.WaitGroup
	defer func() {
		stopChecks()
		running.Wait()
	}()
	changes := make(chan struct{}, 1)
	// The explorer's WebSocket: of the node the last channel read names (SPEC.md §31), with its
	// fallback, opened once a check read the channel (before, whether mempool.space may be asked
	// is not known), and opened again when the channel names another node. The node's failures
	// are the call's, shared by its checks and its WebSocket. Closed before the checks are waited
	// for (deferred after them): none opens it again once the call is over.
	failures := onchain.NewNodeFailures()
	var watchMu sync.Mutex
	var watcher *onchain.Watch
	watched, over := "", false
	call := &callExplorer{failures: failures, onNode: func(node *bitcoinnode.Node) {
		key := onchain.MempoolExplorer
		if node != nil {
			key = string(node.Fallback) + " " + node.URL
		}
		watchMu.Lock()
		if over || key == watched {
			watchMu.Unlock()
			return
		}
		previous := watcher
		watched = key
		watcher = onchain.WatchAddress(ctx, onchain.WatchOptions{
			Dialer:   e.dialer,
			Header:   e.header,
			Timeout:  e.relayTimeout,
			Explorer: onchain.NewExplorer(e.http, node, failures, e.now),
		}, stored.OnchainAddress, func() {
			select {
			case changes <- struct{}{}:
			default:
			}
		})
		watchMu.Unlock()
		if previous != nil {
			previous.Close()
		}
	}}
	defer func() {
		watchMu.Lock()
		over = true
		current := watcher
		watchMu.Unlock()
		if current != nil {
			current.Close()
		}
	}()
	// The last result of each part; unknown until it was checked once.
	var lightning *onchain.LightningResult
	lightningKnown := stored.Lightning == nil
	var onchainResult *onchain.OnchainResult
	onchainKnown := false
	var rests *onchain.RestPart
	var lightningTimer, onchainTimer *time.Timer
	timerC := func(t *time.Timer) <-chan time.Time {
		if t == nil {
			return nil
		}
		return t.C
	}
	stop := func(t *time.Timer) {
		if t != nil {
			t.Stop()
		}
	}
	defer func() {
		stop(lightningTimer)
		stop(onchainTimer)
	}()
	checking, again, lightningChecking := false, false, false
	// Full checks started, the last one that ended, and the first one a status must wait for.
	started, ended, waitFor := 0, 0, 0
	// At most one full check and one check of the Lightning invoices at a time: neither ever
	// waits to hand its result over, even once this call returned.
	fullDone := make(chan fullCheck, 1)
	lightningDone := make(chan lightningCheck, 1)

	var final *model.PaymentStatus
	var failure error
	done := false
	finish := func(status *model.PaymentStatus, err error) {
		if !done {
			done, final, failure = true, status, err
		}
	}
	report := func() {
		if done || !lightningKnown || !onchainKnown {
			return
		}
		// A rest invoice was paid after the last full check began: its on-chain part was judged
		// against what was due then, never combined with it (it might read underpaid for good).
		if ended < waitFor {
			return
		}
		status, err := statusOf(stored, &parts{lightning: lightning, onchain: onchainResult, rests: rests}, e.seconds())
		if err != nil {
			if !errs.Is(err, errs.Network) {
				finish(nil, err)
			} else {
				warnings.failed(err)
			}
			return
		}
		if onStatus != nil {
			onStatus(status)
		}
		if status.State == model.Paid || status.State == model.Expired {
			finish(status, nil)
		}
	}
	// lightningPending tells whether a Lightning invoice (the order's, or one of the rest) may
	// still change.
	lightningPending := func() bool {
		if !lightningKnown || lightning != nil && !isFinal(*lightning) {
			return true
		}
		if rests != nil {
			for _, r := range rests.Results {
				if !isFinal(r) {
					return true
				}
			}
		}
		return false
	}
	scheduleLightning := func(delay time.Duration) {
		if done || lightningTimer != nil || lightningChecking || !lightningPending() {
			return
		}
		lightningTimer = time.NewTimer(delay)
	}
	checkLightning := func() {
		lightningTimer = nil
		lightningChecking = true
		known := rests
		checkOwn := stored.Lightning != nil && (!lightningKnown || lightning != nil && !isFinal(*lightning))
		running.Add(1)
		go func() {
			defer running.Done()
			r := lightningCheck{checkedOwn: checkOwn, known: known}
			var wg sync.WaitGroup
			if checkOwn {
				wg.Add(1)
				go func() {
					defer wg.Done()
					own := e.verify(checks, *stored.Lightning)
					r.own = &own
				}()
			}
			if known != nil {
				r.results = make([]onchain.LightningResult, len(known.Invoices))
				for i, inv := range known.Invoices {
					if isFinal(known.Results[i]) {
						r.results[i] = known.Results[i]
						continue
					}
					wg.Add(1)
					go func(i int, inv lnurl.Invoice) {
						defer wg.Done()
						r.results[i] = e.verify(checks, inv)
					}(i, lightningOf(inv))
				}
			}
			wg.Wait()
			lightningDone <- r
		}()
	}
	checkOnchain := func() {
		if done {
			return
		}
		if checking {
			again = true
			return
		}
		checking = true
		started++
		mine := started
		// Every 60 s at least: a socket may go quiet without closing.
		stop(onchainTimer)
		onchainTimer = time.NewTimer(proto.OnchainCheckIntervalMs * time.Millisecond)
		running.Add(1)
		go func() {
			defer running.Done()
			p, err := e.check(checks, stored, nil, warnings, call)
			fullDone <- fullCheck{mine: mine, p: p, err: err}
		}()
	}

	checkOnchain()
	for !done {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changes:
			checkOnchain()
		case <-timerC(onchainTimer):
			onchainTimer = nil
			checkOnchain()
		case <-timerC(lightningTimer):
			checkLightning()
		case r := <-fullDone:
			checking = false
			if r.err != nil {
				// A cancellation: only a cancelled call ends a full check with an error.
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				finish(nil, r.err)
				break
			}
			ended = r.mine
			onchainResult, onchainKnown = r.p.onchain, true
			rests = r.p.rests
			if r.p.lightning != nil {
				lightning, lightningKnown = r.p.lightning, true
			}
			report()
			// A full check ended: what it, and the checks of the Lightning invoices since the last
			// one, no longer met has stopped.
			warnings.checked()
			scheduleLightning(interval)
			if again {
				again = false
				checkOnchain()
			}
		case r := <-lightningDone:
			lightningChecking = false
			if done {
				break
			}
			if r.checkedOwn {
				lightning, lightningKnown = r.own, true
			}
			paid := false
			// A full check may have found other rest invoices meanwhile: its results stand.
			if r.known != nil && rests == r.known {
				for i, result := range r.results {
					if isPaid(result) && !isPaid(r.known.Results[i]) {
						paid = true
					}
				}
				rests = &onchain.RestPart{Invoices: r.known.Invoices, Results: r.results, Err: r.known.Err, Unrecorded: r.known.Unrecorded}
			}
			// Paid by a rest invoice: what is due on-chain changed, a full check tells the status.
			if paid {
				waitFor = started + 1
				checkOnchain()
			} else {
				report()
			}
			scheduleLightning(interval)
		}
	}
	if failure != nil {
		return nil, failure
	}
	return final, nil
}

// onceWarnings are the warnings of the checks of one WaitForPayment (SPEC.md §23): a warning each
// check meets again (an explorer or a Lightning address that does not answer, a rest invoice that
// cannot be asked, a witness that cannot be published) is logged once, and again only once a
// check went without it. Safe for concurrent use.
type onceWarnings struct {
	mu     sync.Mutex
	logger logx.Logger
	before map[string]bool
	now    map[string]bool
}

func newOnceWarnings(logger logx.Logger) *onceWarnings {
	return &onceWarnings{logger: logger, before: map[string]bool{}, now: map[string]bool{}}
}

// Warn logs message unless the last check or this one met it already.
func (w *onceWarnings) Warn(message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.before[message] && !w.now[message] {
		w.logger.Warn(message)
	}
	w.now[message] = true
}

// failed warns of a check that failed (network): it is checked again.
func (w *onceWarnings) failed(err error) {
	w.Warn("github.com/sattleme/checkout-go: the payment check failed (" + err.Error() + "); checked again")
}

// checked tells that a check ended: a warning it did not meet stopped.
func (w *onceWarnings) checked() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.before, w.now = w.now, map[string]bool{}
}
