# Cashflow

A simple web app for tracking shared cash flow — collaborative, transparent, and
tamper-proof. Sign up, create a **cashplan** (e.g. "Kids' school cash fund") with a
link of your choosing, record income & expenses, then share the link. History is
**append-only** — it cannot be edited or deleted, so everyone sees the same data.

> The application UI is in Indonesian. This document is available in both languages:
> [Bahasa Indonesia](README.md) · **English**

## Features

- **User accounts** (username + password). Usernames are unique, first-come-first-served.
  Cashplans are stored under your account, so the manage link is never lost — just log
  in to get back to it.
- **Create a cashplan** with a name, description, and a **link (slug) of your own
  choosing** (e.g. `/p/uang-kas-3a`). Slugs are unique across cashplans,
  first-come-first-served.
- **A single transaction form** with a Type selector (Income/Expense):
  - Income: **payer**, amount, date (auto-filled, editable), notes.
  - Expense: **description** (e.g. "buy t-shirts"), **payee** (optional), amount, date.
- **Summary**: total income, total expenses, balance, number of payers, number of transactions.
- **Per-payer breakdown**: total, number of deposits, and last deposit for each payer
  (income only), sorted from the largest contributor.
- **Search & pagination** on the history: search by payer or description, with
  pagination (20 per page) since history can get long.
- **Share** a cashplan via the `/p/{slug}` link — the (logged-in) owner can edit,
  anyone with the link can view.
- **Append-only history** — enforced at the database level (see below).

## Tech stack

- **Backend + frontend**: Go (single binary) — `net/http` (built-in routing in Go
  1.22+), `html/template` for server-side rendering, a little vanilla JS (copy link,
  dynamic transaction form, slug suggestions).
- **Database**: PostgreSQL (`jackc/pgx/v5` driver).
- **Auth**: passwords hashed with `bcrypt`; cookie-based sessions (httpOnly,
  SameSite=Lax) stored in the `sessions` table.
- Templates & static assets are *embedded* into the binary (`go:embed`) → a single
  binary file.

## Running

### Option A — Docker Compose (easiest)

```bash
docker compose up --build
```

Open <http://localhost:8080>, then **Register** to create an account. Postgres runs
automatically inside compose; the schema is created automatically on app start.

### Option B — Local (your own Go + Postgres)

Requires Go 1.26+ and an accessible Postgres.

```bash
docker compose up -d db     # start the database only
export DATABASE_URL="postgres://cashflow:cashflow@localhost:5432/cashflow?sslmode=disable"
export PORT=8080
go run .
```

## Configuration

| Variable       | Default                                                                 | Description    |
| -------------- | ----------------------------------------------------------------------- | -------------- |
| `DATABASE_URL` | `postgres://cashflow:cashflow@localhost:5432/cashflow?sslmode=disable`   | PostgreSQL DSN |
| `PORT`         | `8080`                                                                   | HTTP port      |

## Routes

| Method + Path                | Access                   | Function                                    |
| ---------------------------- | ------------------------ | ------------------------------------------- |
| `GET  /`                     | public                   | Landing (logged out) / Dashboard (logged in)|
| `GET/POST /register`         | public                   | Register an account                         |
| `GET/POST /login`            | public                   | Log in                                      |
| `POST /logout`               | logged in                | Log out                                     |
| `POST /cashplans`            | logged in                | Create cashplan (title, slug, description)  |
| `GET  /kelola/{slug}`        | owner                    | Manage: add transactions, summary, share    |
| `POST /kelola/{slug}/entry`  | owner                    | Add transaction (income/expense)            |
| `GET  /p/{slug}`             | anyone with the link     | View (read-only) summary & history          |

## Access model (design decision)

- **Ownership = account.** Every cashplan is owned by the user who created it. To
  create/manage a cashplan, the user must be logged in. The dashboard shows all of the
  user's cashplans, so the manage link is never lost.
- **Sharing = public slug.** The view link `/p/{slug}` is accessible to anyone without
  logging in. Managing (`/kelola/{slug}`) is for the logged-in owner only; accessing
  someone else's cashplan returns 404 (its existence is not leaked).

> **Security note (to follow up on later):** slugs are user-chosen and therefore
> guessable. Since view mode is public-by-link anyway, this is low risk for now.
> Editing remains safe because it's protected by login + ownership. Future improvements
> could include: a private cashplan option, login rate-limiting, and explicit CSRF
> tokens (currently relying on SameSite=Lax).

## Integrity guarantee (truthfulness)

Data truthfulness is enforced at the **database** level by the `entries_guard` trigger:

1. Entries **cannot be deleted**, and the **amount and type** (income/expense)
   **cannot be changed** — locked so the numbers can't be manipulated.
2. Only the **payer/payee, description, and date** can be corrected. Every change saves
   the old values into the append-only `entry_revisions` table, so the **version history
   stays intact and visible to everyone** (an "edited" label + a version timeline on
   each record).
3. `entry_revisions` itself is append-only (cannot be `UPDATE`d/`DELETE`d).

All of these rules apply even from outside the application (e.g. via `psql`) — trying to
change amount/type or delete an entry will be rejected by the database.

## File structure

```
main.go            Configuration, DB connection, routing, middleware
handlers.go        HTTP handlers + template rendering
auth.go            Password hashing (bcrypt), cookie sessions, user middleware
store.go           Data access layer (users, sessions, cashplans, entries)
slug.go            Slug normalization & validation
money.go           Rupiah parsing & formatting, Indonesian date formatting
schema.sql         DB schema (applied automatically on start)
templates/         html/template (layout, partials, pages)
static/            style.css, app.js
Dockerfile         Build the binary (multi-stage, small image)
docker-compose.yml Postgres + application
```

## Data model

```
users(id, username, password_hash, created_at)
sessions(id, user_id, created_at, expires_at)
cashplans(id, owner_id, slug, title, description, created_at)
entries(id, cashplan_id, type['income'|'expense'], party, description,
        amount /* whole rupiah, immutable */, occurred_at, created_at,
        attachment_url, attachment_name)
entry_revisions(id, entry_id, party, description, occurred_at, revised_at)
```

`party` = payer (income) / payee (expense). `description` = notes (income) /
reason-description (expense).

## Notes

- Money amounts are stored as whole-rupiah integers (`bigint`) — no decimals, as is
  customary for IDR — so there are no floating-point rounding errors.
- Dates default to the `Asia/Jakarta` (WIB) time zone.
- Full reset (delete all data): `docker compose down -v`.
