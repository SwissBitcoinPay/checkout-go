package httpx

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sattleme/checkout-go/internal/errs"
	"github.com/sattleme/checkout-go/internal/jsonv"
	"github.com/sattleme/checkout-go/internal/testkit"
	"github.com/sattleme/checkout-go/internal/transport"
)

func TestAsksWithTheUserAgentAndJSON(t *testing.T) {
	fake := testkit.NewFakeHTTP(func(*http.Request) testkit.Answer { return testkit.JSON(jsonv.Members{}) })
	c := New(fake, time.Second)
	if _, err := c.GetJSON(context.Background(), "https://api.coingecko.com/x", map[string]string{"x-cg-demo-api-key": "k", "User-Agent": "other"}); err != nil {
		t.Fatal(err)
	}
	h := fake.Calls()[0].Headers
	if h.Get("User-Agent") != "sattleme-Checkout/1" || h.Get("Accept") != "application/json" || h.Get("x-cg-demo-api-key") != "k" {
		t.Errorf("headers %v", h)
	}
}

func TestBodies(t *testing.T) {
	for name, c := range map[string]struct {
		body string
		json bool
	}{
		"JSON":                        {`{"a":1}`, true},
		"one byte order mark":         {"\xef\xbb\xbf{\"a\":1}", true},
		"two byte order marks":        {"\xef\xbb\xbf\xef\xbb\xbf{\"a\":1}", false},
		"invalid UTF-8":               {"{\"a\":\"\xff\"}", false},
		"not JSON":                    {"<html>", false},
		"a byte order mark elsewhere": {"{\"a\":1}\xef\xbb\xbf", false},
	} {
		body := c.body
		fake := testkit.NewFakeHTTP(func(*http.Request) testkit.Answer { return testkit.Answer{Text: &body} })
		a, err := New(fake, time.Second).GetJSON(context.Background(), "https://breez.tips/v", nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if (a.Body != nil) != c.json {
			t.Errorf("%s: body %v", name, a.Body)
		}
	}
}

func TestNoAnswers(t *testing.T) {
	big := `"` + strings.Repeat("a", 1<<20) + `"`
	exact := strings.Repeat(" ", 1<<20-2) + "{}"
	for name, c := range map[string]struct {
		answer  testkit.Answer
		network bool
	}{
		"a redirect":            {testkit.Answer{Status: 301, Headers: map[string]string{"Location": "https://evil.example"}}, true},
		"a 307":                 {testkit.Answer{Status: 307}, true},
		"more than 1 MiB":       {testkit.Answer{Text: &big}, true},
		"1 MiB exactly":         {testkit.Answer{Text: &exact}, false},
		"a connection refused":  {testkit.Answer{Err: errors.New("connection refused")}, true},
		"a server error is one": {testkit.Status(500, jsonv.Members{}), false},
	} {
		fake := testkit.NewFakeHTTP(func(req *http.Request) testkit.Answer {
			if req.URL.Host != "breez.tips" {
				t.Errorf("%s: asked %s", name, req.URL)
			}
			return c.answer
		})
		_, err := New(fake, time.Second).GetJSON(context.Background(), "https://breez.tips/v", nil)
		if got := errs.Is(err, errs.Network); got != c.network {
			t.Errorf("%s: %v", name, err)
		}
		if len(fake.Calls()) != 1 {
			t.Errorf("%s: %d requests (a redirect followed?)", name, len(fake.Calls()))
		}
	}
}

// A request that runs out of its own time is network, never the call's cancellation; a cancelled
// call is the context's error, never network.
func TestTimeoutAndCancellation(t *testing.T) {
	slow := roundTripper(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	c := New(slow, 100*time.Millisecond)
	_, err := c.GetJSON(context.Background(), "https://breez.tips/v", nil)
	if !errs.Is(err, errs.Network) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Errorf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err = New(slow, 10*time.Second).GetJSON(ctx, "https://breez.tips/v", nil)
	if !errors.Is(err, context.Canceled) || errs.Is(err, errs.Network) {
		t.Errorf("cancelled: %v", err)
	}
}

// A request ends with its context whatever the transport does: an injected transport that never
// heeds it (it answers once released) is let go when the request's time runs out (network) or
// when the call is cancelled (the context's error), never waited for.
func TestARequestEndsWithItsContextWhateverTheTransport(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	deaf := roundTripper(func(*http.Request) (*http.Response, error) {
		<-release
		return nil, errors.New("released")
	})
	get := func(c *Client, ctx context.Context) error {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			_, err := c.GetJSON(ctx, "https://breez.tips/v", nil)
			done <- err
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(20 * time.Second):
			t.Fatal("the request waits for a transport that does not heed its context")
			return nil
		}
	}
	if err := get(New(deaf, 100*time.Millisecond), context.Background()); !errs.Is(err, errs.Network) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if err := get(New(deaf, time.Minute), ctx); !errors.Is(err, context.Canceled) || errs.Is(err, errs.Network) {
		t.Errorf("cancelled: %v", err)
	}
}

// A URL's error holds the URL: a cause never does (a Lightning address's name is in its path).
func TestCausesHoldNoURL(t *testing.T) {
	failing := roundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial failed") })
	_, err := New(failing, time.Second).GetJSON(context.Background(), "https://breez.tips/.well-known/lnurlp/secretshop", nil)
	for e := error(err); e != nil; e = errors.Unwrap(e) {
		if strings.Contains(e.Error(), "secretshop") {
			t.Errorf("%T holds the path: %v", e, e)
		}
	}
}

// An answer that comes once its request was let go (its time ran out, a transport that does not
// heed its context) is closed unread: nothing more is asked of the transport for a request nobody
// waits for, its call may have returned.
func TestAnAnswerThatComesOnceItsRequestWasLetGoIsClosedUnread(t *testing.T) {
	release := make(chan struct{})
	body := &watchedBody{closed: make(chan struct{})}
	deaf := roundTripper(func(*http.Request) (*http.Response, error) {
		<-release
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, nil
	})
	if _, err := New(deaf, 100*time.Millisecond).GetJSON(context.Background(), "https://breez.tips/v", nil); !errs.Is(err, errs.Network) {
		t.Fatalf("timeout: %v", err)
	}
	close(release)
	select {
	case <-body.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the body was not closed")
	}
	if n := body.readCount(); n != 0 {
		t.Errorf("%d reads of the body", n)
	}
}

// watchedBody is an empty body that counts its reads, and says when it is closed.
type watchedBody struct {
	mu     sync.Mutex
	reads  int
	once   sync.Once
	closed chan struct{}
}

func (b *watchedBody) Read([]byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	return 0, io.EOF
}

func (b *watchedBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func (b *watchedBody) readCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reads
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// SPEC.md §15 (Head): what the client holds of an answer's head is 10 MiB at most in all: its
// status line and headers are read up to 10 MiB in all, a chunked body's size lines up to 4 KiB
// each, and its trailers only when they end within 4 KiB (net/http's bounds, the first set by the
// package's transport). A larger head is no answer (network), never read on: one header line of
// 11 MiB, 10 MiB of short ones, a chunk extension of 5 000 bytes or of 11 MiB, 11 MiB of zeros
// before a chunk's size, 11 MiB of trailers. Answers of a local server, over the package's own
// transport, read as a merchant's Bitcoin node's (in clear http: SPEC.md §15 holds for it too).
func TestAnAnswersHeadIsBounded(t *testing.T) {
	big := strings.Repeat("a", transport.MaxHeadBytes+1<<20)
	answers := map[string]string{
		"/header": "HTTP/1.1 200 OK\r\nX-Large: " + big + "\r\nContent-Length: 2\r\n\r\n{}",
		"/headers": "HTTP/1.1 200 OK\r\n" + strings.Repeat("X-Many: "+strings.Repeat("b", 1000)+"\r\n", transport.MaxHeadBytes/1000) +
			"Content-Length: 2\r\n\r\n{}",
		"/chunk":     "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2;" + strings.Repeat("x", 5000) + "\r\n{}\r\n0\r\n\r\n",
		"/extension": "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2;" + big + "\r\n{}\r\n0\r\n\r\n",
		"/zeros":     "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" + strings.Repeat("0", len(big)) + "2\r\n{}\r\n0\r\n\r\n",
		"/trailers":  "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\nX-Trailer: " + big + "\r\n\r\n",
		"/fine": "HTTP/1.1 200 OK\r\nX-Large: " + big[:1<<20] + "\r\nTransfer-Encoding: chunked\r\n\r\n0002;x\r\n{}\r\n0\r\n" +
			"X-Trailer: " + big[:1000] + "\r\n\r\n",
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				request, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				_, _ = io.WriteString(conn, answers[strings.TrimPrefix(request.URL.Path, "/api")])
			}()
		}
	}()
	rt := transport.New(nil)
	rt.Proxy = nil
	// The node's host is the local server.
	rt.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, listener.Addr().String())
	}
	c := New(rt, 30*time.Second).ForNode()
	for path := range answers {
		_, err := c.GetJSON(context.Background(), "http://192.168.1.10:3006/api"+path, nil)
		if path == "/fine" {
			if err != nil {
				t.Errorf("%s: %v", path, err)
			}
		} else if !errs.Is(err, errs.Network) {
			t.Errorf("%s: %v", path, err)
		}
	}
}

