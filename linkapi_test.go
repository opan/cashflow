package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests need Postgres: set CASHFLOW_TEST_DATABASE_URL to a throwaway
// database (the schema is applied to it). Without it they are skipped.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("CASHFLOW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CASHFLOW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return &Store{pool: pool}
}

type linkFixture struct {
	app     *App
	srv     http.Handler
	plan    *CashPlan
	other   *CashPlan    // another user's cashplan
	cookie  *http.Cookie // the owner of plan, an aio admin
	cookie2 *http.Cookie // the owner of other, not an aio admin
}

func newLinkFixture(t *testing.T) *linkFixture {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	suffix := strings.ToLower(newToken()[:10])
	adminID := uuid.NewString()
	// aio says only adminID is an admin.
	aioSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admin := strings.Contains(r.URL.Path, "/"+adminID+"/")
		fmt.Fprintf(w, `{"success":true,"data":{"admin":%t,"blocked":false}}`, admin)
	}))
	t.Cleanup(aioSrv.Close)
	mkUser := func(name, aioID string, admin bool) (*User, *http.Cookie) {
		u, err := s.CreateAioUser(ctx, aioID, name+"-"+suffix)
		if err != nil {
			t.Fatal(err)
		}
		tok := newToken() + newToken()
		if err := s.CreateSession(ctx, tok, u.ID, time.Now().Add(time.Hour), "", admin); err != nil {
			t.Fatal(err)
		}
		return u, &http.Cookie{Name: sessionCookie, Value: tok}
	}
	u1, c1 := mkUser("owner", adminID, true)
	u2, c2 := mkUser("other", uuid.NewString(), false)
	plan, err := s.CreatePlan(ctx, u1.ID, "kas-"+suffix, "Kas Badminton Sampai Pingsan", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreatePlan(ctx, u2.ID, "lain-"+suffix, "Kas lain", "")
	if err != nil {
		t.Fatal(err)
	}
	app := &App{store: s, tmpl: buildTemplates(), guard: newSubmitGuard(time.Hour),
		aio: &AioAuth{enabled: true, internalURL: aioSrv.URL, httpClient: &http.Client{Timeout: time.Second}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /kelola/{slug}", app.handleManage)
	mux.HandleFunc("GET /kelola/{slug}/integrasi", app.handleIntegrasi)
	mux.HandleFunc("POST /kelola/{slug}/integrasi", app.handleCreateLinkToken)
	mux.HandleFunc("POST /kelola/{slug}/integrasi/{id}/revoke", app.handleRevokeLinkToken)
	mux.HandleFunc("GET /p/{slug}/entry/{id}", app.handleViewEntryDetail)
	mux.HandleFunc("POST /api/v1/plans/{slug}/entries", app.handleAPIAddEntry)
	return &linkFixture{app: app, srv: app.withUser(mux), plan: plan, other: other, cookie: c1, cookie2: c2}
}

func (f *linkFixture) do(t *testing.T, method, path string, body string, setup func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if setup != nil {
		setup(req)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func (f *linkFixture) form(t *testing.T, cookie *http.Cookie, path string, vals url.Values) *httptest.ResponseRecorder {
	return f.do(t, http.MethodPost, path, vals.Encode(), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(cookie)
	})
}

func (f *linkFixture) api(t *testing.T, slug, token, body string) (*httptest.ResponseRecorder, apiEnvelope) {
	t.Helper()
	rec := f.do(t, http.MethodPost, "/api/v1/plans/"+slug+"/entries", body, func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
	})
	var env apiEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("response is not JSON (%d): %s", rec.Code, rec.Body)
	}
	return rec, env
}

func apiData(t *testing.T, env apiEnvelope) apiEntryResponse {
	t.Helper()
	b, _ := json.Marshal(env.Data)
	var d apiEntryResponse
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func errCode(env apiEnvelope) string {
	if env.Error == nil {
		return ""
	}
	return *env.Error
}

func TestLinkTokenStore(t *testing.T) {
	f := newLinkFixture(t)
	ctx := context.Background()
	s := f.app.store

	tok, err := s.CreateLinkToken(ctx, f.plan.ID, "tally")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, linkTokenPrefix) || len(tok) < 50 {
		t.Fatalf("token %q looks wrong", tok)
	}
	var stored string
	if err := s.pool.QueryRow(ctx, `SELECT token_hash FROM link_tokens WHERE cashplan_id = $1`, f.plan.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == tok || stored != hashLinkToken(tok) {
		t.Fatal("only the token's hash should be stored")
	}
	got, err := s.ActiveLinkToken(ctx, tok)
	if err != nil || got.CashplanID != f.plan.ID || got.Name != "tally" {
		t.Fatalf("ActiveLinkToken = %+v, %v", got, err)
	}

	for i := 1; i < maxLinkTokens; i++ {
		if _, err := s.CreateLinkToken(ctx, f.plan.ID, "extra"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateLinkToken(ctx, f.plan.ID, "one too many"); err != ErrTooManyLinkTokens {
		t.Fatalf("token over the limit: err = %v", err)
	}

	if n, err := s.RevokeLinkToken(ctx, f.other.ID, got.ID); err != nil || n != 0 {
		t.Fatalf("revoking through another cashplan: n=%d err=%v", n, err)
	}
	if n, err := s.RevokeLinkToken(ctx, f.plan.ID, got.ID); err != nil || n != 1 {
		t.Fatalf("revoke: n=%d err=%v", n, err)
	}
	if _, err := s.ActiveLinkToken(ctx, tok); err == nil {
		t.Fatal("a revoked token still works")
	}
	if _, err := s.CreateLinkToken(ctx, f.plan.ID, "after revoke"); err != nil {
		t.Fatalf("revoking should free a slot: %v", err)
	}
}

func TestLinkAPIAddEntry(t *testing.T) {
	f := newLinkFixture(t)
	ctx := context.Background()
	s := f.app.store
	tok, err := s.CreateLinkToken(ctx, f.plan.ID, "tally")
	if err != nil {
		t.Fatal(err)
	}
	otherTok, err := s.CreateLinkToken(ctx, f.other.ID, "tally")
	if err != nil {
		t.Fatal(err)
	}
	slug := f.plan.Slug
	body := `{"key":"tally:42:1","party":"Mabar Minggu","amount":1050000,"date":"2026-11-02","description":"21 × Rp50.000 (18 tunai, 3 transfer)"}`

	t.Run("auth", func(t *testing.T) {
		cases := []struct {
			name, slug, token, code string
		}{
			{"no token", slug, "", "missing_token"},
			{"unknown token", slug, linkTokenPrefix + "nope", "invalid_token"},
			{"other cashplan's token", slug, otherTok, "invalid_token"},
			{"unknown cashplan", "no-such-plan", tok, "invalid_token"},
		}
		for _, c := range cases {
			rec, env := f.api(t, c.slug, c.token, body)
			if rec.Code != http.StatusUnauthorized || errCode(env) != c.code || env.Success {
				t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Errorf("%s: no WWW-Authenticate header", c.name)
			}
		}
	})

	t.Run("validation", func(t *testing.T) {
		cases := []struct{ name, body, code string }{
			{"not JSON", `amount=5`, "invalid_json"},
			{"unknown field", `{"key":"k1","party":"x","amount":5,"type":"expense"}`, "invalid_json"},
			{"no key", `{"party":"x","amount":5}`, "invalid_key"},
			{"key with a space", `{"key":"a b","party":"x","amount":5}`, "invalid_key"},
			{"zero amount", `{"key":"k1","party":"x","amount":0}`, "invalid_amount"},
			{"negative amount", `{"key":"k1","party":"x","amount":-5}`, "invalid_amount"},
			{"fractional amount", `{"key":"k1","party":"x","amount":5.5}`, "invalid_json"},
			{"no party", `{"key":"k1","party":"  ","amount":5}`, "invalid_party"},
			{"bad date", `{"key":"k1","party":"x","amount":5,"date":"2/11/2026"}`, "invalid_date"},
			{"long party", `{"key":"k1","party":"` + strings.Repeat("a", maxPartyLen+1) + `","amount":5}`, "invalid_party"},
		}
		for _, c := range cases {
			rec, env := f.api(t, slug, tok, c.body)
			if rec.Code != http.StatusBadRequest || errCode(env) != c.code {
				t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
			}
		}
		rec, env := f.api(t, slug, tok, `{"key":"k1","party":"x","amount":5,"description":"`+strings.Repeat("a", maxAPIBodyBytes)+`"}`)
		if rec.Code != http.StatusRequestEntityTooLarge || errCode(env) != "too_large" {
			t.Errorf("big body: %d %s", rec.Code, rec.Body)
		}
	})

	var first apiEntryResponse
	t.Run("created", func(t *testing.T) {
		rec, env := f.api(t, slug, tok, body)
		if rec.Code != http.StatusCreated || !env.Success {
			t.Fatalf("add: %d %s", rec.Code, rec.Body)
		}
		first = apiData(t, env)
		if !first.Created || first.Amount != 1050000 || first.Date != "2026-11-02" || first.ID == "" {
			t.Fatalf("data = %+v", first)
		}
		if !strings.HasSuffix(first.URL, "/p/"+slug+"/entry/"+first.ID) {
			t.Fatalf("url = %q", first.URL)
		}
		e, err := s.EntryByID(ctx, first.ID)
		if err != nil {
			t.Fatal(err)
		}
		if e.Type != "income" || e.Party != "Mabar Minggu" || e.Source != "tally" || e.CashplanID != f.plan.ID {
			t.Fatalf("entry = %+v", e)
		}
	})

	t.Run("retry adds nothing", func(t *testing.T) {
		rec, env := f.api(t, slug, tok, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("retry: %d %s", rec.Code, rec.Body)
		}
		if d := apiData(t, env); d.Created || d.ID != first.ID {
			t.Fatalf("retry data = %+v, want the first entry", d)
		}
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM entries WHERE cashplan_id = $1`, f.plan.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("%d entries after a retry, want 1", n)
		}
	})

	t.Run("same key, other amount", func(t *testing.T) {
		rec, env := f.api(t, slug, tok, strings.Replace(body, "1050000", "1100000", 1))
		if rec.Code != http.StatusConflict || errCode(env) != "key_conflict" {
			t.Fatalf("conflict: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("keys are per cashplan", func(t *testing.T) {
		rec, _ := f.api(t, f.other.Slug, otherTok, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("same key on another cashplan: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("no date means today", func(t *testing.T) {
		rec, env := f.api(t, slug, tok, `{"key":"tally:42:2","party":"Susulan","amount":50000}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if d := apiData(t, env); d.Date != time.Now().In(jakarta).Format("2006-01-02") {
			t.Fatalf("date = %q", d.Date)
		}
	})

	t.Run("source is locked", func(t *testing.T) {
		if _, err := s.pool.Exec(ctx, `UPDATE entries SET source_key = 'x' WHERE id = $1`, first.ID); err == nil {
			t.Fatal("source_key could be changed")
		}
		if _, err := s.pool.Exec(ctx, `UPDATE entries SET link_token_id = NULL WHERE id = $1`, first.ID); err == nil {
			t.Fatal("link_token_id could be changed")
		}
	})

	t.Run("detail names the integration", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/p/"+slug+"/entry/"+first.ID, "", nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "lewat integrasi <strong>tally</strong>") {
			t.Fatalf("detail: %d", rec.Code)
		}
	})

	t.Run("revoked", func(t *testing.T) {
		got, _ := s.ActiveLinkToken(ctx, tok)
		if _, err := s.RevokeLinkToken(ctx, f.plan.ID, got.ID); err != nil {
			t.Fatal(err)
		}
		rec, env := f.api(t, slug, tok, `{"key":"tally:42:3","party":"x","amount":5}`)
		if rec.Code != http.StatusUnauthorized || errCode(env) != "invalid_token" {
			t.Fatalf("revoked token: %d %s", rec.Code, rec.Body)
		}
	})
}

func TestIntegrasiPage(t *testing.T) {
	f := newLinkFixture(t)
	path := "/kelola/" + f.plan.Slug + "/integrasi"

	if rec := f.do(t, http.MethodGet, path, "", nil); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Fatalf("logged out: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := f.do(t, http.MethodGet, path, "", func(r *http.Request) { r.AddCookie(f.cookie2) }); rec.Code != http.StatusNotFound {
		t.Fatalf("another user's cashplan: %d", rec.Code)
	}
	// Not for owners who aren't aio admins: no page, no button, no tokens.
	otherPath := "/kelola/" + f.other.Slug + "/integrasi"
	if rec := f.do(t, http.MethodGet, otherPath, "", func(r *http.Request) { r.AddCookie(f.cookie2) }); rec.Code != http.StatusNotFound {
		t.Fatalf("owner who isn't an aio admin: %d", rec.Code)
	}
	if rec := f.form(t, f.cookie2, otherPath, url.Values{"name": {"tally"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("token created by an owner who isn't an aio admin: %d", rec.Code)
	}
	if rec := f.do(t, http.MethodGet, "/kelola/"+f.other.Slug, "", func(r *http.Request) { r.AddCookie(f.cookie2) }); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "/integrasi") {
		t.Fatalf("manage page for a non-admin: %d, shows Integrasi: %t", rec.Code, strings.Contains(rec.Body.String(), "/integrasi"))
	}
	// An aio admin still needs to own the cashplan.
	if rec := f.do(t, http.MethodGet, otherPath, "", func(r *http.Request) { r.AddCookie(f.cookie) }); rec.Code != http.StatusNotFound {
		t.Fatalf("admin on someone else's cashplan: %d", rec.Code)
	}
	if rec := f.do(t, http.MethodGet, "/kelola/"+f.plan.Slug, "", func(r *http.Request) { r.AddCookie(f.cookie) }); !strings.Contains(rec.Body.String(), path) {
		t.Fatal("manage page for an admin doesn't show Integrasi")
	}

	rec := f.form(t, f.cookie, path, url.Values{"name": {"tally"}, "form_token": {"ft1"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d", rec.Code)
	}
	page := rec.Body.String()
	i := strings.Index(page, `value="`+linkTokenPrefix)
	if i < 0 {
		t.Fatal("the new token is not shown")
	}
	token := page[i+len(`value="`):]
	token = token[:strings.IndexByte(token, '"')]
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the page with the token may be cached")
	}
	if _, err := f.app.store.ActiveLinkToken(context.Background(), token); err != nil {
		t.Fatalf("shown token doesn't work: %v", err)
	}

	if rec := f.form(t, f.cookie, path, url.Values{"name": {"tally"}, "form_token": {"ft1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("resent form: %d, want a redirect without a new token", rec.Code)
	}
	if rec := f.form(t, f.cookie, path, url.Values{"name": {" "}}); !strings.Contains(rec.Body.String(), "wajib diisi") {
		t.Fatal("empty name accepted")
	}

	rec = f.do(t, http.MethodGet, path, "", func(r *http.Request) { r.AddCookie(f.cookie) })
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), token) {
		t.Fatal("the token is shown again after creating it")
	}
	if !strings.Contains(rec.Body.String(), "/api/v1/plans/"+f.plan.Slug+"/entries") {
		t.Fatal("the endpoint is not shown")
	}

	tokens, err := f.app.store.LinkTokens(context.Background(), f.plan.ID)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("tokens = %v, %v", tokens, err)
	}
	revoke := path + "/" + tokens[0].ID + "/revoke"
	if rec := f.form(t, f.cookie2, revoke, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("revoke by another user: %d", rec.Code)
	}
	if rec := f.form(t, f.cookie, revoke, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if _, err := f.app.store.ActiveLinkToken(context.Background(), token); err == nil {
		t.Fatal("token still works after revoke")
	}
}
