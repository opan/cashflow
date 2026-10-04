package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

// AioAuth logs users in through all-in-one (OpenID Connect). It is used only
// when AUTH_PROVIDER=aio; otherwise cashflow keeps its own username/password
// login exactly as before.
//
// Unlike the rate limiter, authentication fails closed: if aio is unreachable
// nobody new can log in. Existing cashflow sessions keep working, since every
// request after login uses cashflow's own session cookie.
type AioAuth struct {
	enabled       bool
	issuer        string
	clientID      string
	clientSecret  string
	redirectURL   string
	postLogoutURL string

	mu            sync.Mutex
	oauth         oauth2.Config
	verifier      *oidc.IDTokenVerifier
	endSessionURL string
	ready         bool
}

const (
	oidcStateCookie = "cashflow_oidc"
	oidcStateTTL    = 10 * time.Minute
)

var errUnlinkedAccount = errors.New("username belongs to a local cashflow account")

// NewAioAuthFromEnv reads AUTH_PROVIDER and the AIO_* settings. A half-done
// configuration is a startup error rather than a silent fallback, so a typo
// can't leave cashflow running without the login the operator expected.
func NewAioAuthFromEnv() (*AioAuth, error) {
	if strings.ToLower(strings.TrimSpace(env("AUTH_PROVIDER", "local"))) != "aio" {
		return &AioAuth{}, nil
	}
	a := &AioAuth{
		enabled:       true,
		issuer:        strings.TrimRight(strings.TrimSpace(env("AIO_ISSUER", "")), "/"),
		clientID:      strings.TrimSpace(env("AIO_CLIENT_ID", "")),
		clientSecret:  strings.TrimSpace(env("AIO_CLIENT_SECRET", "")),
		redirectURL:   strings.TrimSpace(env("AIO_REDIRECT_URL", "")),
		postLogoutURL: strings.TrimSpace(env("AIO_POST_LOGOUT_URL", "")),
	}
	var missing []string
	for k, v := range map[string]string{"AIO_ISSUER": a.issuer, "AIO_CLIENT_ID": a.clientID,
		"AIO_CLIENT_SECRET": a.clientSecret, "AIO_REDIRECT_URL": a.redirectURL} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("AUTH_PROVIDER=aio requires %s", strings.Join(missing, ", "))
	}
	return a, nil
}

func (a *AioAuth) Enabled() bool { return a != nil && a.enabled }

