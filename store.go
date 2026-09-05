package main

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the data-access layer over PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
}

var (
	ErrUsernameTaken = errors.New("nama pengguna sudah dipakai")
	ErrSlugTaken     = errors.New("tautan (slug) sudah dipakai")
)

type User struct {
	ID        string
	Username  string
	CreatedAt time.Time
}

type CashPlan struct {
	ID          string
	OwnerID     string
	Slug        string
	Title       string
	Description string
	CreatedAt   time.Time
}

type Entry struct {
	ID             string
	Type           string // "income" | "expense"
	Party          string // income: payer; expense: payee
	Description    string
	Amount         int64
	OccurredAt     time.Time
	CreatedAt      time.Time
	AttachmentURL  string // public Nextcloud share link, or ""
	AttachmentName string // original filename, for display
}

func (e Entry) IsIncome() bool      { return e.Type == "income" }
func (e Entry) HasAttachment() bool { return e.AttachmentURL != "" }

type Summary struct {
	TotalIncome  int64
	TotalExpense int64
	Balance      int64
	PayerCount   int
	EntryCount   int
}

// PlanCard is a plan plus its summary, for the owner dashboard.
type PlanCard struct {
	Plan    CashPlan
	Summary Summary
}

// newToken returns an unguessable, URL-safe token (~26 lowercase chars).
func newToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing is not recoverable
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	return strings.ToLower(enc.EncodeToString(b))
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// --- Users & sessions ---

func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (*User, error) {
	u := &User{Username: username}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (username, password_hash) VALUES ($1, $2)
		 RETURNING id, created_at`, username, passwordHash,
	).Scan(&u.ID, &u.CreatedAt)
	if isUniqueViolation(err) {
		return nil, ErrUsernameTaken
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// UserByUsername returns the user and its password hash, for login.
func (s *Store) UserByUsername(ctx context.Context, username string) (*User, string, error) {
	u := &User{}
	var hash string
	err := s.pool.QueryRow(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE username = $1`, username,
	).Scan(&u.ID, &u.Username, &hash, &u.CreatedAt)
	if err != nil {
		return nil, "", err
	}
	return u, hash, nil
}

func (s *Store) CreateSession(ctx context.Context, token, userID string, expires time.Time) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (id, user_id, expires_at) VALUES ($1, $2, $3)`,
		token, userID, expires)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, token)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	ct, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
	return ct.RowsAffected(), err
}

func (s *Store) UserBySession(ctx context.Context, token string) (*User, error) {
	u := &User{}
	err := s.pool.QueryRow(ctx,
		`SELECT u.id, u.username, u.created_at
		 FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.id = $1 AND s.expires_at > now()`, token,
	).Scan(&u.ID, &u.Username, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// --- Cashplans ---

func (s *Store) CreatePlan(ctx context.Context, ownerID, slug, title, desc string) (*CashPlan, error) {
	p := &CashPlan{OwnerID: ownerID, Slug: slug, Title: title, Description: desc}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO cashplans (owner_id, slug, title, description)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, created_at`,
		ownerID, slug, title, desc,
	).Scan(&p.ID, &p.CreatedAt)
	if isUniqueViolation(err) {
		return nil, ErrSlugTaken
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Store) PlanBySlug(ctx context.Context, slug string) (*CashPlan, error) {
	p := &CashPlan{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, owner_id, slug, title, description, created_at
		 FROM cashplans WHERE slug = $1`, slug,
	).Scan(&p.ID, &p.OwnerID, &p.Slug, &p.Title, &p.Description, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// PlansByOwner returns the owner's plans, each with a computed summary.
func (s *Store) PlansByOwner(ctx context.Context, ownerID string) ([]PlanCard, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT c.id, c.owner_id, c.slug, c.title, c.description, c.created_at,
		        COALESCE(SUM(e.amount) FILTER (WHERE e.type = 'income'), 0),
		        COALESCE(SUM(e.amount) FILTER (WHERE e.type = 'expense'), 0),
		        COUNT(DISTINCT e.party) FILTER (WHERE e.type = 'income' AND e.party <> ''),
		        COUNT(e.id)
		 FROM cashplans c
		 LEFT JOIN entries e ON e.cashplan_id = c.id
		 WHERE c.owner_id = $1
		 GROUP BY c.id
		 ORDER BY c.created_at DESC`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PlanCard
	for rows.Next() {
		var pc PlanCard
		if err := rows.Scan(
			&pc.Plan.ID, &pc.Plan.OwnerID, &pc.Plan.Slug, &pc.Plan.Title,
			&pc.Plan.Description, &pc.Plan.CreatedAt,
			&pc.Summary.TotalIncome, &pc.Summary.TotalExpense,
			&pc.Summary.PayerCount, &pc.Summary.EntryCount,
		); err != nil {
			return nil, err
		}
		pc.Summary.Balance = pc.Summary.TotalIncome - pc.Summary.TotalExpense
		out = append(out, pc)
	}
	return out, rows.Err()
}

// --- Entries ---

func (s *Store) AddEntry(ctx context.Context, planID, typ, party, desc string, amount int64, occurred time.Time, attachmentURL, attachmentName string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO entries (cashplan_id, type, party, description, amount, occurred_at, attachment_url, attachment_name)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		planID, typ, party, desc, amount, occurred.Format("2006-01-02"), attachmentURL, attachmentName,
	)
	return err
}

