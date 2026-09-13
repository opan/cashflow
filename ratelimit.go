package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// RateLimiter calls all-in-one's external rate-limit check API. It is fail-open
// by design: any error, timeout, non-200 response, or the feature being
// disabled yields allowed=true, so an aio outage — or the flag being off —
// never blocks cashflow.
type RateLimiter struct {
	enabled bool
	url     string
	token   string
	client  *http.Client
}

// NewRateLimiterFromEnv builds the limiter from AIO_RATELIMIT_* env vars. It is
// enabled only when AIO_RATELIMIT_ENABLED is truthy AND both the URL and token
// are set; otherwise it is a no-op that always allows (identical behavior to
// before this feature existed).
func NewRateLimiterFromEnv() *RateLimiter {
	url := strings.TrimSpace(env("AIO_RATELIMIT_URL", ""))
	token := strings.TrimSpace(env("AIO_RATELIMIT_TOKEN", ""))
	enabled := isTruthy(env("AIO_RATELIMIT_ENABLED", "")) && url != "" && token != ""
	return &RateLimiter{
		enabled: enabled,
		url:     url,
		token:   token,
		// A tight timeout keeps a slow/unreachable aio off the request hot path;
		// on timeout we fail open.
		client: &http.Client{Timeout: 250 * time.Millisecond},
	}
}

// Enabled reports whether the limiter will actually call aio.
func (rl *RateLimiter) Enabled() bool { return rl != nil && rl.enabled }

type checkRequest struct {
	TargetKey string `json:"target_key"`
	BucketKey string `json:"bucket_key"`
}

type checkEnvelope struct {
	Data struct {
		Allowed           bool `json:"allowed"`
		RetryAfterSeconds int  `json:"retry_after_seconds"`
	} `json:"data"`
}

// Allow reports whether an action may proceed, plus the seconds to wait on a
// denial (0 otherwise). It fails open — returns (true, 0) whenever the limiter
// is disabled or aio cannot give a clear answer — and never retries, because
// /check increments as a side effect and a retry would double-charge the quota.
func (rl *RateLimiter) Allow(ctx context.Context, targetKey, bucketKey string) (allowed bool, retryAfter int) {
	if !rl.Enabled() {
		return true, 0
	}

	body, err := json.Marshal(checkRequest{TargetKey: targetKey, BucketKey: bucketKey})
	if err != nil {
		return true, 0
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rl.url, bytes.NewReader(body))
	if err != nil {
		return true, 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", rl.token)

	resp, err := rl.client.Do(req)
	if err != nil {
		log.Printf("ratelimit: check failed, failing open: %v", err)
		return true, 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 503 (external limiting disabled on aio), 401 (bad token), or any 5xx:
		// fail open rather than block a real user on a misconfiguration.
		log.Printf("ratelimit: check returned %d, failing open", resp.StatusCode)
		return true, 0
	}

	var env checkEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		log.Printf("ratelimit: decode failed, failing open: %v", err)
		return true, 0
	}
	return env.Data.Allowed, env.Data.RetryAfterSeconds
}

func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// clientIP resolves the caller's IP for ip-scoped buckets. It prefers the
// left-most X-Forwarded-For entry, then X-Real-IP, then RemoteAddr — cashflow
// resolves its own IP because aio only ever sees the string cashflow sends.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.SplitN(xff, ",", 2)[0])
	}
	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		return strings.TrimSpace(xrip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
