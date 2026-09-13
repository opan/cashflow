package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIP(t *testing.T) {
	cases := []struct {
		name       string
		header     string // configured trusted header (rl.clientIPHeader)
		reqHeaders map[string]string
		remoteAddr string
		want       string
	}{
		{"no trusted header ignores XFF, uses RemoteAddr", "", map[string]string{"X-Forwarded-For": "9.9.9.9"}, "10.0.0.1:5555", "10.0.0.1"},
		{"trusted header used", "CF-Connecting-IP", map[string]string{"CF-Connecting-IP": "203.0.113.7"}, "10.0.0.1:5555", "203.0.113.7"},
		{"spoofed XFF ignored when trusting CF header", "CF-Connecting-IP", map[string]string{"X-Forwarded-For": "1.2.3.4", "CF-Connecting-IP": "203.0.113.7"}, "10.0.0.1:5555", "203.0.113.7"},
		{"trusted header absent falls back to RemoteAddr", "CF-Connecting-IP", map[string]string{}, "10.0.0.1:5555", "10.0.0.1"},
		{"trusted header empty falls back to RemoteAddr", "CF-Connecting-IP", map[string]string{"CF-Connecting-IP": ""}, "10.0.0.1:5555", "10.0.0.1"},
		{"multi-value trusted header takes first", "X-Real-IP", map[string]string{"X-Real-IP": "203.0.113.7, 10.0.0.2"}, "10.0.0.1:5555", "203.0.113.7"},
		{"ipv6 RemoteAddr", "", map[string]string{}, "[2001:db8::1]:5555", "2001:db8::1"},
		{"RemoteAddr without port", "", map[string]string{}, "10.0.0.9", "10.0.0.9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rl := &RateLimiter{clientIPHeader: c.header}
			r := httptest.NewRequest(http.MethodGet, "/p/x", nil)
			r.RemoteAddr = c.remoteAddr
			for k, v := range c.reqHeaders {
				r.Header.Set(k, v)
			}
			if got := rl.ClientIP(r); got != c.want {
				t.Errorf("ClientIP = %q, want %q", got, c.want)
			}
		})
	}
}

func TestClientIP_NilReceiver(t *testing.T) {
	var rl *RateLimiter
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.2.3:80"
	if got := rl.ClientIP(r); got != "10.1.2.3" {
		t.Errorf("nil ClientIP = %q, want 10.1.2.3", got)
	}
}

func TestAllow_Disabled(t *testing.T) {
	rl := &RateLimiter{enabled: false}
	if ok, ra := rl.Allow(context.Background(), "t", "b"); !ok || ra != 0 {
		t.Fatalf("disabled Allow = (%v,%d), want (true,0)", ok, ra)
	}
}

func TestAllow_AllowedAndDenied(t *testing.T) {
	var gotBody checkRequest
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if gotBody.BucketKey == "ip:blocked" {
			io.WriteString(w, `{"data":{"allowed":false,"retry_after_seconds":42}}`)
			return
		}
		io.WriteString(w, `{"data":{"allowed":true,"retry_after_seconds":0}}`)
	}))
	defer srv.Close()
	rl := &RateLimiter{enabled: true, url: srv.URL, token: "secret", client: srv.Client()}

	if ok, ra := rl.Allow(context.Background(), "act", "ip:ok"); !ok || ra != 0 {
		t.Errorf("allowed case = (%v,%d), want (true,0)", ok, ra)
	}
	ok, ra := rl.Allow(context.Background(), "act", "ip:blocked")
	if ok || ra != 42 {
		t.Errorf("denied case = (%v,%d), want (false,42)", ok, ra)
	}
	if gotKey != "secret" {
		t.Errorf("X-API-Key = %q, want secret", gotKey)
	}
	if gotBody.TargetKey != "act" || gotBody.BucketKey != "ip:blocked" {
		t.Errorf("last request body = %+v, want target=act bucket=ip:blocked", gotBody)
	}
}

func TestAllow_FailOpen(t *testing.T) {
	srv503 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv503.Close()
	srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `not json`)
	}))
	defer srvBad.Close()

	cases := []struct{ name, url string }{
		{"non-200 fails open", srv503.URL},
		{"bad json fails open", srvBad.URL},
		{"transport error fails open", "http://127.0.0.1:1"}, // connection refused
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rl := &RateLimiter{enabled: true, url: c.url, token: "t", client: &http.Client{Timeout: time.Second}}
			if ok, ra := rl.Allow(context.Background(), "act", "b"); !ok || ra != 0 {
				t.Errorf("Allow = (%v,%d), want fail-open (true,0)", ok, ra)
			}
		})
	}
}

func TestNewRateLimiterFromEnv(t *testing.T) {
	t.Setenv("AIO_RATELIMIT_URL", "http://aio/check")
	t.Setenv("AIO_RATELIMIT_TOKEN", "tok")

	t.Setenv("AIO_RATELIMIT_ENABLED", "false")
	if NewRateLimiterFromEnv().Enabled() {
		t.Error("want disabled when flag is off")
	}

	t.Setenv("AIO_RATELIMIT_ENABLED", "true")
	t.Setenv("AIO_RATELIMIT_TOKEN", "")
	if NewRateLimiterFromEnv().Enabled() {
		t.Error("want disabled when token missing")
	}

	t.Setenv("AIO_RATELIMIT_TOKEN", "tok")
	t.Setenv("AIO_RATELIMIT_CLIENT_IP_HEADER", "CF-Connecting-IP")
	rl := NewRateLimiterFromEnv()
	if !rl.Enabled() {
		t.Error("want enabled when flag+url+token set")
	}
	if rl.clientIPHeader != "CF-Connecting-IP" {
		t.Errorf("clientIPHeader = %q, want CF-Connecting-IP", rl.clientIPHeader)
	}
}

func TestIsTruthy(t *testing.T) {
	for _, s := range []string{"1", "true", "TRUE", "Yes", " on "} {
		if !isTruthy(s) {
			t.Errorf("isTruthy(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "0", "false", "no", "off", "maybe"} {
		if isTruthy(s) {
			t.Errorf("isTruthy(%q) = true, want false", s)
		}
	}
}
