package main

import (
	"sync"
	"testing"
	"time"
)

func TestSubmitGuardFirstUse(t *testing.T) {
	g := newSubmitGuard(time.Hour)

	if !g.firstUse("tok-a") {
		t.Fatal("first use of a fresh token should be allowed")
	}
	if g.firstUse("tok-a") {
		t.Fatal("second use of the same token should be rejected")
	}
	if !g.firstUse("tok-b") {
		t.Fatal("a different token should be independent")
	}
}

// A double-submit races two requests carrying the same token. Exactly one must
// win; the guard is the only thing standing between them and a duplicate entry.
func TestSubmitGuardConcurrentSameToken(t *testing.T) {
	g := newSubmitGuard(time.Hour)

	const n = 100
	var wins int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if g.firstUse("same-token") {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("exactly one submission should win, got %d", wins)
	}
}
