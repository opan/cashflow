package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// App wires the store, templates, and optional Nextcloud client into HTTP handlers.
type App struct {
	store *Store
	tmpl  map[string]*template.Template
	nc    *Nextcloud // nil when uploads are not configured
}

const (
	maxUploadBytes = 5 << 20   // 5 MiB per receipt
	maxFormBytes   = 256 << 10 // 256 KiB cap for ordinary (non-upload) form posts
	maxTitleLen    = 200
	maxDescLen     = 1000
	maxPartyLen    = 200
	maxPasswordLen = 72 // bcrypt hashes at most 72 bytes
)

var allowedReceiptExt = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
	".gif": true, ".pdf": true, ".heic": true,
}

// limitBody caps a request body to guard against oversized posts (DoS).
func limitBody(w http.ResponseWriter, r *http.Request, max int64) {
	r.Body = http.MaxBytesReader(w, r.Body, max)
}

// tooLong reports whether s exceeds max runes.
func tooLong(s string, max int) bool { return utf8.RuneCountInString(s) > max }

func buildTemplates() map[string]*template.Template {
	funcs := template.FuncMap{
		"rupiah": formatRupiah,
		"tgl":    formatTanggal,
		"add1":   func(i int) int { return i + 1 },
		"iso":    func(t time.Time) string { return t.Format("2006-01-02") },
	}
	pages := map[string]*template.Template{}
	// Each page gets its own template set (layout + partials + that page) so
	// their "content"/"title" blocks don't collide.
	for _, name := range []string{"landing", "login", "register", "dashboard", "manage", "view", "laporan", "edit", "versions", "notfound"} {
		t := template.New(name).Funcs(funcs)
		t = template.Must(t.ParseFS(tmplFS,
			"templates/layout.html",
			"templates/partials.html",
			"templates/"+name+".html",
		))
		pages[name] = t
	}
	return pages
}

// pageData wraps every page's view model with cross-cutting state (login).
type pageData struct {
	Username string
	Data     any
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	a.renderStatus(w, r, name, data, http.StatusOK)
}

func (a *App) renderStatus(w http.ResponseWriter, r *http.Request, name string, data any, status int) {
	t, ok := a.tmpl[name]
	if !ok {
		http.Error(w, "template not found: "+name, http.StatusInternalServerError)
		return
	}
	pd := pageData{Data: data}
	if u := currentUser(r); u != nil {
		pd.Username = u.Username
	}
	// Render into a buffer first so a template error becomes a clean 500
	// instead of a half-written response.
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", pd); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (a *App) notFound(w http.ResponseWriter, r *http.Request) {
	a.renderStatus(w, r, "notfound", nil, http.StatusNotFound)
}

// --- View models ---

type dashboardVM struct {
	Plans   []PlanCard
	BaseURL string
	Err     string
	FTitle  string // sticky form values on error
	FSlug   string
	FDesc   string
}

type manageVM struct {
	Plan     *CashPlan
	Summary  Summary
	Payers   []PayerTotal
	History  HistoryView
	ShareURL string
	Today    string
	Uploads  bool // whether receipt uploads are configured
	Err      string
}

type viewVM struct {
	Plan     *CashPlan
	Summary  Summary
	Payers   []PayerTotal
	History  HistoryView
	ShareURL string
}

// HistoryView carries one page of history plus its search/pagination state,
// including prebuilt URLs for the prev/next links.
type HistoryView struct {
	Entries    []Entry
	Query      string
	Page       int
	TotalPages int
	Total      int
	PrevURL    string // "" when there is no previous page
	NextURL    string // "" when there is no next page
	BasePath   string // "/kelola/{slug}" or "/p/{slug}", the search form target
	Owner      bool   // owner view → show edit links
}

func historyURL(basePath, q string, page int) string {
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	if enc := v.Encode(); enc != "" {
		return basePath + "?" + enc
	}
	return basePath
}

func buildHistoryView(ep EntryPage, q, basePath string, owner bool) HistoryView {
	hv := HistoryView{
		Entries:    ep.Entries,
		Query:      q,
		Page:       ep.Page,
		TotalPages: ep.TotalPages,
		Total:      ep.Total,
		BasePath:   basePath,
		Owner:      owner,
	}
	if ep.Page > 1 {
		hv.PrevURL = historyURL(basePath, q, ep.Page-1)
	}
	if ep.Page < ep.TotalPages {
		hv.NextURL = historyURL(basePath, q, ep.Page+1)
	}
	return hv
}

