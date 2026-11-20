package checkout

import (
	"context"
	"errors"
	"net/http"

	"github.com/sattleme/checkout-go/internal/relay"
	"github.com/sattleme/checkout-go/internal/wsdial"
)

// WebSocketConn is a WebSocket connection, as the package uses one: a message at a time. Read is
// called from one goroutine and Write from another, at the same time; Close from any, while they
// run.
type WebSocketConn interface {
	// Read returns the next message (text or binary).
	Read(ctx context.Context) ([]byte, error)
	// Write sends a text message.
	Write(ctx context.Context, data []byte) error
	// Close closes the connection, without waiting for the peer: a Read or a Write running then
	// returns.
	Close() error
}

// WebSocketDialer opens the WebSockets of the relays and of the explorer (wss:// URLs, and ws://
// for a merchant's Bitcoin node of a local network, SPEC.md §31): the opening handshake carries
// header (the User-Agent of SPEC.md §15) and is bounded by ctx. A dialer of yours SHOULD follow no
// redirect (a redirect is a failed connection: it would skip the checks of the URL), offer no
// compression and cap a message at 1 MiB, as NewWebSocketDialer's does. Dial is called from several
// goroutines at once (a call opens every relay of a list at once): it must be safe for concurrent
// use.
type WebSocketDialer interface {
	Dial(ctx context.Context, url string, header http.Header) (WebSocketConn, error)
}

// NewWebSocketDialer is the package's own WebSocket client (github.com/coder/websocket) over
// transport (when nil, the package's default: http.DefaultTransport's settings, each connection
// dialed, its name resolved, within the handshake's context): no redirect followed, no
// compression offered (permessage-deflate: a few compressed bytes would unpack into many large
// messages, and sealed records do not compress), a message read up to 1 MiB. Over a typed nil (a
// nil *http.Transport), it is no dialer: New refuses it (invalid_option), and it dials nothing.
func NewWebSocketDialer(transport http.RoundTripper) WebSocketDialer {
	if transport != nil && isNil(transport) {
		return dialer{}
	}
	return dialer{d: wsdial.New(transport)}
}

// dialer is NewWebSocketDialer's; d is nil over a typed nil transport.
type dialer struct{ d *wsdial.Dialer }

func (d dialer) Dial(ctx context.Context, url string, header http.Header) (WebSocketConn, error) {
	if d.d == nil {
		return nil, errors.New("checkout: NewWebSocketDialer of a nil http.RoundTripper dials nothing")
	}
	return d.d.Dial(ctx, url, header)
}

// engineDialer is the engine's dialer of the integrator's.
type engineDialer struct{ d WebSocketDialer }

func (d engineDialer) Dial(ctx context.Context, url string, header http.Header) (relay.Conn, error) {
	conn, err := d.d.Dial(ctx, url, header)
	if err != nil {
		return nil, err
	}
	return conn, nil
}
