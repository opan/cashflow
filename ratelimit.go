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
	// clientIPHeader is a forwarding header an operator has explicitly opted to
	// trust for the real client IP (e.g. "CF-Connecting-IP" behind Cloudflare).
	// Empty means trust nothing and use the direct connection address.
	clientIPHeader string
	client         *http.Client
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
		enabled:        enabled,
		url:            url,
		token:          token,
		clientIPHeader: strings.TrimSpace(env("AIO_RATELIMIT_CLIENT_IP_HEADER", "")),
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

// ClientIP resolves the caller's IP for ip-scoped buckets. Forwarding headers
// are trusted ONLY when an operator has named one via AIO_RATELIMIT_CLIENT_IP_HEADER
// (e.g. "CF-Connecting-IP" behind Cloudflare, or "X-Real-IP" behind a proxy that
// sets it authoritatively). That header must be one your edge sets from the real
// connection and does not pass through unverified from the client — otherwise a
// caller could spoof it to evade or misattribute the limit. Never point it at
// X-Forwarded-For, whose left-most entry is client-controlled.
//
// With no trusted header configured (or the header absent on a request) it falls
// back to the direct connection address (RemoteAddr): unspoofable, but behind a
// proxy every client collapses into one bucket — so set the header in any real
// deployment. cashflow resolves its own IP because aio only sees what we send.
func (rl *RateLimiter) ClientIP(r *http.Request) string {
	if rl != nil && rl.clientIPHeader != "" {
		if v := r.Header.Get(rl.clientIPHeader); v != "" {
			// Defensive: if a multi-value header slips through, take the first entry.
			return strings.TrimSpace(strings.SplitN(v, ",", 2)[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
