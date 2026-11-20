// Package httpx makes the package's HTTP requests (SPEC.md §15): GET asking for JSON with the
// User-Agent of every implementation, no redirect followed (each URL is checked before it is
// asked: a redirect would skip the check, it is no answer), the body read up to 1 MiB within the
// request's timeout, one leading byte order mark ignored. https URLs only, but for the reads of a
// merchant's Bitcoin node (ForNode), whose URL follows a rule of its own (SPEC.md §31): http for
// a node of a local network.
package httpx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/sattleme/checkout-go/internal/bitcoinnode"
	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/proto"
)

// Client makes the requests of one client, over its transport.
type Client struct {
	client  *http.Client
	timeout time.Duration
	// node allows http to a node of a local network (ForNode).
	node bool
}

// New returns a client over the transport, each request (body included) bounded by timeout.
func New(transport http.RoundTripper, timeout time.Duration) *Client {
	return &Client{
		client: &http.Client{
			Transport: transport,
			// A redirect is handed over as it is, never followed: getBytes refuses it.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		timeout: timeout,
	}
}

// ForNode is the client of the reads of a merchant's Bitcoin node: c's, but that a URL whose
// scheme and authority are a node URL's of a local host (bitcoinnode.ParseURL: 10.0.0.0/8,
// 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, a name under .local) may be asked in clear http.
// Every other rule of SPEC.md §15 holds for it.
func (c *Client) ForNode() *Client {
	return &Client{client: c.client, timeout: c.timeout, node: true}
}

// allowed tells whether rawURL may be asked: https, or, for a node's reads, http to a host of a
// local network (SPEC.md §31: its scheme and authority a node URL's).
func (c *Client) allowed(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" || !c.node {
		return false
	}
	// A node URL in http is of a local host (bitcoinnode.ParseURL).
	_, ok := bitcoinnode.ParseURL("http://" + u.Host)
	return ok
}

// Answer is an HTTP answer: its status, and its body as JSON (nil when it is not JSON in UTF-8).
type Answer struct {
	Status int
	// OK is a status from 200 to 299.
	OK   bool
	Body any
}

// HostOf is the host of a URL, for messages: never its path (a Lightning address's name is in
// it).
func HostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "(invalid URL)"
	}
	return u.Host
}

// GetJSON asks for rawURL (headers added to the package's own). An error of the package
// (network) when there is no answer in time, a redirect or a body too large; ctx's error when ctx
// is done.
func (c *Client) GetJSON(ctx context.Context, rawURL string, headers map[string]string) (Answer, error) {
	status, data, err := c.getBytes(ctx, rawURL, headers)
	if err != nil {
		return Answer{}, err
	}
	answer := Answer{Status: status, OK: status >= 200 && status <= 299}
	if text, ok := BodyText(data); ok {
		if v, err := jsonv.ParseString(text); err == nil {
			answer.Body = v
		}
	}
	return answer, nil
}

// GetText asks for rawURL as GetJSON does, for the one plain-text answer (an explorer's height):
// the body as text, false when it is not UTF-8.
func (c *Client) GetText(ctx context.Context, rawURL string) (status int, text string, isText bool, err error) {
	status, data, err := c.getBytes(ctx, rawURL, nil)
	if err != nil {
		return 0, "", false, err
	}
	text, isText = BodyText(data)
	return status, text, isText, nil
}

// BodyText is the text of an HTTP body (SPEC.md §15): UTF-8, one leading byte order mark (EF BB
// BF) ignored, as WHATWG's UTF-8 decode does; false when it is not UTF-8.
func BodyText(data []byte) (string, bool) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) {
		return "", false
	}
	return string(data), true
}

// fetched is what a request's transport gave: the status and the body (1 MiB and a byte at most),
// or what failed.
type fetched struct {
	status int
	data   []byte
	err    error
}

func (c *Client) getBytes(ctx context.Context, rawURL string, headers map[string]string) (int, []byte, error) {
	host := HostOf(rawURL)
	// Checked before by each caller (an https URL, SPEC.md §15; the node's own rule, SPEC.md §31):
	// plain http goes nowhere else.
	if !c.allowed(rawURL) {
		return 0, nil, errs.NewNetwork("Not an https URL: "+host, nil)
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, errs.NewNetwork("No answer from "+host, err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", proto.UserAgent)
	// The request ends with its context whatever the transport does: one that does not heed it
	// (an injected one) is let go, its answer dropped when it comes. So a request is bounded by
	// its timeout, and a cancelled call, or a check that was stopped, waits for nothing.
	answer := make(chan fetched, 1)
	go func() {
		response, err := c.client.Do(request)
		if err != nil {
			answer <- fetched{err: err}
			return
		}
		defer response.Body.Close()
		// An answer that comes once the request was let go is closed unread: nobody waits for it.
		if err := requestCtx.Err(); err != nil {
			answer <- fetched{err: err}
			return
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, proto.MaxHTTPBodyBytes+1))
		answer <- fetched{status: response.StatusCode, data: data, err: err}
	}()
	var got fetched
	select {
	case got = <-answer:
	case <-requestCtx.Done():
		got.err = requestCtx.Err()
	}
	if got.err != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, errs.NewNetwork("No answer from "+host, got.err)
	}
	// The client hands a redirect over (CheckRedirect); an injected transport may too.
	if got.status >= 300 && got.status < 400 {
		return 0, nil, errs.NewNetwork(host+" answered a redirect (HTTP "+strconv.Itoa(got.status)+")", nil)
	}
	if len(got.data) > proto.MaxHTTPBodyBytes {
		return 0, nil, errs.NewNetwork(host+" answered more than "+strconv.Itoa(proto.MaxHTTPBodyBytes)+" bytes", nil)
	}
	return got.status, got.data, nil
}

// IsNetwork tells whether err is the package's network error.
func IsNetwork(err error) bool {
	var e *errs.Error
	return errors.As(err, &e) && e.Code == errs.Network
}
