// Package testkit holds what the tests run against: fakes of the Lightning address, the rate
// sources and the explorers over an injected HTTP transport, local relays over an injected
// WebSocket dialer, a merchant's app, a chain. URLs keep their real host names (the specification
// refuses localhost and IP addresses): nothing reaches the network.
package testkit

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"github.com/sattleme/checkout-go/internal/jsonv"
)

// Answer is what a fake HTTP server answers: a status (200 when 0), a body as JSON (written by
// jsonv) or as text, or no answer (Err).
type Answer struct {
	Status int
	JSON   any
	// Text is the body as it is, when set (JSON is then ignored).
	Text    *string
	Headers map[string]string
	Err     error
}

// JSON answers v as JSON with a 200.
func JSON(v any) Answer { return Answer{JSON: v} }

// Status answers v as JSON with status.
func Status(status int, v any) Answer { return Answer{Status: status, JSON: v} }

// Text answers a body as it is.
func Text(status int, body string) Answer { return Answer{Status: status, Text: &body} }

// Call is a request a fake received.
type Call struct {
	URL     *url.URL
	Headers http.Header
}

// FakeHTTP is an http.RoundTripper whose answers come from Route.
type FakeHTTP struct {
	Route func(req *http.Request) Answer
	mu    sync.Mutex
	calls []Call
}

// NewFakeHTTP returns a fake whose requests go to route.
func NewFakeHTTP(route func(req *http.Request) Answer) *FakeHTTP {
	return &FakeHTTP{Route: route}
}

// Calls returns the requests received so far.
func (f *FakeHTTP) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// RoundTrip answers req.
func (f *FakeHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, Call{URL: req.URL, Headers: req.Header.Clone()})
	f.mu.Unlock()
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	a := f.Route(req)
	if a.Err != nil {
		return nil, a.Err
	}
	status := a.Status
	if status == 0 {
		status = 200
	}
	var body []byte
	if a.Text != nil {
		body = []byte(*a.Text)
	} else {
		body = jsonv.Marshal(a.JSON)
	}
	header := http.Header{"Content-Type": {"application/json"}}
	for name, value := range a.Headers {
		header.Set(name, value)
	}
	return &http.Response{
		Status:        strconv.Itoa(status) + " " + http.StatusText(status),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// ErrNoAnswer is a fake server that does not answer.
var ErrNoAnswer = errors.New("testkit: no answer")

// Random is a reproducible stream of bytes for tests (SHA-256 of label and a counter): never the
// randomness of a client but where a test asks for it.
func Random(label string) io.Reader {
	return &stream{label: label}
}

type stream struct {
	mu      sync.Mutex
	label   string
	counter uint64
	buf     []byte
}

func (s *stream) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.buf) < len(p) {
		block := sha256.Sum256([]byte(s.label + "/" + strconv.FormatUint(s.counter, 10)))
		s.counter++
		s.buf = append(s.buf, block[:]...)
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}
