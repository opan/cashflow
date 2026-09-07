package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// duesMaxCols caps the number of period columns rendered, so a wildly overpaid
// member or a very old start date can't produce an enormous table.
const duesMaxCols = 36

// ErrMemberExists is returned by AddMember when the (case-insensitive) name is
// already on the roster.
var ErrMemberExists = errors.New("member already exists")

// Member is one entry on a cashplan's dues roster.
type Member struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// --- Store: dues config + roster ---

// SetDues enables/updates the recurring dues config for a cashplan.
func (s *Store) SetDues(ctx context.Context, planID string, amount int64, period string, start time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE cashplans SET due_amount = $2, due_period = $3, due_start = $4 WHERE id = $1`,
		planID, amount, period, start.Format("2006-01-02"))
	return err
}

// DisableDues turns dues tracking off for a cashplan (roster is kept).
func (s *Store) DisableDues(ctx context.Context, planID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE cashplans SET due_amount = 0, due_period = '', due_start = NULL WHERE id = $1`, planID)
	return err
}

func (s *Store) ListMembers(ctx context.Context, planID string) ([]Member, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, created_at FROM members WHERE cashplan_id = $1 ORDER BY lower(btrim(name))`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.Name, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddMember inserts a roster member, returning ErrMemberExists on a duplicate
// (case-insensitive) name within the plan.
func (s *Store) AddMember(ctx context.Context, planID, name string) error {
	ct, err := s.pool.Exec(ctx,
		`INSERT INTO members (cashplan_id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`, planID, name)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrMemberExists
	}
	return nil
}

func (s *Store) RemoveMember(ctx context.Context, planID, memberID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM members WHERE id = $1 AND cashplan_id = $2`, memberID, planID)
	return err
}

