package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

// Link tokens let another app (tally, for RFC-004) add income entries to one
// cashplan through POST /api/v1/plans/{slug}/entries. The owner creates and
// revokes them on /kelola/{slug}/integrasi. A token can only add income: it
// cannot read the cashplan, edit an entry or add an expense.

const (
	linkTokenPrefix  = "cfl_"
	maxLinkTokens    = 5 // active tokens per cashplan
	maxLinkNameLen   = 60
	maxSourceKeyLen  = 200
	maxAPIBodyBytes  = 16 << 10
	maxAPIAmount     = 1_000_000_000_000 // Rp 1 triliun: anything above is a bug in the caller
	linkEntryRLScope = "cashflow.entry.create"
)

var ErrTooManyLinkTokens = errors.New("too many active link tokens")

type LinkToken struct {
	ID         string
	CashplanID string
	Name       string
	CreatedAt  time.Time
	LastUsedAt time.Time // zero when never used
	RevokedAt  time.Time // zero while active
	Entries    int       // entries added with this token
}

func (t LinkToken) Active() bool { return t.RevokedAt.IsZero() }

func hashLinkToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// --- Store ---

// CreateLinkToken makes a token for the cashplan and returns it. Only its hash
// is stored, so this is the one time the token can be shown.
func (s *Store) CreateLinkToken(ctx context.Context, planID, name string) (string, error) {
	token := linkTokenPrefix + newToken() + newToken()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	// Lock the cashplan row so two creates can't both pass the limit.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM cashplans WHERE id = $1 FOR UPDATE`, planID); err != nil {
		return "", err
	}
	var active int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM link_tokens WHERE cashplan_id = $1 AND revoked_at IS NULL`, planID,
	).Scan(&active); err != nil {
		return "", err
	}
	if active >= maxLinkTokens {
		return "", ErrTooManyLinkTokens
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO link_tokens (cashplan_id, name, token_hash) VALUES ($1, $2, $3)`,
		planID, name, hashLinkToken(token)); err != nil {
		return "", err
	}
	return token, tx.Commit(ctx)
}

