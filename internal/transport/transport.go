// Package transport is the HTTP transport of the package when the integrator gives none: of its
// HTTP requests, and of the WebSocket handshakes of the relays and of the explorer.
package transport

import (
	"net"
	"net/http"
	"time"
)

// MaxHeadBytes bounds an answer's head, its status line and its headers (SPEC.md §15): a larger
// one is no answer. net/http reads a chunked body's size lines up to 4 KiB each.
const MaxHeadBytes = 10 << 20

// New is http.DefaultTransport's settings, each connection dialed within its request's context,
// its host's name resolved by resolver (Go's own when nil) within that context too (SPEC.md §13:
// the wait for the connection is within the timeout). The wait for a connection, its DNS lookup
// included, so counts in each request's timeout, and a call's cancellation stops it. An answer's
// head is read up to MaxHeadBytes (net/http's default bound, set here whatever it becomes).
func New(resolver *net.Resolver) *http.Transport {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Resolver: resolver}
	return &http.Transport{
		Proxy:                  http.ProxyFromEnvironment,
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           100,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: MaxHeadBytes,
	}
}

// Default is New(nil), which every client given no transport shares.
var Default = New(nil)