const pageSize = 20

// EntryPage is one page of history, plus enough state to paginate.
type EntryPage struct {
	Entries    []Entry
	Total      int // total rows matching the query
	Page       int // clamped to [1, TotalPages]
	TotalPages int
}

// escapeLike neutralizes LIKE wildcards so a search term is matched literally.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}

// EntriesPage returns page `page` of the history, filtered by a case-insensitive
// substring `q` over party and description. An empty q matches everything.
func (s *Store) EntriesPage(ctx context.Context, planID, q string, page int) (EntryPage, error) {
	pattern := "%" + escapeLike(strings.TrimSpace(q)) + "%"

	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM entries
		 WHERE cashplan_id = $1
		   AND (party ILIKE $2 ESCAPE '\' OR description ILIKE $2 ESCAPE '\')`,
		planID, pattern).Scan(&total); err != nil {
		return EntryPage{}, err
	}

	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	rows, err := s.pool.Query(ctx,
		`SELECT id, type, party, description, amount, occurred_at, created_at, attachment_url, attachment_name
		 FROM entries
		 WHERE cashplan_id = $1
		   AND (party ILIKE $2 ESCAPE '\' OR description ILIKE $2 ESCAPE '\')
		 ORDER BY occurred_at DESC, created_at DESC
		 LIMIT $3 OFFSET $4`,
		planID, pattern, pageSize, (page-1)*pageSize)
	if err != nil {
		return EntryPage{}, err
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Type, &e.Party, &e.Description,
			&e.Amount, &e.OccurredAt, &e.CreatedAt, &e.AttachmentURL, &e.AttachmentName); err != nil {
			return EntryPage{}, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return EntryPage{}, err
	}
	return EntryPage{Entries: out, Total: total, Page: page, TotalPages: totalPages}, nil
}

// PayerTotal is one row of the per-payer breakdown (income only).
type PayerTotal struct {
	Party string
	Total int64
	Count int
	Last  time.Time
}

// PayerBreakdown aggregates income by payer, largest contributor first.
func (s *Store) PayerBreakdown(ctx context.Context, planID string) ([]PayerTotal, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT party, SUM(amount), COUNT(*), MAX(occurred_at)
		 FROM entries
		 WHERE cashplan_id = $1 AND type = 'income' AND party <> ''
		 GROUP BY party
		 ORDER BY SUM(amount) DESC, party ASC`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PayerTotal
	for rows.Next() {
		var p PayerTotal
		if err := rows.Scan(&p.Party, &p.Total, &p.Count, &p.Last); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) SummaryFor(ctx context.Context, planID string) (Summary, error) {
	var sm Summary
	err := s.pool.QueryRow(ctx,
		`SELECT
		   COALESCE(SUM(amount) FILTER (WHERE type = 'income'), 0),
		   COALESCE(SUM(amount) FILTER (WHERE type = 'expense'), 0),
		   COUNT(DISTINCT party) FILTER (WHERE type = 'income' AND party <> ''),
		   COUNT(*)
		 FROM entries WHERE cashplan_id = $1`, planID,
	).Scan(&sm.TotalIncome, &sm.TotalExpense, &sm.PayerCount, &sm.EntryCount)
	if err != nil {
		return sm, err
	}
	sm.Balance = sm.TotalIncome - sm.TotalExpense
	return sm, nil
}

// ExpensesFor returns all expense entries in chronological order (for the report).
func (s *Store) ExpensesFor(ctx context.Context, planID string) ([]Entry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, type, party, description, amount, occurred_at, created_at, attachment_url, attachment_name
		 FROM entries
		 WHERE cashplan_id = $1 AND type = 'expense'
		 ORDER BY occurred_at ASC, created_at ASC`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Type, &e.Party, &e.Description,
			&e.Amount, &e.OccurredAt, &e.CreatedAt, &e.AttachmentURL, &e.AttachmentName); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PlanPeriod returns the earliest and latest entry dates; ok is false when the
// plan has no entries yet.
func (s *Store) PlanPeriod(ctx context.Context, planID string) (start, end time.Time, ok bool, err error) {
	var mn, mx *time.Time
	err = s.pool.QueryRow(ctx,
		`SELECT MIN(occurred_at), MAX(occurred_at) FROM entries WHERE cashplan_id = $1`, planID,
	).Scan(&mn, &mx)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if mn == nil || mx == nil {
		return time.Time{}, time.Time{}, false, nil
	}
	return *mn, *mx, true, nil
}
