# Cashflow — Catatan & Rencana (Notes & Backlog)

Living backlog so we don't lose context. Newest context at top of each section.

_Last updated: 2026-09-05_

---

## A. Security review (2026-09-05)

**Verdict: no critical/high findings.** Fundamentals are solid — all SQL is
parameterized, `html/template` auto-escapes everything (no raw-HTML sinks),
ownership checks prevent IDOR, passwords are bcrypt-hashed, sessions are random
+ HttpOnly/SameSite=Lax/Secure-on-HTTPS, and the append-only DB trigger holds.
Everything below is medium-and-below **hardening** — do before exposing to the
public internet.

### Quick-win bundle — ✅ DONE 2026-09-05
`M1, M2, M4, L3, L5, L8` implemented:
- **M1** — `limitBody` (256 KiB) on register/login/logout/create; entry keeps its
  5 MB upload cap; server-side rune-length caps on title/desc/party.
- **M2** — `securityHeaders` middleware: strict CSP (`default-src 'self'`; img
  `data:`; `frame-ancestors 'none'`), X-Frame-Options, nosniff, Referrer-Policy.
  Removed the one inline `onclick` so the CSP needs no `unsafe-inline`.
- **M4** — Dockerfile pinned to `golang:1.26.6-alpine`. Verified: the deployed
  binary reports `go1.26.6` and binary-mode govulncheck finds **no stdlib vulns**.
  (govulncheck binary mode also flags GO-2026-5932 `x/crypto/openpgp` — a FALSE
  POSITIVE: `go list -deps` shows openpgp is not compiled in; we only use bcrypt.)
  NOTE: bump the **local** dev Go to 1.26.6 for parity — local is still 1.26.5,
  so `govulncheck ./...` on the source will keep flagging the (already-shipped-fixed)
  stdlib advisories.
- **L3** — password capped at 8–72 chars (bcrypt limit) with a friendly message.
- **L5** — `pruneSessions` goroutine deletes expired sessions at startup + hourly.
- **L8** — Read/Write/Idle timeouts added (generous, to not break uploads).

