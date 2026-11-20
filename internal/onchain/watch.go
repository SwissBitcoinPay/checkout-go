package onchain

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/relay"
)

// The explorer's WebSocket (SPEC.md §31, "Waiting for an on-chain payment"): while waitForPayment
// waits, the explorer (mempool.space, or the merchant's Bitcoin node) pushes what changes for the
// address (a transaction entering the mempool, confirmed, or leaving it) and each new block (one
// more confirmation). Only a signal to check the on-chain part again: what it pushes is never
// trusted, the check asks the explorer.

// changes are the messages that say something changed for the address, or for its confirmations.
var changes = []string{"address-transactions", "block-transactions", "address-removed-transactions", "block"}

// WatchOptions is how the explorer's WebSocket is reached.
type WatchOptions struct {
	Dialer relay.Dialer
	// Header is what each handshake carries: the User-Agent of SPEC.md §13 and §15.
	Header http.Header
	// Timeout bounds each handshake (relayTimeoutMs) and each message sent.
	Timeout time.Duration
	// Explorer is the explorer of the call, whose sources each connection tries in order: nil for
	// mempool.space's WebSocket alone.
	Explorer *Explorer
	// Sleep waits d or until ctx is done (time's own when nil): how a test hurries the
	// reconnections.
	Sleep func(ctx context.Context, d time.Duration)
}

// Watch is a watch of an address, until Close.
type Watch struct {
	stop context.CancelFunc
	done chan struct{}
}

// Close ends the watch, and waits until its connection is let go.
func (w *Watch) Close() {
	w.stop()
	<-w.done
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// WatchAddress watches address on the explorer's WebSocket until Close or ctx is done: onChange
// is called at each connection (what happened before was not pushed) and at each message about
// the address or a new block, from the watch's own goroutine. Each connection tries the WebSockets
// of the explorer's sources in order (Explorer.Sources): one that fails, or is lost before any
// message, goes to the next at once (the node's to mempool.space's with fallback "mempool",
// SPEC.md §31; the node's failure remembered for the call, so that for 60 s mempool.space's is
// tried first). A lost socket connects again after 1 s, doubled at each failure (of the last one
// tried) up to 30 s; a connection lost before it received a message is a failure, so that an
// explorer that accepts and drops each connection (each one starts a full check) is connected to
// less and less often.
func WatchAddress(ctx context.Context, o WatchOptions, address string, onChange func()) *Watch {
	ctx, stop := context.WithCancel(ctx)
	w := &Watch{stop: stop, done: make(chan struct{})}
	sources := func() []Source {
		if o.Explorer == nil {
			return []Source{{Name: MempoolExplorer, WebSocket: proto.MempoolWebSocketURL}}
		}
		return o.Explorer.Sources()
	}
	wait := o.Sleep
	if wait == nil {
		wait = sleep
	}
	want := jsonv.Marshal(jsonv.O("action", "want", "data", []string{"blocks"}))
	track := jsonv.Marshal(jsonv.O("track-address", address))
	// connection runs one connection to url until it is lost: whether the explorer answered on it
	// (a message received), which sets the delay of the next connection back to 1 s; else it
	// failed.
	connection := func(url string) bool {
		dialCtx, cancel := context.WithTimeout(ctx, o.Timeout)
		conn, err := o.Dialer.Dial(dialCtx, url, o.Header.Clone())
		cancel()
		if err != nil {
			return false
		}
		var once sync.Once
		release := func() { once.Do(func() { conn.Close() }) }
		defer release()
		// Closed as soon as the watch ends, whatever its reads and writes heed: Close does not wait
		// for them.
		closeAtEnd := context.AfterFunc(ctx, release)
		defer closeAtEnd()
		for _, message := range [][]byte{want, track} {
			writeCtx, cancel := context.WithTimeout(ctx, o.Timeout)
			err := conn.Write(writeCtx, message)
			cancel()
			if err != nil {
				return false
			}
		}
		if ctx.Err() != nil {
			return false
		}
		onChange()
		answered := false
		for {
			data, err := conn.Read(ctx)
			if err != nil {
				return answered
			}
			answered = true
			message, err := jsonv.Parse(data)
			if err != nil {
				continue
			}
			if o, ok := jsonv.AsObject(message); ok {
				for _, name := range changes {
					if o.Has(name) {
						onChange()
						break
					}
				}
			}
		}
	}
	go func() {
		defer close(w.done)
		delay := time.Duration(proto.ReconnectFirstMs) * time.Millisecond
		for ctx.Err() == nil {
			// The sources in order, the next at once when one fails.
			answered := false
			for _, source := range sources() {
				if connection(source.WebSocket) {
					answered = true
					break
				}
				if ctx.Err() != nil {
					return
				}
				if source.Name == NodeExplorer {
					o.Explorer.NodeFailed()
				}
			}
			if answered {
				delay = time.Duration(proto.ReconnectFirstMs) * time.Millisecond
			}
			if ctx.Err() != nil {
				return
			}
			wait(ctx, delay)
			delay = min(delay*2, time.Duration(proto.ReconnectMaxMs)*time.Millisecond)
		}
	}()
	return w
}