// https URLs only (SPEC.md §15), but for the reads of a merchant's Bitcoin node (ForNode), which
// may be in clear http to a host of a local network (SPEC.md §31): never http elsewhere, nor to
// another host, nor through a client that is not the node's.
func TestPlainHTTPOnlyForANodeOfALocalNetwork(t *testing.T) {
	fake := testkit.NewFakeHTTP(func(*http.Request) testkit.Answer { return testkit.JSON(jsonv.Members{}) })
	c := New(fake, time.Second)
	node := c.ForNode()
	for _, u := range []string{"https://breez.tips/v", "https://mempool.space/api/blocks/tip/height", "https://node.example/api/x"} {
		for _, client := range []*Client{c, node} {
			if _, err := client.GetJSON(context.Background(), u, nil); err != nil {
				t.Errorf("%s: %v", u, err)
			}
		}
	}
	local := []string{"http://192.168.1.10:3006/api/blocks/tip/height", "http://umbrel.local/api/x", "http://10.1.2.3/api", "http://100.64.0.1:80/api",
		"http://172.16.0.1/mempool/api/address/bc1p/txs"}
	for _, u := range local {
		if _, err := node.GetJSON(context.Background(), u, nil); err != nil {
			t.Errorf("%s: %v", u, err)
		}
		if _, err := c.GetJSON(context.Background(), u, nil); !errs.Is(err, errs.Network) {
			t.Errorf("%s without the node's client: %v", u, err)
		}
	}
	asked := len(fake.Calls())
	for _, u := range []string{"http://breez.tips/v", "http://node.example/api", "http://8.8.8.8/api", "http://127.0.0.1/api", "http://localhost/api",
		"http://0.0.0.0/api", "http://172.32.0.1/api", "http://user@192.168.1.10/api", "http://192.168.1.10:0/api", "http://[::1]/api",
		"ws://192.168.1.10/api", "ftp://192.168.1.10/api", "http:///api"} {
		for _, client := range []*Client{c, node} {
			if _, err := client.GetJSON(context.Background(), u, nil); !errs.Is(err, errs.Network) {
				t.Errorf("%s: %v", u, err)
			}
			if _, _, _, err := client.GetText(context.Background(), u); !errs.Is(err, errs.Network) {
				t.Errorf("%s: %v", u, err)
			}
		}
	}
	if n := len(fake.Calls()); n != asked {
		t.Errorf("%d requests refused were made", n-asked)
	}
}
