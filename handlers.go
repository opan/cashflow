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
	maxUploadBytes = 2 << 20   // 2 MiB per receipt
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
	}
	pages := map[string]*template.Template{}
	// Each page gets its own template set (layout + partials + that page) so
	// their "content"/"title" blocks don't collide.
	for _, name := range []string{"landing", "login", "register", "dashboard", "manage", "view", "laporan", "notfound"} {
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

func buildHistoryView(ep EntryPage, q, basePath string) HistoryView {
	hv := HistoryView{
		Entries:    ep.Entries,
		Query:      q,
		Page:       ep.Page,
		TotalPages: ep.TotalPages,
		Total:      ep.Total,
		BasePath:   basePath,
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
	Plan       *CashPlan
	Summary    Summary
	Payers     []PayerTotal
	Expenses   []Entry
	Period     string
	Generated  time.Time
	Keterangan string
	ShareURL   string
	Public     bool // true when rendered from the public /p/ route
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
		History:  buildHistoryView(ep, q, "/kelola/"+plan.Slug),
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
		a.renderManage(w, r, plan, "Formulir tidak valid atau berkas melebihi batas (maks 2 MB).")
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
		} else if !errors.Is(err, http.ErrMissingFile) {
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
		return "", "", fmt.Errorf("berkas melebihi 2 MB")
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
		return "", "", fmt.Errorf("berkas melebihi 2 MB")
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
		History:  buildHistoryView(ep, q, "/p/"+plan.Slug),
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
	sum, _ := a.store.SummaryFor(ctx, plan.ID)
	payers, _ := a.store.PayerBreakdown(ctx, plan.ID)
	expenses, _ := a.store.ExpensesFor(ctx, plan.ID)
	start, end, ok, _ := a.store.PlanPeriod(ctx, plan.ID)

	period := "—"
	if ok {
		if start.Year() == end.Year() && start.Month() == end.Month() {
			period = formatBulanTahun(start)
		} else {
			period = formatBulanTahun(start) + " – " + formatBulanTahun(end)
		}
	}

	a.render(w, r, "laporan", reportVM{
		Plan:       plan,
		Summary:    sum,
		Payers:     payers,
		Expenses:   expenses,
		Period:     period,
		Generated:  time.Now().In(jakarta),
		Keterangan: buildKeterangan(sum),
		ShareURL:   baseURL(r) + "/p/" + plan.Slug,
		Public:     public,
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