**Still open (next security pass):** M3 (login rate-limiting), L1 (trusted-proxy /
canonical BASE_URL), L2 (CSRF tokens), L4 (enum/timing), L6 (least-priv DB role),
L7 (prod DB creds / don't expose 5432).

### Medium
- **M1 · No request body size limit (DoS).** POST handlers call `r.FormValue`
  with no `http.MaxBytesReader` and no server-side length caps (`maxlength` is
  client-side only). Fix: middleware wrapping body in `MaxBytesReader` (~64KB) +
  server-side length validation on title/description/party/note.
- **M2 · Missing security headers (clickjacking / MIME-sniff).** `main.go` sets
  none. Fix: middleware adding `X-Frame-Options: DENY`,
  `X-Content-Type-Options: nosniff`, `Content-Security-Policy: default-src 'self'`,
  `Referrer-Policy: same-origin`. (CSP `default-src 'self'` is fine — all CSS/JS
  is same-origin.)
- **M3 · No brute-force protection on login.** `handleLogin` allows unlimited
  attempts. Fix: per-IP + per-username rate limiting / backoff / lockout.
- **M4 · Go toolchain has 5 reachable stdlib CVEs.** `govulncheck` flags 5 issues
  fixed in **go1.26.6** (built on 1.26.5): net/http HTTP/2 ReadHeaderTimeout,
  crypto/tls, encoding/xml & asn1 recursion guards — all DoS-class. Fix: bump
  Dockerfile base to `golang:1.26.6-alpine`+ and rebuild.

### Low / hardening
- **L1 · `X-Forwarded-Host`/`-Proto` trusted unconditionally** (`baseURL`,
  `isHTTPS`) → share-link / base-URL poisoning; can flip the Secure-cookie
  decision if exposed without a header-stripping proxy. Fix: configured canonical
  `BASE_URL`, or only honor forwarded headers from trusted proxies.
- **L2 · CSRF relies only on SameSite=Lax** — no per-form tokens. Fix: add
  synchronizer / double-submit CSRF tokens on state-changing POSTs.
- **L3 · Passwords >72 bytes throw a 500** (Go bcrypt limit); no upper bound.
  Fix: validate password 8–64 chars before hashing with a friendly message.
- **L4 · Username enumeration + login timing** — registration reveals "taken";
  login skips bcrypt for unknown users (timing oracle). Low (usernames semi-public).
  Fix (optional): dummy bcrypt compare for unknown users.
- **L5 · Sessions never pruned** — expired rows only filtered, never deleted; each
  login adds a row. Fix: periodic `DELETE FROM sessions WHERE expires_at < now()`.
- **L6 · App DB role can drop the append-only trigger** (defense-in-depth) — app
  connects as schema owner. Fix: least-privilege role with only `SELECT, INSERT`
  on `entries`, separate from migration/owner role.
- **L7 · Weak default DB creds + exposed 5432** in compose — fine locally. Fix:
  require strong `DATABASE_URL` in prod, don't publish 5432, document it.
- **L8 · Only `ReadHeaderTimeout` set** on `http.Server` (Slowloris). Fix: add
  `ReadTimeout` / `WriteTimeout` / `IdleTimeout`.

### Verified good (no action)
Parameterized SQL · LIKE-injection escaped (`ESCAPE '\'`) · XSS auto-escaped, no
`template.HTML` · IDOR guarded (`ownedPlan` → 404, no existence leak) · bcrypt
cost 10 + generic login error + 32-byte random sessions + fresh token per login
(no fixation) · cookie HttpOnly/SameSite=Lax/Secure-on-HTTPS + real server-side
logout · append-only DB trigger · no open redirect · no secrets in logs/URLs ·
static served from traversal-safe `embed.FS`.

---

## B. Feature: Laporan Keuangan (printable report)

**Goal:** generate a designed, printable financial report for a cashplan,
like the reference the user shared (al-zahra / "KORLAS 1B" class-fund report).

> **STATUS (2026-09-05): being built now.** v1 = in-app printable HTML page at
> `/kelola/{slug}/laporan` (owner) + `/p/{slug}/laporan` (public), all data
> derived from existing entries; auto keterangan from balance. Deferred to later:
> custom org/logo/signatories, custom keterangan field, flow diagram, dues mode,
> and the Lampiran section (waits on feature C).

**Reference layout (sections in the example):**
1. Header/branding — org name + logo, report title, **period** ("Sep 2025 – Mei
   2026"), one-line purpose.
2. **Pemasukan** — summary (example used fixed dues: jumlah siswa × iuran per
   anak = total). Our generic model → total pemasukan + jumlah pembayar +
   per-payer breakdown.
3. **Pengeluaran** — numbered table: No | Keterangan | Nominal, with total.
4. **Rekapitulasi** — Total Pemasukan / Total Pengeluaran / Saldo Akhir (shows
   "(KURANG)" in red when negative).
5. **Keterangan** — free-text notes (e.g. "kekurangan ditutup oleh KORLAS").
6. **Skema Alur Keuangan** — optional 5-step flow diagram (Pemasukan → Kas →
   Pengeluaran → Hasil Akhir → Penutup).
7. **Lampiran Bukti Transaksi** — **DECISION (2026-09-05): do NOT embed receipts
   inline in the report** — with ~100 txns each having a receipt the report would
   be enormous. The per-row "📎 bukti" links in the history already give access.
   If revisited, do a compact links-only appendix, not inline images.
8. Footer — thank-you + signatories (e.g. "KORLAS 1B: Hasni, Echa, Cendana, Desty").

**Proposed implementation:**
- Route `GET /kelola/{slug}/laporan` (owner) and optionally `GET /p/{slug}/laporan`
  (public), rendering a print-optimized HTML page (A4 print CSS → "Save as PDF"
  from the browser; no new dependency). PDF-lib export can come later if needed.
- Report inputs the owner fills in: org/title, logo (optional), period (default =
  min/max `occurred_at`, overridable), free-text keterangan, signatories. Store
  these on the cashplan (new nullable columns) or pass at render time.
- Data comes from existing queries (summary, per-payer breakdown, expense list).

**Gaps / decisions:**
- Example assumes **fixed per-head dues** (siswa × iuran). Our model is per-entry.
  Decide: keep generic (total + per-payer), OR add optional "dues mode" (set a
  headcount + amount-per-head).
- Format: in-app printable HTML page (recommended) vs generated PDF file.
- Could first produce a visual **mockup as an Artifact** to lock the design, then
  wire real data in-app.

---

## C. Feature: File attachments (receipts) on entries — via Nextcloud

**Goal:** attach a receipt/photo to income & expense entries; the cashflow app
**stores only the link**, not the file. Feeds the report's "Lampiran Bukti
Transaksi" section (feature B.7).

**Nextcloud feasibility: YES.** **DECISION (2026-09-05): Option A chosen. BUILT
2026-09-05** — upload-on-submit: the receipt rides in the entry's multipart POST;
backend validates fields, then uploads to Nextcloud (WebDAV PUT + OCS public
share) and stores only the share link on the entry. Config via `.env`
(`NEXTCLOUD_URL/USER/APP_PASSWORD/FOLDER`); disabled if unset (file field hidden).
Attachment columns are on `entries` (immutable, set at INSERT). Receipt link
shown in history as "📎 bukti". TODO next: surface receipts in the report's
Lampiran section; note public-share privacy (see below). Options:

- **Option A (CHOSEN): backend uploads to Nextcloud via WebDAV.**
  On entry create, the Go backend `PUT`s the file to
  `https://<nc>/remote.php/dav/files/<user>/<folder>/<slug>/<token>-<name>`
  (grouped per cashplan by slug since 2026-09-05; folders created via MKCOL per
  level) (auth via a
  Nextcloud **app password**), then creates a public share link via the OCS Share
  API (`POST /ocs/v2.php/apps/files_sharing/api/v1/shares`, `shareType=3`), and
  stores the returned share URL (+ remote path) in the DB. App holds only the link.
  - Config: `NEXTCLOUD_URL`, `NEXTCLOUD_USER`, `NEXTCLOUD_APP_PASSWORD`,
    `NEXTCLOUD_FOLDER`. Optional: share password / expiry.
- **Option B: user pastes a share link.** User uploads to Nextcloud themselves and
  pastes the public link into a URL field on the entry. Simplest, no creds, worse UX.
- (Alt) local disk or S3-compatible storage — but user specifically asked for
  Nextcloud.

**Schema:** simplest = add nullable `attachment_url text` + `attachment_name text`
to `entries`. If multiple files per entry are wanted → separate `attachments`
table (entry_id, url, name, created_at).

**Truthfulness:** attachment link should be **immutable** like the entry (set at
creation, covered by the append-only trigger if on `entries`; add equivalent
protection if a separate table).

**Security to mind:** validate file type + size (server-side) [done: 5 MB, image/PDF
allowlist, MaxBytesReader on the entry POST]; never expose Nextcloud creds to the
browser [done: upload is server-side]; store only the link [done].
- **PRIVACY (important):** receipts are shared via **public** Nextcloud links
  (`shareType=3`), so anyone with the cashplan link can open them. The reference
  report's receipts contain bank account numbers — same public-by-link exposure
  as the rest of the history. Future options: password-protected shares, share
  expiry, or gating receipt links behind login instead of the public view.

**Decisions needed:**
1. Do you have a Nextcloud instance + can you create an app password? → picks A vs B.
2. One file per entry, or multiple?
3. Upload at entry-creation only (immutable), or allow adding later?

---

## D. Other queued ideas (from earlier)
- Masuk/Keluar **type filter** on history (alongside search).
- **Edit plan** title/description (entries stay locked/append-only).
- **"Terbilang"** hint under the amount field (spell out the number).
- Report/plan **currency** always IDR integer (no decimals) — keep.
