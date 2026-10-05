package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLocalUsername(t *testing.T) {
	cases := []struct{ preferred, want string }{
		{"budi", "budi"},
		{"Budi", "budi"},
		{"budi.santoso", "budi_santoso"},
		{"  ana  ", "ana"},
		{"ab", "user_a1b2c3d4"},
		{"", "user_a1b2c3d4"},
		{strings.Repeat("x", 40), strings.Repeat("x", 30)},
	}
	for _, c := range cases {
		if got := localUsername(c.preferred, "a1b2c3d4-e5f6-0000-0000-000000000000"); got != c.want {
			t.Errorf("localUsername(%q) = %q, want %q", c.preferred, got, c.want)
		}
	}
}

func TestSuffixedUsername(t *testing.T) {
	sub := "a1b2c3d4-e5f6-0000-0000-000000000000"
	if got := suffixedUsername("budi", sub); got != "budi_a1b2c3d4" {
		t.Errorf("short name: got %q", got)
	}
	got := suffixedUsername(strings.Repeat("x", 30), sub)
	if want := strings.Repeat("x", 21) + "_a1b2c3d4"; got != want {
		t.Errorf("long name: got %q, want %q", got, want)
	}
	if err := validateUsername(got); err != nil {
		t.Errorf("suffixed name %q breaks cashflow's own rules: %v", got, err)
	}
}

func TestNextOrHome(t *testing.T) {
	for in, want := range map[string]string{"": "/", "/kelola/kas": "/kelola/kas", "//evil.example.com": "/", "/\t/evil.example.com": "/"} {
		if got := nextOrHome(in); got != want {
			t.Errorf("nextOrHome(%q) = %q, want %q", in, got, want)
		}
	}
}

// While aio is down, each login must wait only for its own discovery
// attempt, not for every attempt queued ahead of it.
func TestDiscover_ConcurrentAttemptsDontQueue(t *testing.T) {
	const delay = 300 * time.Millisecond
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer slow.Close()
	a := &AioAuth{enabled: true, issuer: slow.URL}

	const n = 5
	start := time.Now()
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.discover(context.Background()); err == nil {
				t.Error("discovery against a failing aio must fail")
			}
		}()
	}
	wg.Wait()
	if elapsed := time.Since(start); elapsed > 3*delay {
		t.Errorf("%d concurrent attempts took %v; they are queueing behind one another", n, elapsed)
	}
}

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"/kelola/kas":          "/kelola/kas",
		"/":                    "/",
		"":                     "",
		"//evil.example.com":   "",
		"/\\evil.example.com":  "",
		"https://evil.example": "",
		"kelola/kas":           "",
		// Browsers drop tabs/newlines and read "\\" as "/", so these are "//evil.example.com".
		"/\t/evil.example.com": "",
		"/\n/evil.example.com": "",
		"/\r/evil.example.com": "",
		"/a\\b":                "",
		"/\x7f":                "",
		"/kelola?tab=1#x":      "/kelola?tab=1#x",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoginStateRoundTrip(t *testing.T) {
	st := loginState{State: "s", Nonce: "n", Verifier: "v", Next: "/kelola/kas"}
	got, err := decodeLoginState(st.encode())
	if err != nil || got != st {
		t.Fatalf("round trip = %+v, %v; want %+v", got, err, st)
	}
	if _, err := decodeLoginState("not base64!"); err == nil {
		t.Error("garbage must not decode")
	}
}

func TestNewAioAuthFromEnv(t *testing.T) {
	t.Run("local by default", func(t *testing.T) {
		t.Setenv("AUTH_PROVIDER", "")
		a, err := NewAioAuthFromEnv()
		if err != nil || a.Enabled() {
			t.Fatalf("want disabled, got enabled=%v err=%v", a.Enabled(), err)
		}
	})
	t.Run("aio without settings is a startup error", func(t *testing.T) {
		t.Setenv("AUTH_PROVIDER", "aio")
		t.Setenv("AIO_ISSUER", "http://localhost:18080")
		_, err := NewAioAuthFromEnv()
		if err == nil || !strings.Contains(err.Error(), "AIO_CLIENT_ID") {
			t.Fatalf("want error naming the missing settings, got %v", err)
		}
	})
	t.Run("aio fully configured", func(t *testing.T) {
		t.Setenv("AUTH_PROVIDER", "aio")
		t.Setenv("AIO_ISSUER", "http://localhost:18080/")
		t.Setenv("AIO_CLIENT_ID", "cashflow")
		t.Setenv("AIO_CLIENT_SECRET", "s")
		t.Setenv("AIO_REDIRECT_URL", "http://localhost:8090/auth/callback")
		a, err := NewAioAuthFromEnv()
		if err != nil || !a.Enabled() || a.issuer != "http://localhost:18080" {
			t.Fatalf("got enabled=%v issuer=%q err=%v", a.Enabled(), a.issuer, err)
		}
	})
}

func TestLogoutURL(t *testing.T) {
	a := &AioAuth{clientID: "cashflow", postLogoutURL: "http://localhost:8090/",
		ready: true, endSessionURL: "http://aio/api/v1/oauth2/end_session"}
	u, err := url.Parse(a.logoutURL("the-id-token"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("id_token_hint") != "the-id-token" || q.Get("post_logout_redirect_uri") != "http://localhost:8090/" || q.Get("client_id") != "cashflow" {
		t.Errorf("unexpected logout URL %s", u)
	}
	if a.logoutURL("") != "" {
		t.Error("no ID token (a local session) means nothing to tell aio")
	}
}

func TestSecurityHeaders_FormActionAllowsAioOnlyWhenEnabled(t *testing.T) {
	csp := func(a *AioAuth) string {
		rr := httptest.NewRecorder()
		securityHeaders(a.origin(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
			ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		return rr.Header().Get("Content-Security-Policy")
	}
	if got := csp(&AioAuth{}); !strings.Contains(got, "form-action 'self';") {
		t.Errorf("local login keeps form-action 'self' only, got %q", got)
	}
	got := csp(&AioAuth{enabled: true, issuer: "https://auth.example.com"})
	if !strings.Contains(got, "form-action 'self' https://auth.example.com;") {
		t.Errorf("aio login must allow the logout redirect to aio, got %q", got)
	}
}