// MemberPaidTotals returns, per normalized payer name, the total income recorded
// for a cashplan. Keys are lower(btrim(party)) so they match roster names.
func (s *Store) MemberPaidTotals(ctx context.Context, planID string) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT lower(btrim(party)), COALESCE(SUM(amount), 0)
		 FROM entries WHERE cashplan_id = $1 AND type = 'income' AND btrim(party) <> ''
		 GROUP BY lower(btrim(party))`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// --- Period math ---

func monthsPerPeriod(period string) int {
	switch period {
	case "bimonthly":
		return 2
	case "quarterly":
		return 3
	case "yearly":
		return 12
	default: // monthly
		return 1
	}
}

func validDuePeriod(period string) bool {
	switch period {
	case "weekly", "monthly", "bimonthly", "quarterly", "yearly":
		return true
	}
	return false
}

func periodTypeLabel(period string) string {
	switch period {
	case "weekly":
		return "Mingguan"
	case "monthly":
		return "Bulanan"
	case "bimonthly":
		return "Dwibulanan (2 bulan)"
	case "quarterly":
		return "Triwulanan (3 bulan)"
	case "yearly":
		return "Tahunan"
	default:
		return period
	}
}

// periodIndex returns the 0-based index of the period that t falls into, counting
// forward from start. t before start yields 0.
func periodIndex(start time.Time, period string, t time.Time) int {
	if t.Before(start) {
		return 0
	}
	if period == "weekly" {
		return int(t.Sub(start).Hours()/24) / 7
	}
	months := (t.Year()-start.Year())*12 + int(t.Month()) - int(start.Month())
	if t.Day() < start.Day() { // the current period hasn't rolled over yet
		months--
	}
	if months < 0 {
		months = 0
	}
	return months / monthsPerPeriod(period)
}

// periodLabel renders the i-th period (0-based) as a compact Indonesian label.
func periodLabel(start time.Time, period string, i int) string {
	switch period {
	case "weekly":
		return weekLabel(start.AddDate(0, 0, 7*i))
	case "bimonthly":
		ps := start.AddDate(0, 2*i, 0)
		pe := ps.AddDate(0, 1, 0)
		return fmt.Sprintf("%s–%s %d", bulanID[int(ps.Month())], bulanID[int(pe.Month())], pe.Year())
	case "quarterly":
		ps := start.AddDate(0, 3*i, 0)
		pe := ps.AddDate(0, 2, 0)
		return fmt.Sprintf("%s–%s %d", bulanID[int(ps.Month())], bulanID[int(pe.Month())], pe.Year())
	case "yearly":
		return fmt.Sprintf("%d", start.AddDate(i, 0, 0).Year())
	default: // monthly
		return formatBulanTahun(start.AddDate(0, i, 0))
	}
}

func normName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// --- Coverage table ---

type DuesCell struct {
	Status string // "lunas" | "sebagian" | "belum"
	Paid   int64  // amount applied to this period
	Short  int64  // amount still needed (only when "sebagian")
}

type DuesRow struct {
	Member      string
	TotalPaid   int64
	Cells       []DuesCell
	PaidPeriods int
	StatusText  string
}

type DuesTable struct {
	Due         int64
	PeriodType  string
	PeriodLabel string
	StartLabel  string
	Columns     []string
	Rows        []DuesRow
	Truncated   bool
	BasePath    string
	Owner       bool
}

// buildDuesTable computes the per-member coverage matrix using cumulative
// (waterfall) allocation: each member's total income fills consecutive periods
// from the start, one due amount at a time. now determines how many empty
// periods to show for members who haven't paid up to date.
func buildDuesTable(plan *CashPlan, members []Member, paid map[string]int64, now time.Time) DuesTable {
	due := plan.DueAmount
	if due <= 0 {
		return DuesTable{}
	}
	start := plan.DueStart

	cols := periodIndex(start, plan.DuePeriod, now) + 1
	for _, m := range members {
		cells := int(paid[normName(m.Name)] / due)
		if paid[normName(m.Name)]%due != 0 {
			cells++
		}
		if cells > cols {
			cols = cells
		}
	}
	if cols < 1 {
		cols = 1
	}
	truncated := false
	if cols > duesMaxCols {
		cols, truncated = duesMaxCols, true
	}

	columns := make([]string, cols)
	for i := range columns {
		columns[i] = periodLabel(start, plan.DuePeriod, i)
	}

	rows := make([]DuesRow, 0, len(members))
	for _, m := range members {
		total := paid[normName(m.Name)]
		full := int(total / due)
		rem := total - int64(full)*due
		cells := make([]DuesCell, cols)
		for i := range cells {
			switch {
			case i < full:
				cells[i] = DuesCell{Status: "lunas", Paid: due}
			case i == full && rem > 0:
				cells[i] = DuesCell{Status: "sebagian", Paid: rem, Short: due - rem}
			default:
				cells[i] = DuesCell{Status: "belum"}
			}
		}
		rows = append(rows, DuesRow{
			Member:      m.Name,
			TotalPaid:   total,
			Cells:       cells,
			PaidPeriods: full,
			StatusText:  duesStatusText(plan.DuePeriod, start, full, rem, due),
		})
	}

	return DuesTable{
		Due:         due,
		PeriodType:  plan.DuePeriod,
		PeriodLabel: periodTypeLabel(plan.DuePeriod),
		StartLabel:  periodLabel(start, plan.DuePeriod, 0),
		Columns:     columns,
		Rows:        rows,
		Truncated:   truncated,
	}
}

func duesStatusText(period string, start time.Time, full int, rem, due int64) string {
	if full == 0 && rem == 0 {
		return "Belum bayar"
	}
	var s string
	if full > 0 {
		s = "Lunas s/d " + periodLabel(start, period, full-1)
	}
	if rem > 0 {
		if s != "" {
			s += ", "
		}
		s += "kurang " + formatRupiah(due-rem) + " untuk " + periodLabel(start, period, full)
	}
	return s
}

// --- View model + handlers ---

type duesVM struct {
	Plan     *CashPlan
	Table    DuesTable
	Members  []Member
	Owner    bool
	BasePath string
	Today    string
	Err      string
}

func (a *App) renderDues(w http.ResponseWriter, r *http.Request, plan *CashPlan, owner bool, errMsg string) {
	base := "/p/" + plan.Slug
	if owner {
		base = "/kelola/" + plan.Slug
	}
	members, err := a.store.ListMembers(r.Context(), plan.ID)
	if err != nil {
		log.Printf("list members: %v", err)
	}
	var table DuesTable
	if plan.DuesEnabled() {
		paid, err := a.store.MemberPaidTotals(r.Context(), plan.ID)
		if err != nil {
			log.Printf("member totals: %v", err)
		}
		table = buildDuesTable(plan, members, paid, time.Now().In(jakarta))
		table.BasePath = base
		table.Owner = owner
	}
	a.render(w, r, "iuran", duesVM{
		Plan:     plan,
		Table:    table,
		Members:  members,
		Owner:    owner,
		BasePath: base,
		Today:    time.Now().In(jakarta).Format("2006-01-02"),
		Err:      errMsg,
	})
}

func (a *App) handleManageDues(w http.ResponseWriter, r *http.Request) {
	plan := a.ownedPlan(w, r)
	if plan == nil {
		return
	}
	a.renderDues(w, r, plan, true, "")
}

func (a *App) handleViewDues(w http.ResponseWriter, r *http.Request) {
	plan, err := a.store.PlanBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		a.notFound(w, r)
		return
	}
	a.renderDues(w, r, plan, false, "")
}

func (a *App) handleDuesConfig(w http.ResponseWriter, r *http.Request) {
	plan := a.ownedPlan(w, r)
	if plan == nil {
		return
	}
	limitBody(w, r, maxFormBytes)
	if r.FormValue("action") == "disable" {
		if err := a.store.DisableDues(r.Context(), plan.ID); err != nil {
			log.Printf("disable dues: %v", err)
			a.renderDues(w, r, plan, true, "Gagal menonaktifkan iuran.")
			return
		}
		http.Redirect(w, r, "/kelola/"+plan.Slug+"/iuran", http.StatusSeeOther)
		return
	}
	amount, aErr := parseAmount(r.FormValue("amount"))
	period := r.FormValue("period")
	start, sErr := time.ParseInLocation("2006-01-02", strings.TrimSpace(r.FormValue("start")), jakarta)
	switch {
	case aErr != nil:
		a.renderDues(w, r, plan, true, "Jumlah iuran tidak valid: "+aErr.Error()+".")
		return
	case !validDuePeriod(period):
		a.renderDues(w, r, plan, true, "Periode tidak valid.")
		return
	case sErr != nil:
		a.renderDues(w, r, plan, true, "Tanggal mulai tidak valid.")
		return
	}
	if err := a.store.SetDues(r.Context(), plan.ID, amount, period, start); err != nil {
		log.Printf("set dues: %v", err)
		a.renderDues(w, r, plan, true, "Gagal menyimpan pengaturan iuran.")
		return
	}
	http.Redirect(w, r, "/kelola/"+plan.Slug+"/iuran", http.StatusSeeOther)
}

func (a *App) handleAddMember(w http.ResponseWriter, r *http.Request) {
	plan := a.ownedPlan(w, r)
	if plan == nil {
		return
	}
	limitBody(w, r, maxFormBytes)
	name := strings.TrimSpace(r.FormValue("name"))
	switch {
	case name == "":
		a.renderDues(w, r, plan, true, "Nama anggota wajib diisi.")
		return
	case tooLong(name, maxPartyLen):
		a.renderDues(w, r, plan, true, "Nama terlalu panjang (maksimal 200 karakter).")
		return
	}
	if err := a.store.AddMember(r.Context(), plan.ID, name); err != nil {
		if errors.Is(err, ErrMemberExists) {
			a.renderDues(w, r, plan, true, "Anggota \""+name+"\" sudah ada.")
			return
		}
		log.Printf("add member: %v", err)
		a.renderDues(w, r, plan, true, "Gagal menambah anggota.")
		return
	}
	http.Redirect(w, r, "/kelola/"+plan.Slug+"/iuran", http.StatusSeeOther)
}

func (a *App) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	plan := a.ownedPlan(w, r)
	if plan == nil {
		return
	}
	if err := a.store.RemoveMember(r.Context(), plan.ID, r.PathValue("id")); err != nil {
		log.Printf("remove member: %v", err)
	}
	http.Redirect(w, r, "/kelola/"+plan.Slug+"/iuran", http.StatusSeeOther)
}
