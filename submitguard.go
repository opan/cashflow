package main

import (
	"sync"
	"time"
)

// submitGuard makes form submissions idempotent: a single rendered form carries
// a one-time token, and only the first submission bearing a given token is
// processed. This defeats duplicate entries from a double-click or a resent POST
// (common when a slow network makes the page appear to hang).
//
// State is kept in memory and cleared on restart. The residual gap is therefore
// narrow: a token can only be replayed successfully if the very same form is
// resubmitted across a process restart, which briefly loses dedup for that one
// submission. A durable (DB-backed) backstop would close that gap entirely; it
// is deliberately deferred until monitoring shows it is needed.
type submitGuard struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
}

// newSubmitGuard returns a guard that remembers consumed tokens for ttl and
// sweeps expired ones in the background. ttl only needs to outlast the realistic
// lifetime of an open form; entries are recorded per submission, not per render,
// so the map stays small.
func newSubmitGuard(ttl time.Duration) *submitGuard {
	g := &submitGuard{seen: make(map[string]time.Time), ttl: ttl}
	go g.gc()
	return g
}

// firstUse records token and reports whether this is its first use. A return of
// false means the token was already consumed, i.e. this is a duplicate submit.
func (g *submitGuard) firstUse(token string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.seen[token]; ok {
		return false
	}
	g.seen[token] = time.Now()
	return true
}

func (g *submitGuard) gc() {
	t := time.NewTicker(g.ttl)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-g.ttl)
		g.mu.Lock()
		for k, seenAt := range g.seen {
			if seenAt.Before(cutoff) {
				delete(g.seen, k)
			}
		}
		g.mu.Unlock()
	}
}
