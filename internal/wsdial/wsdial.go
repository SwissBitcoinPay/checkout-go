// Package wsdial is the package's own WebSocket client (coder/websocket), for the relays and the
// explorer: the opening handshake follows no redirect (a redirect, 3xx, is a failed connection:
// it would skip the checks of the URL, SPEC.md §13), carries the given headers (the User-Agent of
// SPEC.md §15), offers no compression (permessage-deflate: a few compressed bytes would unpack
// into many large messages, and sealed records do not compress), and a message is read up to
// 1 MiB. The handshake is bounded by the context the caller gives it.
package wsdial

import (
	"context"
	"net/http"

	"github.com/coder/websocket"

	"github.com/sattleme/checkout-go/internal/proto"
	"github.com/sattleme/checkout-go/internal/transport"
)

// Conn is a WebSocket connection: one text message at a time.
type Conn interface {
	// Read returns the next message.
	Read(ctx context.Context) ([]byte, error)
	// Write sends a text message.
	Write(ctx context.Context, data []byte) error
	// Close closes the connection; it does not wait for the peer.
	Close() error
}

// Dialer opens WebSocket connections over an HTTP transport.
type Dialer struct {
	client *http.Client
}

// New returns a dialer over transport (transport.Default when nil: its name resolved within the
// handshake's context, SPEC.md §13).
func New(rt http.RoundTripper) *Dialer {
	if rt == nil {
		rt = transport.Default
	}
	return &Dialer{client: &http.Client{
		Transport: rt,
		// A redirect is handed over as it is, never followed: the handshake then fails (101 is
		// the only answer it takes).
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Dial opens a connection to url with the headers given.
func (d *Dialer) Dial(ctx context.Context, url string, header http.Header) (Conn, error) {
	c, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient:      d.client,
		HTTPHeader:      header,
		CompressionMode: websocket.CompressionDisabled,
	})
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(proto.MaxRelayMessageBytes)
	return &conn{c: c}, nil
}

type conn struct {
	c *websocket.Conn
}

func (c *conn) Read(ctx context.Context) ([]byte, error) {
	_, data, err := c.c.Read(ctx)
	return data, err
}

func (c *conn) Write(ctx context.Context, data []byte) error {
	return c.c.Write(ctx, websocket.MessageText, data)
}

func (c *conn) Close() error {
	// The closing handshake waits for the peer (5 s at most): not the caller.
	go c.c.Close(websocket.StatusNormalClosure, "")
	return nil
}