type authVM struct {
	Err      string
	Username string // sticky
}

type reportVM struct {
	Plan        *CashPlan
	Summary     Summary
	Income      Matrix // weekly matrix: pembayar × minggu
	Expense     Matrix // weekly matrix: penerima × minggu
	Expenses    []Entry
	PeriodLabel string
	FromInput   string // YYYY-MM-DD for the range form
	ToInput     string
	Generated   time.Time
	Keterangan  string
	ShareURL    string
	Public      bool   // true when rendered from the public /p/ route
	BasePath    string // "/kelola/{slug}" or "/p/{slug}"
}

// Matrix is a party × week grid of amounts with row/column totals.
type Matrix struct {
	Cols      []string
	Rows      []MatrixRow
	ColTotals []int64
	Grand     int64
}

type MatrixRow struct {
	Party string
	Cells []int64
	Total int64
}

func (m Matrix) HasData() bool { return len(m.Rows) > 0 }

// buildMatrix pivots per-(party, week) totals into a grid: weeks as columns
// (chronological), parties as rows (largest total first), with totals.
func buildMatrix(cells []WeekCell) Matrix {
	var weeks []time.Time
	seen := map[time.Time]bool{}
	for _, c := range cells {
		if !seen[c.Week] {
			seen[c.Week] = true
			weeks = append(weeks, c.Week)
		}
	}
	sort.Slice(weeks, func(i, j int) bool { return weeks[i].Before(weeks[j]) })
	idx := make(map[time.Time]int, len(weeks))
	cols := make([]string, len(weeks))
	for i, w := range weeks {
		idx[w] = i
		cols[i] = weekLabel(w)
	}

	rows := map[string]*MatrixRow{}
	var order []string
	for _, c := range cells {
		row := rows[c.Party]
		if row == nil {
			row = &MatrixRow{Party: c.Party, Cells: make([]int64, len(cols))}
			rows[c.Party] = row
			order = append(order, c.Party)
		}
		row.Cells[idx[c.Week]] += c.Total
		row.Total += c.Total
	}

	m := Matrix{Cols: cols, ColTotals: make([]int64, len(cols))}
	for _, p := range order {
		row := rows[p]
		for i, v := range row.Cells {
			m.ColTotals[i] += v
		}
		m.Grand += row.Total
		m.Rows = append(m.Rows, *row)
	}
	sort.Slice(m.Rows, func(i, j int) bool { return m.Rows[i].Total > m.Rows[j].Total })
	return m
}

// --- Home / landing / dashboard ---

func (a *App) handleHome(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) == nil {
		a.render(w, r, "landing", nil)
		return
	}
	a.renderDashboard(w, r, dashboardVM{})
}

func (a *App) renderDashboard(w http.ResponseWriter, r *http.Request, vm dashboardVM) {
	u := currentUser(r)
	plans, err := a.store.PlansByOwner(r.Context(), u.ID)
	if err != nil {
		log.Printf("plans by owner: %v", err)
	}
	vm.Plans = plans
	vm.BaseURL = baseURL(r)
	a.render(w, r, "dashboard", vm)
}

// --- Auth ---

func (a *App) handleRegisterForm(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, r, "register", authVM{})
}

