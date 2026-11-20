package wsdial

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sattleme/checkout-go/internal/transport"
)

// SPEC.md §13: the opening handshake is bounded by its context, the wait for the connection (its
// host's name resolved) included.
func TestTheHandshakesNameIsResolvedWithinItsContext(t *testing.T) {
	var asked atomic.Int32
	rt := transport.New(&net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			asked.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	rt.Proxy = nil
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := New(rt).Dial(ctx, "wss://never-answers.example", http.Header{}); err == nil {
		t.Fatal("dialed")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("took %v", elapsed)
	}
	if asked.Load() == 0 {
		t.Error("the resolver was not asked")
	}
}

// SPEC.md §15 (Head) and §13: what the client holds of the answer to a WebSocket's opening
// handshake is bounded as an HTTP answer's head, 10 MiB at most (the package's transport): an
// answer with a header of 11 MiB is a failed connection, never read on. The same answer with a
// header of 1 MiB opens: a local server's answers, a valid handshake each, over the package's own
// transport.
func TestAHandshakesAnswerIsBounded(t *testing.T) {
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
				size := 1 << 20
				if request.URL.Path == "/large" {
					size = transport.MaxHeadBytes + 1<<20
				}
				sum := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
					"Sec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(sum[:])+"\r\nX-Large: "+strings.Repeat("h", size)+"\r\n\r\n")
				time.Sleep(2 * time.Second)
			}()
		}
	}()
	rt := transport.New(nil)
	rt.Proxy = nil
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base := "ws://" + listener.Addr().String()
	conn, err := New(rt).Dial(ctx, base+"/fine", http.Header{})
	if err != nil {
		t.Fatalf("a handshake with a header of 1 MiB: %v", err)
	}
	conn.Close()
	if _, err := New(rt).Dial(ctx, base+"/large", http.Header{}); err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("a handshake with a header of 11 MiB: %v", err)
	}
}
