package main

import (
	"errors"
	"log"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// requireAdmin lets the request through only if aio says, right now, that the
// user is an aio admin (RFC-002 in all-in-one). There is no cache: a user
// removed from aio's admin group loses access on their next request. Pages
// are 404 for everyone else so they aren't advertised; if aio can't be asked,
// the page is refused (fails closed).
func (a *App) requireAdmin(w http.ResponseWriter, r *http.Request) *User {
	if !a.aio.Enabled() {
		a.notFound(w, r)
		return nil
	}
	u := a.requireUser(w, r)
	if u == nil {
		return nil
	}
	if u.AioUserID == "" {
		recordAdminCheck(r.Context(), "local_account")
		a.notFound(w, r)
		return nil
	}
	st, err := a.aio.userStatus(r.Context(), u.AioUserID)
	if err != nil {
		log.Printf("admin: status check for %s: %v", u.Username, err)
		recordAdminCheck(r.Context(), "error")
		a.renderStatus(w, r, "adminerror", nil, http.StatusServiceUnavailable)
		return nil
	}
	if !st.Admin {
		recordAdminCheck(r.Context(), "not_admin")
		a.notFound(w, r)
		return nil
	}
	recordAdminCheck(r.Context(), "admin")
	w.Header().Set("Cache-Control", "no-store")
	return u
}

func (a *App) logAdminView(r *http.Request, admin *User, page, subjectID string) {
	if err := a.store.LogAdminView(r.Context(), admin.ID, page, subjectID); err != nil {
		log.Printf("admin: audit log (%s %s %s): %v", admin.Username, page, subjectID, err)
	}
}

type adminUsersVM struct {
	Users []AdminUserRow
}

// handleAdminUsers is the overview: every user's activity, no amounts.
func (a *App) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	admin := a.requireAdmin(w, r)
	if admin == nil {
		return
	}
	users, err := a.store.AdminUsers(r.Context())
	if err != nil {
		log.Printf("admin: list users: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.logAdminView(r, admin, "users", "")
	a.render(w, r, "admin", adminUsersVM{Users: users})
}

type adminUserVM struct {
	User  *User
	Plans []PlanCard
}

// handleAdminUser is one user's cashplans with their totals. Opening it is
// the audit-logged event that reveals money.
func (a *App) handleAdminUser(w http.ResponseWriter, r *http.Request) {
	admin := a.requireAdmin(w, r)
	if admin == nil {
		return
	}
	id := r.PathValue("id")
	if uuid.Validate(id) != nil {
		a.notFound(w, r)
		return
	}
	u, err := a.store.UserByID(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		a.notFound(w, r)
		return
	}
	if err != nil {
		log.Printf("admin: load user %s: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	plans, err := a.store.PlansByOwner(r.Context(), u.ID)
	if err != nil {
		log.Printf("admin: plans of %s: %v", u.Username, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.logAdminView(r, admin, "user", u.ID)
	a.render(w, r, "adminuser", adminUserVM{User: u, Plans: plans})
}