func (a *App) handleRegister(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r, maxFormBytes)
	username := normalizeUsername(r.FormValue("username"))
	pw := r.FormValue("password")
	pw2 := r.FormValue("password2")

	if err := validateUsername(username); err != nil {
		a.render(w, r, "register", authVM{Err: "Nama pengguna tidak valid: " + err.Error() + ".", Username: username})
		return
	}
	switch {
	case len(pw) < 8:
		a.render(w, r, "register", authVM{Err: "Kata sandi minimal 8 karakter.", Username: username})
		return
	case len(pw) > maxPasswordLen:
		a.render(w, r, "register", authVM{Err: "Kata sandi terlalu panjang (maksimal 72 karakter).", Username: username})
		return
	case pw != pw2:
		a.render(w, r, "register", authVM{Err: "Konfirmasi kata sandi tidak cocok.", Username: username})
		return
	}

	hash, err := hashPassword(pw)
	if err != nil {
		log.Printf("hash password: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	u, err := a.store.CreateUser(r.Context(), username, hash)
	if errors.Is(err, ErrUsernameTaken) {
		a.render(w, r, "register", authVM{Err: "Nama pengguna sudah dipakai. Pilih yang lain atau masuk.", Username: username})
		return
	}
	if err != nil {
		log.Printf("create user: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := a.startSession(w, r, u.ID); err != nil {
		log.Printf("start session: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, r, "login", authVM{})
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r, maxFormBytes)
	username := normalizeUsername(r.FormValue("username"))
	pw := r.FormValue("password")

	u, hash, err := a.store.UserByUsername(r.Context(), username)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !checkPassword(hash, pw)) {
		a.render(w, r, "login", authVM{Err: "Nama pengguna atau kata sandi salah.", Username: username})
		return
	}
	if err != nil {
		log.Printf("login lookup: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := a.startSession(w, r, u.ID); err != nil {
		log.Printf("start session: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r, maxFormBytes)
	a.endSession(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- Cashplans (owner) ---

func (a *App) handleCreate(w http.ResponseWriter, r *http.Request) {
	u := a.requireUser(w, r)
	if u == nil {
		return
	}
	limitBody(w, r, maxFormBytes)
	title := strings.TrimSpace(r.FormValue("title"))
	desc := strings.TrimSpace(r.FormValue("description"))
	slug := normalizeSlug(r.FormValue("slug"))
	// Fall back to a slug derived from the title if none was provided.
	if slug == "" {
		slug = normalizeSlug(title)
	}

	fail := func(msg string) {
		a.renderDashboard(w, r, dashboardVM{Err: msg, FTitle: title, FSlug: r.FormValue("slug"), FDesc: desc})
	}
	switch {
	case title == "":
		fail("Nama cashplan wajib diisi.")
		return
	case tooLong(title, maxTitleLen):
		fail("Nama cashplan terlalu panjang (maksimal 200 karakter).")
		return
	case tooLong(desc, maxDescLen):
		fail("Deskripsi terlalu panjang (maksimal 1000 karakter).")
		return
	case slug == "":
		fail("Tautan (slug) wajib diisi.")
		return
	}
	if err := validateSlug(slug); err != nil {
		fail("Tautan tidak valid: " + err.Error() + ".")
		return
	}

	plan, err := a.store.CreatePlan(r.Context(), u.ID, slug, title, desc)
	if errors.Is(err, ErrSlugTaken) {
		fail("Tautan \"" + slug + "\" sudah dipakai. Pilih yang lain.")
		return
	}
	if err != nil {
		log.Printf("create plan: %v", err)
		http.Error(w, "gagal membuat cashplan", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/kelola/"+plan.Slug, http.StatusSeeOther)
}

// ownedPlan loads a plan by slug and confirms the current user owns it.
// Returns nil after writing an appropriate response when access is denied.
func (a *App) ownedPlan(w http.ResponseWriter, r *http.Request) *CashPlan {
	u := a.requireUser(w, r)
	if u == nil {
		return nil
	}
	plan, err := a.store.PlanBySlug(r.Context(), r.PathValue("slug"))
	if err != nil || plan.OwnerID != u.ID {
		a.notFound(w, r) // don't reveal existence of plans you don't own
		return nil
	}
	return plan
}

func (a *App) handleManage(w http.ResponseWriter, r *http.Request) {
	plan := a.ownedPlan(w, r)
	if plan == nil {
		return
	}
	a.renderManage(w, r, plan, "")
}

func (a *App) renderManage(w http.ResponseWriter, r *http.Request, plan *CashPlan, errMsg string) {
	sum, err := a.store.SummaryFor(r.Context(), plan.ID)
	if err != nil {
		log.Printf("summary: %v", err)
	}
	payers, err := a.store.PayerBreakdown(r.Context(), plan.ID)
	if err != nil {
		log.Printf("payer breakdown: %v", err)
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	ep, err := a.store.EntriesPage(r.Context(), plan.ID, q, atoiDefault(r.URL.Query().Get("page"), 1))
	if err != nil {
		log.Printf("entries page: %v", err)
	}
	a.render(w, r, "manage", manageVM{
		Plan:     plan,
		Summary:  sum,
		Payers:   payers,
		History:  buildHistoryView(ep, q, "/kelola/"+plan.Slug, true),
		ShareURL: baseURL(r) + "/p/" + plan.Slug,
		Today:    time.Now().In(jakarta).Format("2006-01-02"),
		Uploads:  a.nc.Enabled(),
		Err:      errMsg,
	})
}

func (a *App) handleAddEntry(w http.ResponseWriter, r *http.Request) {
	plan := a.ownedPlan(w, r)
	if plan == nil {
		return
	}
	// Cap the request (fields + optional receipt) and parse the form. The browser
	// sends multipart (the form has a file field); a plain urlencoded post is
	// tolerated too. Only a genuine parse failure (e.g. body too large) errors.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1<<20)
	if err := r.ParseMultipartForm(4 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		a.renderManage(w, r, plan, "Formulir tidak valid atau berkas melebihi batas (maks 5 MB).")
		return
	}

	typ := r.FormValue("type")
	party := strings.TrimSpace(r.FormValue("party"))
	desc := strings.TrimSpace(r.FormValue("description"))
	amount, aErr := parseAmount(r.FormValue("amount"))

	switch {
	case typ != "income" && typ != "expense":
		a.renderManage(w, r, plan, "Jenis transaksi tidak valid.")
		return
	case aErr != nil:
		a.renderManage(w, r, plan, "Jumlah tidak valid: "+aErr.Error()+".")
		return
	case typ == "income" && party == "":
		a.renderManage(w, r, plan, "Pembayar wajib diisi untuk pemasukan.")
		return
	case typ == "expense" && desc == "":
		a.renderManage(w, r, plan, "Keterangan wajib diisi untuk pengeluaran.")
		return
	case tooLong(party, maxPartyLen):
		a.renderManage(w, r, plan, "Nama terlalu panjang (maksimal 200 karakter).")
		return
	case tooLong(desc, maxDescLen):
		a.renderManage(w, r, plan, "Keterangan terlalu panjang (maksimal 1000 karakter).")
		return
	}

	// Optional receipt: only after the entry fields validate, so nothing is
	// uploaded for a submission that gets rejected.
	var attURL, attName string
	if a.nc.Enabled() {
		if file, hdr, err := r.FormFile("receipt"); err == nil {
			defer file.Close()
			url, name, upErr := a.uploadReceipt(r.Context(), plan.Slug, file, hdr)
			if upErr != nil {
				log.Printf("upload receipt: %v", upErr)
				a.renderManage(w, r, plan, "Gagal mengunggah bukti: "+upErr.Error()+". Transaksi belum disimpan.")
				return
			}
			attURL, attName = url, name
		} else if !errors.Is(err, http.ErrMissingFile) && !errors.Is(err, http.ErrNotMultipart) {
			a.renderManage(w, r, plan, "Berkas bukti tidak dapat dibaca.")
			return
		}
	}

	if err := a.store.AddEntry(r.Context(), plan.ID, typ, party, desc, amount, parseDate(r.FormValue("date")), attURL, attName); err != nil {
		log.Printf("add entry: %v", err)
		a.renderManage(w, r, plan, "Gagal menyimpan transaksi.")
		return
	}
	http.Redirect(w, r, "/kelola/"+plan.Slug, http.StatusSeeOther)
}

// uploadReceipt validates and uploads a receipt to Nextcloud, returning its
// public share URL and the (original) display name.
func (a *App) uploadReceipt(ctx context.Context, subfolder string, file multipart.File, hdr *multipart.FileHeader) (string, string, error) {
	if hdr.Size > maxUploadBytes {
		return "", "", fmt.Errorf("berkas melebihi 5 MB")
	}
	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if !allowedReceiptExt[ext] {
		return "", "", fmt.Errorf("tipe berkas tidak didukung (gunakan gambar atau PDF)")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxUploadBytes+1))
	if err != nil {
		return "", "", err
	}
	if int64(len(data)) > maxUploadBytes {
		return "", "", fmt.Errorf("berkas melebihi 5 MB")
	}
	contentType := hdr.Header.Get("Content-Type")
	if contentType == "" {
		contentType = mime.TypeByExtension(ext)
	}
	shareURL, err := a.nc.Upload(ctx, subfolder, hdr.Filename, data, contentType)
	if err != nil {
		return "", "", err
	}
	return shareURL, hdr.Filename, nil
}

// --- Public view ---

func (a *App) handleView(w http.ResponseWriter, r *http.Request) {
	plan, err := a.store.PlanBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		a.notFound(w, r)
		return
	}
	sum, _ := a.store.SummaryFor(r.Context(), plan.ID)
	payers, _ := a.store.PayerBreakdown(r.Context(), plan.ID)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	ep, _ := a.store.EntriesPage(r.Context(), plan.ID, q, atoiDefault(r.URL.Query().Get("page"), 1))
	a.render(w, r, "view", viewVM{
		Plan:     plan,
		Summary:  sum,
		Payers:   payers,
		History:  buildHistoryView(ep, q, "/p/"+plan.Slug, false),
		ShareURL: baseURL(r) + "/p/" + plan.Slug,
	})
}

// --- Report (Laporan Keuangan) ---

func (a *App) handleManageReport(w http.ResponseWriter, r *http.Request) {
	plan := a.ownedPlan(w, r)
	if plan == nil {
		return
	}
	a.renderReport(w, r, plan, false)
}

func (a *App) handleViewReport(w http.ResponseWriter, r *http.Request) {
	plan, err := a.store.PlanBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		a.notFound(w, r)
		return
	}
	a.renderReport(w, r, plan, true)
}

func (a *App) renderReport(w http.ResponseWriter, r *http.Request, plan *CashPlan, public bool) {
	ctx := r.Context()

	// Default range = the plan's full span; fall back to the current month when
	// there are no entries. Overridable via ?from= & ?to=.
	from, to, ok, _ := a.store.PlanPeriod(ctx, plan.ID)
	if !ok {
		now := time.Now().In(jakarta)
		from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, jakarta)
		to = now
	}
	if v := r.URL.Query().Get("from"); v != "" {
		if t, e := time.ParseInLocation("2006-01-02", v, jakarta); e == nil {
			from = t
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, e := time.ParseInLocation("2006-01-02", v, jakarta); e == nil {
			to = t
		}
	}
	if to.Before(from) {
		from, to = to, from
	}

	sum, _ := a.store.SummaryForRange(ctx, plan.ID, from, to)
	incomeCells, _ := a.store.WeeklyBreakdown(ctx, plan.ID, "income", from, to)
	expenseCells, _ := a.store.WeeklyBreakdown(ctx, plan.ID, "expense", from, to)
	expenses, _ := a.store.ExpensesForRange(ctx, plan.ID, from, to)

	base := "/p/" + plan.Slug
	if !public {
		base = "/kelola/" + plan.Slug
	}

	a.render(w, r, "laporan", reportVM{
		Plan:        plan,
		Summary:     sum,
		Income:      buildMatrix(incomeCells),
		Expense:     buildMatrix(expenseCells),
		Expenses:    expenses,
		PeriodLabel: formatTanggal(from) + " – " + formatTanggal(to),
		FromInput:   from.Format("2006-01-02"),
		ToInput:     to.Format("2006-01-02"),
		Generated:   time.Now().In(jakarta),
		Keterangan:  buildKeterangan(sum),
		ShareURL:    baseURL(r) + "/p/" + plan.Slug,
		Public:      public,
		BasePath:    base,
	})
}

// buildKeterangan derives the report's closing note from the balance.
func buildKeterangan(s Summary) string {
	switch {
	case s.Balance < 0:
		return "Terdapat kekurangan dana sebesar " + formatRupiah(-s.Balance) +
			". Selisih ini ditanggung/ditutup di luar kas."
	case s.Balance > 0:
		return "Terdapat sisa saldo sebesar " + formatRupiah(s.Balance) + "."
	default:
		return "Dana terpakai seluruhnya; saldo akhir nihil (Rp 0)."
	}
}

// --- Edit an entry (party/description/date) + version history ---

type editVM struct {
	Plan  *CashPlan
	Entry *Entry
	Today string
	Err   string
}

type entryDetailVM struct {
	Plan      *CashPlan
	Entry     *Entry
	Revisions []Revision
	BasePath  string
	Owner     bool
}

func (a *App) handleEditEntryForm(w http.ResponseWriter, r *http.Request) {
	plan := a.ownedPlan(w, r)
	if plan == nil {
		return
	}
	entry, err := a.store.EntryByID(r.Context(), r.PathValue("id"))
	if err != nil || entry.CashplanID != plan.ID {
		a.notFound(w, r)
		return
	}
	a.render(w, r, "edit", editVM{Plan: plan, Entry: entry, Today: time.Now().In(jakarta).Format("2006-01-02")})
}

func (a *App) handleEditEntry(w http.ResponseWriter, r *http.Request) {
	plan := a.ownedPlan(w, r)
	if plan == nil {
		return
	}
	entry, err := a.store.EntryByID(r.Context(), r.PathValue("id"))
	if err != nil || entry.CashplanID != plan.ID {
		a.notFound(w, r)
		return
	}
	limitBody(w, r, maxFormBytes)
	party := strings.TrimSpace(r.FormValue("party"))
	desc := strings.TrimSpace(r.FormValue("description"))
	date := parseDate(r.FormValue("date"))

	fail := func(msg string) {
		e := *entry // keep entered values on the form
		e.Party, e.Description, e.OccurredAt = party, desc, date
		a.render(w, r, "edit", editVM{Plan: plan, Entry: &e, Today: time.Now().In(jakarta).Format("2006-01-02"), Err: msg})
	}
	switch {
	case entry.IsIncome() && party == "":
		fail("Pembayar wajib diisi.")
		return
	case !entry.IsIncome() && desc == "":
		fail("Keterangan wajib diisi.")
		return
	case tooLong(party, maxPartyLen):
		fail("Nama terlalu panjang (maksimal 200 karakter).")
		return
	case tooLong(desc, maxDescLen):
		fail("Keterangan terlalu panjang (maksimal 1000 karakter).")
		return
	}
	if _, err := a.store.EditEntry(r.Context(), entry.ID, plan.ID, party, desc, date); err != nil {
		log.Printf("edit entry: %v", err)
		fail("Gagal menyimpan perubahan.")
		return
	}
	http.Redirect(w, r, "/kelola/"+plan.Slug+"/entry/"+entry.ID, http.StatusSeeOther)
}

func (a *App) handleManageEntryDetail(w http.ResponseWriter, r *http.Request) {
	plan := a.ownedPlan(w, r)
	if plan == nil {
		return
	}
	a.renderEntryDetail(w, r, plan, "/kelola/"+plan.Slug, true)
}

func (a *App) handleViewEntryDetail(w http.ResponseWriter, r *http.Request) {
	plan, err := a.store.PlanBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		a.notFound(w, r)
		return
	}
	a.renderEntryDetail(w, r, plan, "/p/"+plan.Slug, false)
}

func (a *App) renderEntryDetail(w http.ResponseWriter, r *http.Request, plan *CashPlan, basePath string, owner bool) {
	entry, err := a.store.EntryByID(r.Context(), r.PathValue("id"))
	if err != nil || entry.CashplanID != plan.ID {
		a.notFound(w, r)
		return
	}
	revs, err := a.store.EntryRevisions(r.Context(), entry.ID)
	if err != nil {
		log.Printf("entry revisions: %v", err)
	}
	a.render(w, r, "versions", entryDetailVM{Plan: plan, Entry: entry, Revisions: revs, BasePath: basePath, Owner: owner})
}

// --- Helpers ---

func (a *App) requireUser(w http.ResponseWriter, r *http.Request) *User {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return nil
	}
	return u
}

// parseDate reads an <input type="date"> value (YYYY-MM-DD), falling back to
// today in Jakarta time when empty or malformed.
func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s != "" {
		if t, err := time.ParseInLocation("2006-01-02", s, jakarta); err == nil {
			return t
		}
	}
	return time.Now().In(jakarta)
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}