// origin is aio's scheme://host[:port] ("" when disabled), for the CSP.
func (a *AioAuth) origin() string {
	if !a.Enabled() {
		return ""
	}
	u, err := url.Parse(a.issuer)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// discover fetches aio's discovery document and keys on first use. If aio is
// down it returns an error and is retried on the next login attempt, so
// cashflow can start (and serve logged-in users) while aio is unavailable.
func (a *AioAuth) discover(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ready {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	p, err := oidc.NewProvider(ctx, a.issuer)
	if err != nil {
		return fmt.Errorf("aio discovery: %w", err)
	}
	var extra struct {
		EndSession string `json:"end_session_endpoint"`
	}
	_ = p.Claims(&extra)
	a.oauth = oauth2.Config{
		ClientID:     a.clientID,
		ClientSecret: a.clientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  a.redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile"},
	}
	a.verifier = p.Verifier(&oidc.Config{ClientID: a.clientID})
	a.endSessionURL = extra.EndSession
	a.ready = true
	return nil
}

// loginState travels in a short-lived HttpOnly cookie between /auth/login and
// /auth/callback: the values the callback must match (state, nonce), the PKCE
// verifier, and where to land afterwards.
type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"x,omitempty"`
}

func (st loginState) encode() string {
	b, _ := json.Marshal(st)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeLoginState(raw string) (loginState, error) {
	var st loginState
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(b, &st)
	return st, err
}

// safeNext keeps only same-site paths, so ?next= can't redirect off-site.
func safeNext(raw string) string {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/\\") {
		return ""
	}
	return raw
}

// localUsername derives cashflow's username from aio's preferred_username,
// keeping cashflow's own rules (lowercase letters, digits, underscore, 3-30).
func localUsername(preferred, sub string) string {
	var b strings.Builder
	for _, r := range normalizeUsername(preferred) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	name := b.String()
	if len(name) > 30 {
		name = name[:30]
	}
	if len(name) < 3 {
		name = "user_" + shortID(sub)
	}
	return name
}

func shortID(sub string) string {
	s := strings.ReplaceAll(sub, "-", "")
	if len(s) > 8 {
		s = s[:8]
	}
	return strings.ToLower(s)
}

func (a *App) setLoginStateCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: oidcStateCookie, Value: value, Path: "/auth", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r),
	})
}

func (a *App) authError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	a.renderStatus(w, r, "autherror", struct{ Message string }{msg}, status)
}

// handleAuthLogin starts a login (or, with ?signup=1, a registration) at aio.
func (a *App) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !a.aio.Enabled() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if currentUser(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := a.aio.discover(r.Context()); err != nil {
		log.Printf("aio login: %v", err)
		a.authError(w, r, http.StatusServiceUnavailable, "Layanan masuk sedang tidak tersedia. Coba lagi beberapa saat lagi.")
		return
	}
	st := loginState{
		State: newToken(), Nonce: newToken(), Verifier: oauth2.GenerateVerifier(),
		Next: safeNext(r.URL.Query().Get("next")),
	}
	a.setLoginStateCookie(w, r, st.encode(), int(oidcStateTTL.Seconds()))

	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(st.Verifier), oidc.Nonce(st.Nonce)}
	if r.URL.Query().Get("signup") == "1" {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "create"))
	}
	http.Redirect(w, r, a.aio.oauth.AuthCodeURL(st.State, opts...), http.StatusFound)
}

// handleAuthCallback finishes a login: checks state, exchanges the code with
// the client secret and PKCE verifier, verifies the ID token (signature,
// issuer, audience, expiry, nonce), then logs the linked local user in.
func (a *App) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	if !a.aio.Enabled() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	c, err := r.Cookie(oidcStateCookie)
	if err != nil {
		http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
		return
	}
	a.setLoginStateCookie(w, r, "", -1)
	st, err := decodeLoginState(c.Value)
	q := r.URL.Query()
	if err != nil || st.State == "" || q.Get("state") != st.State {
		a.authError(w, r, http.StatusBadRequest, "Sesi masuk tidak valid atau sudah kedaluwarsa.")
		return
	}
	if e := q.Get("error"); e != "" {
		log.Printf("aio login: aio returned %s: %s", e, q.Get("error_description"))
		a.authError(w, r, http.StatusBadRequest, "Proses masuk dibatalkan atau ditolak.")
		return
	}
	if err := a.aio.discover(r.Context()); err != nil {
		log.Printf("aio login: %v", err)
		a.authError(w, r, http.StatusServiceUnavailable, "Layanan masuk sedang tidak tersedia. Coba lagi beberapa saat lagi.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	tok, err := a.aio.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		log.Printf("aio login: code exchange: %v", err)
		a.authError(w, r, http.StatusBadGateway, "Gagal menyelesaikan proses masuk. Silakan coba lagi.")
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := a.aio.verifier.Verify(ctx, rawID)
	if err != nil || idt.Nonce != st.Nonce {
		log.Printf("aio login: id token rejected: %v (nonce match: %v)", err, idt != nil && idt.Nonce == st.Nonce)
		a.authError(w, r, http.StatusBadRequest, "Gagal memverifikasi identitas. Silakan coba lagi.")
		return
	}
	var claims struct {
		PreferredUsername string `json:"preferred_username"`
	}
	_ = idt.Claims(&claims)

	u, err := a.linkedUser(ctx, idt.Subject, claims.PreferredUsername)
	if errors.Is(err, errUnlinkedAccount) {
		a.authError(w, r, http.StatusConflict, "Nama pengguna ini sudah dipakai akun cashflow lama. Hubungi admin untuk menghubungkannya dengan akun all-in-one kamu.")
		return
	}
	if err != nil {
		log.Printf("aio login: link user: %v", err)
		http.Error(w, "gagal masuk", http.StatusInternalServerError)
		return
	}
	if err := a.startSession(w, r, u.ID, rawID); err != nil {
		log.Printf("aio login: start session: %v", err)
		http.Error(w, "gagal masuk", http.StatusInternalServerError)
		return
	}
	next := st.Next
	if next == "" {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// linkedUser returns the local user for an aio account, creating it on first
// login. A username that belongs to an existing local-only account is never
// taken over: that needs an explicit link (RFC-001 §7.3).
func (a *App) linkedUser(ctx context.Context, sub, preferred string) (*User, error) {
	u, err := a.store.UserByAioID(ctx, sub)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	name := localUsername(preferred, sub)
	u, err = a.store.CreateAioUser(ctx, sub, name)
	if !errors.Is(err, ErrUsernameTaken) {
		return u, err
	}
	_, ownerAioID, err := a.store.UsernameOwner(ctx, name)
	if err != nil {
		return nil, err
	}
	if ownerAioID == "" {
		return nil, errUnlinkedAccount
	}
	// Taken by another aio account (e.g. a renamed one): keep both distinct.
	return a.store.CreateAioUser(ctx, sub, name+"_"+shortID(sub))
}

// aioLogoutURL is where to send the browser after ending the local session,
// so aio ends its session too. Empty when there is nothing to tell aio.
func (a *AioAuth) logoutURL(idToken string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.ready || a.endSessionURL == "" || idToken == "" {
		return ""
	}
	q := url.Values{"id_token_hint": {idToken}, "client_id": {a.clientID}}
	if a.postLogoutURL != "" {
		q.Set("post_logout_redirect_uri", a.postLogoutURL)
	}
	return a.endSessionURL + "?" + q.Encode()
}
