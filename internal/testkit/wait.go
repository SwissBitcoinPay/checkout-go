package testkit

import (
	"testing"
	"time"
)

// Eventually waits until cond holds, 10 s at most (a busy machine is slow), then fails the test.
func Eventually(tb testing.TB, cond func() bool, what string) {
	tb.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			tb.Fatalf("still not: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
