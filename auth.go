package main

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type ctxKey int

const userCtxKey ctxKey = iota

const (
	sessionCookie = "cashflow_session"
	sessionTTL    = 30 * 24 * time.Hour
)

func hashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

func checkPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

var usernameRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// normalizeUsername lowercases and trims, so "Budi" and "budi" are the same
// account (and collide on the first-come-first-served unique constraint).
func normalizeUsername(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func validateUsername(s string) error {
	switch {
	case len(s) < 3:
		return errors.New("minimal 3 karakter")
	case len(s) > 30:
		return errors.New("maksimal 30 karakter")
	case !usernameRe.MatchString(s):
		return errors.New("hanya boleh huruf kecil, angka, dan garis bawah (_)")
	}
	return nil
}

// withUser loads the current user (if any) from the session cookie into context.
func (a *App) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			if u, err := a.store.UserBySession(r.Context(), c.Value); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), userCtxKey, u))
			}
		}
		next.ServeHTTP(w, r)
	})
}

func currentUser(r *http.Request) *User {
	u, _ := r.Context().Value(userCtxKey).(*User)
	return u
}

func (a *App) startSession(w http.ResponseWriter, r *http.Request, userID string) error {
	token := newToken() + newToken() // 32 random bytes of entropy
	expires := time.Now().Add(sessionTTL)
	if err := a.store.CreateSession(r.Context(), token, userID, expires); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
	return nil
}

func (a *App) endSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}
