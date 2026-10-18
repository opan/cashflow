package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUserStatus(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    aioUserStatus
		wantErr bool
	}{
		{"admin", 200, `{"success":true,"data":{"admin":true,"blocked":false}}`, aioUserStatus{Admin: true}, false},
		{"not admin", 200, `{"success":true,"data":{"admin":false,"blocked":false}}`, aioUserStatus{}, false},
		{"blocked is never admin", 200, `{"success":true,"data":{"admin":true,"blocked":true}}`, aioUserStatus{Blocked: true}, false},
		{"unknown user", 404, `{"success":false,"error":"user not found"}`, aioUserStatus{}, false},
		{"wrong credentials", 401, `{"success":false}`, aioUserStatus{}, true},
		{"aio error", 500, `{"success":false}`, aioUserStatus{}, true},
		{"unsuccessful body", 200, `{"success":false}`, aioUserStatus{}, true},
		{"garbage body", 200, `not json`, aioUserStatus{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotPath, gotUser, gotPass string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.EscapedPath()
				gotUser, gotPass, _ = r.BasicAuth()
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			a := &AioAuth{enabled: true, clientID: "cashflow", clientSecret: "s3cret",
				internalURL: srv.URL, httpClient: &http.Client{Timeout: time.Second}}
			got, err := a.userStatus(context.Background(), "6f1c2a0e-8f4b-4f51-9a39-0d6f3d1e2b7a")
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("status = %+v, want %+v", got, c.want)
			}
			if gotPath != "/api/v1/oidc/users/6f1c2a0e-8f4b-4f51-9a39-0d6f3d1e2b7a/status" {
				t.Errorf("path = %q", gotPath)
			}
			if gotUser != "cashflow" || gotPass != "s3cret" {
				t.Errorf("basic auth = %q/%q", gotUser, gotPass)
			}
		})
	}
}

func TestUserStatus_NonUUIDSubNeverReachesAio(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()

	a := &AioAuth{enabled: true, internalURL: srv.URL, httpClient: &http.Client{Timeout: time.Second}}
	for _, sub := range []string{"../../clients", "budi", "", "6f1c2a0e-8f4b-4f51-9a39-0d6f3d1e2b7a/../x"} {
		got, err := a.userStatus(context.Background(), sub)
		if err != nil || got.Admin {
			t.Errorf("sub %q: status = %+v, err = %v; want not admin, no error", sub, got, err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("aio was called %d times for non-UUID subs", n)
	}
}

func TestUserStatus_TimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	a := &AioAuth{enabled: true, internalURL: srv.URL, httpClient: &http.Client{Timeout: 50 * time.Millisecond}}
	if _, err := a.userStatus(context.Background(), "6f1c2a0e-8f4b-4f51-9a39-0d6f3d1e2b7a"); err == nil {
		t.Fatal("expected a timeout error")
	}
}

func TestInternalURLFallsBackToIssuer(t *testing.T) {
	base := map[string]string{"AUTH_PROVIDER": "aio", "AIO_ISSUER": "https://aio.example.com/",
		"AIO_CLIENT_ID": "cashflow", "AIO_CLIENT_SECRET": "x", "AIO_REDIRECT_URL": "https://cf.example.com/auth/callback"}
	for _, c := range []struct{ internal, want string }{
		{"", "https://aio.example.com"},
		{"http://all-in-one.app.svc.cluster.local/", "http://all-in-one.app.svc.cluster.local"},
	} {
		for k, v := range base {
			t.Setenv(k, v)
		}
		t.Setenv("AIO_INTERNAL_URL", c.internal)
		a, err := NewAioAuthFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if a.internalURL != c.want {
			t.Errorf("AIO_INTERNAL_URL=%q: internalURL = %q, want %q", c.internal, a.internalURL, c.want)
		}
	}
}

func TestAdminTemplatesRender(t *testing.T) {
	tmpl := buildTemplates()
	last := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	created := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		data    any
		want    []string
		notWant []string
	}{
		{"admin", adminUsersVM{Users: []AdminUserRow{
			{User: User{ID: "u1", Username: "budi", CreatedAt: created, AioUserID: "a1"}, PlanCount: 2, EntryCount: 7, LastActivity: &last},
			{User: User{ID: "u2", Username: "lama", CreatedAt: created}},
		}}, []string{"budi", "/admin/users/u1", "1 Okt 2026", "akun lokal"}, []string{"Rp"}},
		{"adminuser", adminUserVM{User: &User{ID: "u1", Username: "budi", CreatedAt: created, AioUserID: "a1"},
			Plans: []PlanCard{{Plan: CashPlan{Slug: "kas-3a", Title: "Kas 3A", CreatedAt: created},
				Summary: Summary{TotalIncome: 150000, TotalExpense: 50000, Balance: 100000, EntryCount: 3}}}},
			[]string{"budi", "/p/kas-3a", "Kas 3A", "Rp"}, nil},
		{"adminerror", nil, []string{"all-in-one sedang tidak terjangkau"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tmpl[c.name].ExecuteTemplate(&buf, "layout", pageData{Username: "admin", IsAdmin: true, Data: c.data}); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			for _, s := range append(c.want, `href="/admin"`) {
				if !strings.Contains(out, s) {
					t.Errorf("missing %q", s)
				}
			}
			for _, s := range c.notWant {
				if strings.Contains(out, s) {
					t.Errorf("overview must not show %q", s)
				}
			}
		})
	}
}