// LinkTokens lists a cashplan's tokens, active ones first, newest first.
func (s *Store) LinkTokens(ctx context.Context, planID string) ([]LinkToken, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.id, t.cashplan_id, t.name, t.created_at, t.last_used_at, t.revoked_at,
		        (SELECT COUNT(*) FROM entries e WHERE e.link_token_id = t.id)
		 FROM link_tokens t
		 WHERE t.cashplan_id = $1
		 ORDER BY t.revoked_at IS NOT NULL, t.created_at DESC`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LinkToken
	for rows.Next() {
		var t LinkToken
		var used, revoked *time.Time
		if err := rows.Scan(&t.ID, &t.CashplanID, &t.Name, &t.CreatedAt, &used, &revoked, &t.Entries); err != nil {
			return nil, err
		}
		if used != nil {
			t.LastUsedAt = *used
		}
		if revoked != nil {
			t.RevokedAt = *revoked
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeLinkToken stops a token from working. The planID predicate enforces
// ownership. Returns rows affected (0 if unknown or already revoked).
func (s *Store) RevokeLinkToken(ctx context.Context, planID, tokenID string) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE link_tokens SET revoked_at = now()
		 WHERE id = $1 AND cashplan_id = $2 AND revoked_at IS NULL`, tokenID, planID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ActiveLinkToken finds the active token with this value, or pgx.ErrNoRows.
func (s *Store) ActiveLinkToken(ctx context.Context, token string) (*LinkToken, error) {
	t := &LinkToken{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, cashplan_id, name, created_at FROM link_tokens
		 WHERE token_hash = $1 AND revoked_at IS NULL`, hashLinkToken(token),
	).Scan(&t.ID, &t.CashplanID, &t.Name, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// LinkedEntry is the result of AddLinkedEntry.
type LinkedEntry struct {
	ID         string
	Amount     int64
	OccurredAt time.Time
	Created    bool // false when the key was already used
}

// AddLinkedEntry adds an income entry sent with a link token. The key is the
// caller's idempotency key: a second call with the same key adds nothing and
// returns the entry the first call made, so the caller can retry safely.
func (s *Store) AddLinkedEntry(ctx context.Context, tok *LinkToken, key, party, desc string, amount int64, occurred time.Time) (LinkedEntry, error) {
	le := LinkedEntry{Created: true}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO entries (cashplan_id, type, party, description, amount, occurred_at, link_token_id, source_key)
		 VALUES ($1, 'income', $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (cashplan_id, source_key) WHERE source_key IS NOT NULL DO NOTHING
		 RETURNING id, amount, occurred_at`,
		tok.CashplanID, party, desc, amount, ymd(occurred), tok.ID, key,
	).Scan(&le.ID, &le.Amount, &le.OccurredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		le.Created = false
		err = s.pool.QueryRow(ctx,
			`SELECT id, amount, occurred_at FROM entries WHERE cashplan_id = $1 AND source_key = $2`,
			tok.CashplanID, key,
		).Scan(&le.ID, &le.Amount, &le.OccurredAt)
	}
	if err != nil {
		return LinkedEntry{}, err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE link_tokens SET last_used_at = now() WHERE id = $1`, tok.ID); err != nil {
		log.Printf("link token last used: %v", err)
	}
	return le, nil
}

// --- Owner page: /kelola/{slug}/integrasi ---

type integrasiVM struct {
	Plan      *CashPlan
	Tokens    []LinkToken
	Active    int
	Max       int
	Endpoint  string
	NewToken  string // set once, right after creating a token
	NewName   string
	FormToken string
	Err       string
}

func (a *App) handleIntegrasi(w http.ResponseWriter, r *http.Request) {
	plan := a.ownedPlan(w, r)
	if plan == nil {
		return
	}
	a.renderIntegrasi(w, r, plan, integrasiVM{})
}

func (a *App) renderIntegrasi(w http.ResponseWriter, r *http.Request, plan *CashPlan, vm integrasiVM) {
	tokens, err := a.store.LinkTokens(r.Context(), plan.ID)
	if err != nil {
		log.Printf("link tokens: %v", err)
	}
	for _, t := range tokens {
		if t.Active() {
			vm.Active++
		}
	}
	vm.Plan, vm.Tokens, vm.Max = plan, tokens, maxLinkTokens
	vm.Endpoint = baseURL(r) + "/api/v1/plans/" + plan.Slug + "/entries"
	vm.FormToken = newToken()
	a.render(w, r, "integrasi", vm)
}

func (a *App) handleCreateLinkToken(w http.ResponseWriter, r *http.Request) {
	plan := a.ownedPlan(w, r)
	if plan == nil {
		return
	}
	limitBody(w, r, maxFormBytes)
	name := strings.TrimSpace(r.FormValue("name"))
	switch {
	case name == "":
		a.renderIntegrasi(w, r, plan, integrasiVM{Err: "Nama integrasi wajib diisi."})
		return
	case tooLong(name, maxLinkNameLen):
		a.renderIntegrasi(w, r, plan, integrasiVM{Err: fmt.Sprintf("Nama integrasi terlalu panjang (maksimal %d karakter).", maxLinkNameLen)})
		return
	}
	// A resent form must not make a second token the owner never saw.
	if ft := r.FormValue("form_token"); ft != "" && !a.guard.firstUse(ft) {
		http.Redirect(w, r, "/kelola/"+plan.Slug+"/integrasi", http.StatusSeeOther)
		return
	}
	token, err := a.store.CreateLinkToken(r.Context(), plan.ID, name)
	if errors.Is(err, ErrTooManyLinkTokens) {
		a.renderIntegrasi(w, r, plan, integrasiVM{Err: fmt.Sprintf("Sudah ada %d token aktif. Cabut salah satu dulu.", maxLinkTokens)})
		return
	}
	if err != nil {
		log.Printf("create link token: %v", err)
		a.renderIntegrasi(w, r, plan, integrasiVM{Err: "Gagal membuat token."})
		return
	}
	// Rendered, not redirected: the token must not end up in a URL or history.
	w.Header().Set("Cache-Control", "no-store")
	a.renderIntegrasi(w, r, plan, integrasiVM{NewToken: token, NewName: name})
}

func (a *App) handleRevokeLinkToken(w http.ResponseWriter, r *http.Request) {
	plan := a.ownedPlan(w, r)
	if plan == nil {
		return
	}
	if _, err := a.store.RevokeLinkToken(r.Context(), plan.ID, r.PathValue("id")); err != nil {
		log.Printf("revoke link token: %v", err)
		a.renderIntegrasi(w, r, plan, integrasiVM{Err: "Gagal mencabut token."})
		return
	}
	http.Redirect(w, r, "/kelola/"+plan.Slug+"/integrasi", http.StatusSeeOther)
}

// --- API: POST /api/v1/plans/{slug}/entries ---

type apiEntryRequest struct {
	Key         string `json:"key"`
	Party       string `json:"party"`
	Description string `json:"description"`
	Amount      int64  `json:"amount"`
	Date        string `json:"date"` // YYYY-MM-DD; empty means today (Asia/Jakarta)
}

type apiEntryResponse struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Amount  int64  `json:"amount"`
	Date    string `json:"date"`
	Created bool   `json:"created"`
}

// apiEnvelope matches the shape all-in-one's APIs answer with.
type apiEnvelope struct {
	Success bool    `json:"success"`
	Message *string `json:"message"`
	Data    any     `json:"data"`
	Error   *string `json:"error"`
}

func writeAPI(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiEnvelope{Success: true, Data: data})
}

// writeAPIError answers with a stable machine code and a human message.
func writeAPIError(w http.ResponseWriter, status int, code, msg string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="cashflow"`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiEnvelope{Message: &msg, Error: &code})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// validSourceKey allows printable text without spaces, like "tally:42:1".
func validSourceKey(k string) bool {
	if k == "" || len(k) > maxSourceKeyLen {
		return false
	}
	for _, r := range k {
		if r > unicode.MaxASCII || !unicode.IsGraphic(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func (a *App) handleAPIAddEntry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := bearerToken(r)
	if token == "" {
		writeAPIError(w, http.StatusUnauthorized, "missing_token", "Send the link token as Authorization: Bearer <token>.")
		return
	}
	tok, err := a.store.ActiveLinkToken(ctx, token)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		log.Printf("api: link token: %v", err)
		writeAPIError(w, http.StatusInternalServerError, "internal", "Could not check the token.")
		return
	}
	var plan *CashPlan
	if tok != nil {
		if plan, err = a.store.PlanBySlug(ctx, r.PathValue("slug")); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("api: plan: %v", err)
			writeAPIError(w, http.StatusInternalServerError, "internal", "Could not load the cashplan.")
			return
		}
	}
	// Unknown, revoked and other-cashplan tokens get the same answer, so a
	// token never tells its holder which other cashplans exist.
	if tok == nil || plan == nil || plan.ID != tok.CashplanID {
		writeAPIError(w, http.StatusUnauthorized, "invalid_token", "The token is unknown, revoked, or not for this cashplan.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAPIBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req apiEntryRequest
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("The body is over %d bytes.", maxAPIBodyBytes))
			return
		}
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "The body is not a valid entry: "+err.Error())
		return
	}
	req.Party = strings.TrimSpace(req.Party)
	req.Description = strings.TrimSpace(req.Description)
	occurred := time.Now().In(jakarta)
	if d := strings.TrimSpace(req.Date); d != "" {
		if occurred, err = time.ParseInLocation("2006-01-02", d, jakarta); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_date", "date must be YYYY-MM-DD.")
			return
		}
	}
	switch {
	case !validSourceKey(req.Key):
		writeAPIError(w, http.StatusBadRequest, "invalid_key", fmt.Sprintf("key is required: up to %d printable ASCII characters, no spaces.", maxSourceKeyLen))
		return
	case req.Amount <= 0 || req.Amount > maxAPIAmount:
		writeAPIError(w, http.StatusBadRequest, "invalid_amount", "amount must be whole rupiah, above 0.")
		return
	case req.Party == "":
		writeAPIError(w, http.StatusBadRequest, "invalid_party", "party (the payer) is required.")
		return
	case tooLong(req.Party, maxPartyLen):
		writeAPIError(w, http.StatusBadRequest, "invalid_party", fmt.Sprintf("party is over %d characters.", maxPartyLen))
		return
	case tooLong(req.Description, maxDescLen):
		writeAPIError(w, http.StatusBadRequest, "invalid_description", fmt.Sprintf("description is over %d characters.", maxDescLen))
		return
	}

	if allowed, _ := a.rl.Allow(ctx, linkEntryRLScope, "link:"+tok.ID); !allowed {
		writeAPIError(w, http.StatusTooManyRequests, "rate_limited", "Too many entries; try again later.")
		return
	}

	le, err := a.store.AddLinkedEntry(ctx, tok, req.Key, req.Party, req.Description, req.Amount, occurred)
	if err != nil {
		log.Printf("api: add entry: %v", err)
		writeAPIError(w, http.StatusInternalServerError, "internal", "Could not save the entry.")
		return
	}
	// The same key with another amount is a bug in the caller, not a retry.
	if !le.Created && le.Amount != req.Amount {
		writeAPIError(w, http.StatusConflict, "key_conflict",
			fmt.Sprintf("key was already used for an entry of %d; amounts never change.", le.Amount))
		return
	}
	status := http.StatusOK
	if le.Created {
		status = http.StatusCreated
		recordEntryCreated(ctx, "income")
	}
	writeAPI(w, status, apiEntryResponse{
		ID:      le.ID,
		URL:     baseURL(r) + "/p/" + plan.Slug + "/entry/" + le.ID,
		Amount:  le.Amount,
		Date:    ymd(le.OccurredAt),
		Created: le.Created,
	})
}
