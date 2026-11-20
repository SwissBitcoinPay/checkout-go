package transport

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// stalled is a resolver whose DNS server never answers: each lookup waits until its context ends.
func stalled(asked *atomic.Int32) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			asked.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
}

// SPEC.md §13 and §15: the wait for a connection, its host's name resolved, is within the
// request's timeout; a cancellation stops it too.
func TestTheHostsNameIsResolvedWithinTheRequestsContext(t *testing.T) {
	for name, ctx := range map[string]func() (context.Context, context.CancelFunc){
		"timeout": func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 300*time.Millisecond)
		},
		"cancellation": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(300*time.Millisecond, cancel)
			return ctx, cancel
		},
	} {
		t.Run(name, func(t *testing.T) {
			var asked atomic.Int32
			rt := New(stalled(&asked))
			// No proxy here: the request's own host is resolved.
			rt.Proxy = nil
			client := &http.Client{Transport: rt}
			ctx, cancel := ctx()
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://never-answers.example/x", nil)
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			_, err = client.Do(request)
			if err == nil || !errors.Is(err, ctx.Err()) {
				t.Errorf("got %v", err)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Errorf("took %v", elapsed)
			}
			if asked.Load() == 0 {
				t.Error("the resolver was not asked")
			}
		})
	}
}
