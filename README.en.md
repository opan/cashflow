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
- **Log in through All-in-one** (optional): one account for cashflow and the other apps that
  use All-in-one, with single sign-on. See
  [Logging in through All-in-one](#logging-in-through-all-in-one-optional).
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
  SameSite=Lax) stored in the `sessions` table. Optional: log in through All-in-one with
  OpenID Connect (`coreos/go-oidc` + `x/oauth2`).
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

Other variables (Nextcloud attachment uploads, rate limiting, OpenTelemetry metrics) are in
[`.env.example`](.env.example). The variables for logging in through All-in-one are described
[below](#all-in-one-login-configuration).

## Routes

| Method + Path                | Access                   | Function                                    |
| ---------------------------- | ------------------------ | ------------------------------------------- |
| `GET  /`                     | public                   | Landing (logged out) / Dashboard (logged in)|
| `GET/POST /register`         | public                   | Register an account (with `AUTH_PROVIDER=aio`: redirected into the All-in-one flow) |
| `GET/POST /login`            | public                   | Log in (with `AUTH_PROVIDER=aio`: redirected into the All-in-one flow) |
| `GET  /auth/login`           | public                   | Start a login at All-in-one (`?next=`, `?signup=1`) |
| `GET  /auth/callback`        | public                   | Back from All-in-one: exchange the code, start a session |
| `POST /logout`               | logged in                | Log out (with `AUTH_PROVIDER=aio`: also logs out of All-in-one) |
| `POST /cashplans`            | logged in                | Create cashplan (title, slug, description)  |
| `GET  /kelola/{slug}`        | owner                    | Manage: add transactions, summary, share    |
| `POST /kelola/{slug}/entry`  | owner                    | Add transaction (income/expense)            |
| `GET  /p/{slug}`             | anyone with the link     | View (read-only) summary & history          |

## Logging in through All-in-one (optional)

Cashflow can use **All-in-one (aio)** as its login service through OpenID Connect. It is off by
default (`AUTH_PROVIDER=local`): signing up and logging in keep using cashflow's own username +
password, exactly as before.

With `AUTH_PROVIDER=aio`:

- **Masuk** (log in) and **Daftar** (sign up) take users to aio's login page. That page is
  presented as Cashflow: the 💰 icon, teal colour and Indonesian text, with a small "Login
  diamankan oleh All-in-one" (login secured by All-in-one) note. Cashflow never sees passwords.
- Users who are already logged in to aio get straight in, with no form (*single sign-on*).
- **Keluar** (log out) in cashflow also ends the aio session.
- The local password forms are no longer used: `POST /login` and `POST /register` redirect into
  the aio flow.

> ⚠️ **Already have users? Move their accounts to aio first** before turning on
> `AUTH_PROVIDER=aio` (see [Moving existing users](#moving-existing-users-to-all-in-one)). Without
> that, old passwords stop working and logging in through aio creates a new, empty account.

### Login flow

```mermaid
sequenceDiagram
    autonumber
    participant B as Browser
    participant C as Cashflow
    participant A as All-in-one
    B->>C: GET /login (click "Masuk")
    C->>B: Redirect to /auth/login, then to aio /authorize
    Note over C: Keep state, nonce and the PKCE verifier<br/>in the cashflow_oidc cookie (10 minutes)
    B->>A: GET /api/v1/oauth2/authorize (client_id, PKCE, ui_locales=id)
    A->>B: Redirect to the hand-off page /oauth/login
    alt Not logged in to aio
        B->>A: aio login/signup form (presented as Cashflow)
    end
    B->>A: Complete the login request (only from the browser that started it)
    A->>B: Redirect to cashflow /auth/callback?code=...&state=...
    B->>C: GET /auth/callback
    C->>A: POST /api/v1/oauth2/token (code + client secret + PKCE verifier)
    A->>C: ID token
    Note over C: Verify signature, issuer, audience,<br/>expiry and nonce. Find or create the local user.
    C->>B: Redirect to the target page + cashflow session cookie
```

1. **Cashflow starts** (`/auth/login`): creates `state`, `nonce` and a PKCE verifier, keeps them in
   the `cashflow_oidc` cookie (HttpOnly, 10 minutes), and redirects to aio with `ui_locales=id`
   (plus `prompt=create` for **Daftar**).
2. **aio checks the request** (registered client, matching `redirect_uri`, PKCE S256), ties it to
   this browser, and shows its hand-off page. Without an aio session the user logs in or signs up
   at aio; with one, this step is skipped.
3. **aio returns a one-time code** to `/auth/callback`.
4. **Cashflow finishes** (`/auth/callback`): matches `state` against the cookie, exchanges the code
   for an ID token **server to server** (client secret + PKCE verifier), verifies the ID token with
   aio's public keys, and starts its own session. After that, cashflow doesn't call aio again until
   the user logs out.

### Logout flow

1. `POST /logout`: cashflow deletes its session, then redirects to aio's `end_session` with the ID
   token kept from login (`id_token_hint`) and `post_logout_redirect_uri`.
2. aio ends its session (only if that ID token belongs to the user of the aio session in that
   browser), then redirects back to cashflow.

If aio can't be reached at logout time, cashflow still logs the user out of cashflow.

### User accounts

- A cashflow user is linked to an aio account by the ID token's `sub` (`users.aio_user_id`) and is
  created automatically on first login, without a local password.
- The username comes from aio's `preferred_username`, adjusted to cashflow's rules (lowercase
  letters, digits, `_`, 3–30 characters).
  - Already used by another aio account (e.g. a renamed one): an `_xxxxxxxx` suffix is added.
  - Already used by an **existing (local) cashflow account**: the login is refused. Existing
    accounts are never taken over automatically; move them first with `users:import` (below).

### Setup

1. **In aio**: turn on the provider (`auth.oidc.enabled: true` and `auth.oidc.issuer`, aio's public
   URL). Run aio as a **single replica** (`Recreate` strategy): login requests are kept in memory.
2. **Register cashflow in aio**, with its branding:

   ```bash
   all-in-one oidc:client:create --id cashflow --name Cashflow \
     --redirect-uri https://cashflow.example.com/auth/callback \
     --post-logout-redirect-uri https://cashflow.example.com/ \
     --brand-color '#0f766e' --icon 💰
   ```

   Keep the client secret it prints; it is shown only once. The branding (name, colour, icon) can
   be changed any time with `all-in-one oidc:client:update cashflow --brand-color ... --icon ...`.
3. **In cashflow**: set the variables below and restart.

For local development, run aio and cashflow on different hosts, e.g. aio on
`http://127.0.0.1:18080` and cashflow on `http://localhost:8090` (aio accepts `http` redirect URIs
only for loopback addresses), with `AIO_REDIRECT_URL=http://localhost:8090/auth/callback`.

### Moving existing users to All-in-one

The `all-in-one users:import` command copies cashflow's local accounts (username + bcrypt password
hash, as-is) into aio, then links them in cashflow (`users.aio_user_id`). As a result:

- **Passwords stay the same.** Users log in on aio's page with the username and password they
  already use. Nobody re-registers or resets a password.
- **Data doesn't move.** Cashplans, entries and dues stay in cashflow's database; `users` rows keep
  their ids and only gain a link to the aio account.
- **Current sessions keep working** until they expire (30 days); after that, users log in through
  aio.

The command reads cashflow's database directly (cashflow and aio usually share one Postgres
server) from the `CASHFLOW_DATABASE_URL` environment variable rather than a flag, so the database
password stays out of the process list. Without `--apply` it only prints the plan.

1. **Deploy the cashflow version with All-in-one login, keeping `AUTH_PROVIDER=local`.** On start,
   cashflow adds the `aio_user_id` column; users notice nothing.
2. **Register cashflow in aio** (step 2 of [Setup](#setup)) and prepare the `AIO_*` variables, but
   don't change `AUTH_PROVIDER` yet.
3. **Dry run**, with aio's configuration (see the Kubernetes Job example below):

   ```bash
   export CASHFLOW_DATABASE_URL='postgres://cashflow:...@<pg-server>:5432/cashflow?sslmode=disable'
   all-in-one users:import
   ```

   ```
   USERNAME  RESULT           AIO ACCOUNT                           NOTE
   budi      created          59d879b8-8c96-43dc-91c9-9a14dcef7fbe
   siti      skipped          -                                     aio already has this username for another account: ...
   admin     skipped          -                                     reserved in aio (aio's bootstrap admin, ...): rename it in cashflow first
   opan      skipped          -                                     aio already has this username for another account: ...
   ```

4. **Resolve the skipped accounts.** `--apply` writes nothing while any account is skipped.

   | Cause | What to do |
   | --- | --- |
   | Username already in aio, **same person** (e.g. your own account) | Add `--link-existing <name>`. The cashflow account is linked to that aio account; it logs in with its **aio password**. |
   | Username already in aio, **someone else** | Rename it in cashflow: `UPDATE users SET username = 'siti_kas' WHERE username = 'siti';` and tell the user their new username. |
   | aio's bootstrap admin name (`rbac.admin_username`, e.g. `admin`) | Rename it in cashflow (aio grants the admin group to this name when it has no admin), or `--link-existing` if it really is your aio admin account. |
   | aio's shared demo account (`demo_mode.username`) | Rename it in cashflow; the demo account can't log in to other apps. |
   | Invalid username or hash | Fix the data in cashflow. |

5. **Run it for real:** `all-in-one users:import --apply` (plus `--link-existing ...` if needed).
   The aio accounts are created in one transaction, then the links are written in cashflow in one
   transaction. Re-running is safe: accounts already moved are recognised and only linked.

   ```
   Imported into aio: 3 new, 0 already there, 1 linked to existing accounts. Linked in cashflow: 4.
   Every cashflow account is linked: cashflow can switch to AUTH_PROVIDER=aio.
   ```

6. **Switch to `AUTH_PROVIDER=aio`** and restart cashflow.

**Going back to local login** (if needed): set `AUTH_PROVIDER=local` again. Cashflow still keeps the
local password hashes, so everyone can log in as before (password changes made in aio after the
switch don't carry back). The aio accounts do no harm and can stay.

<details>
<summary>Kubernetes Job example (aio's image is distroless, so <code>kubectl exec</code> won't do)</summary>

The Job uses the same image, configuration and secrets as aio's deployment, and takes
`CASHFLOW_DATABASE_URL` from cashflow's secret. Run it without `--apply` first, read the log
(`kubectl -n app logs job/all-in-one-users-import`), delete the Job, then run it again with
`--apply`.

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: all-in-one-users-import
  namespace: app
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: users-import
          image: opanmustopah/all-in-one:latest
          args: ["users:import"]   # then: ["users:import", "--apply", "--link-existing", "opan"]
          env:
            - name: CASHFLOW_DATABASE_URL
              valueFrom: { secretKeyRef: { name: cashflow-secrets, key: DATABASE_URL } }
            - name: ALLINONE_AUTH_JWT_SECRET
              valueFrom: { secretKeyRef: { name: all-in-one-secrets, key: jwt-secret } }
            - name: ALLINONE_AUTH_TOTP_ENCRYPTION_KEY
              valueFrom: { secretKeyRef: { name: all-in-one-secrets, key: totp_encryption_secret } }
            - { name: ALLINONE_STORAGE_TYPE, value: "postgres" }
            - { name: ALLINONE_STORAGE_POSTGRES_HOST, value: "192.168.68.124" }
            - { name: ALLINONE_STORAGE_POSTGRES_PORT, value: "5432" }
            - { name: ALLINONE_STORAGE_POSTGRES_USER, value: "allinone" }
            - { name: ALLINONE_STORAGE_POSTGRES_DBNAME, value: "allinone" }
            - { name: ALLINONE_STORAGE_POSTGRES_SSLMODE, value: "disable" }
            - name: ALLINONE_STORAGE_POSTGRES_PASSWORD
              valueFrom: { secretKeyRef: { name: all-in-one-secrets, key: postgres_password } }
          volumeMounts:
            - { name: config, mountPath: /app/config, readOnly: true }
      volumes:
        - name: config
          configMap: { name: all-in-one-config }
```

</details>

### All-in-one login configuration

| Variable              | Example                                      | Description |
| --------------------- | -------------------------------------------- | ----------- |
| `AUTH_PROVIDER`       | `aio`                                        | `local` (default) or `aio` |
| `AIO_ISSUER`          | `https://auth.example.com`                   | aio's public URL (the OIDC issuer) |
| `AIO_CLIENT_ID`       | `cashflow`                                   | The `--id` it was registered with |
| `AIO_CLIENT_SECRET`   | (from `oidc:client:create`)                  | Secret; keep it in a secret store, not in the repo |
| `AIO_REDIRECT_URL`    | `https://cashflow.example.com/auth/callback` | Must match `--redirect-uri` exactly |
| `AIO_POST_LOGOUT_URL` | `https://cashflow.example.com/`              | Optional; where aio returns after logout (must be registered) |

With `AUTH_PROVIDER=aio`, an incomplete configuration stops cashflow from starting (rather than
silently falling back to local login).

### When aio has problems

- **Fails closed**: if aio can't be reached, nobody new can log in. Users who are already logged in
  keep working, since every request uses cashflow's own session (30 days).
- Cashflow still starts when aio is down: aio's configuration (*discovery*) is fetched at the first
  login and retried on the next one.
- The CSP `form-action` header lists aio's origin, because logout is a POST that redirects to aio.

### Error messages

| Message in cashflow | Cause | What to do |
| --- | --- | --- |
| Layanan masuk sedang tidak tersedia (login service unavailable) | aio can't be reached | Check aio and `AIO_ISSUER` |
| Sesi masuk tidak valid atau sudah kedaluwarsa (login session invalid or expired) | The `cashflow_oidc` cookie is missing, older than 10 minutes, or a login was started again in another tab | Start the login again |
| Proses masuk dibatalkan atau ditolak (login cancelled or refused) | aio sent an error back to cashflow (e.g. the callback link was opened in a browser that didn't finish the login) | Log in again from cashflow in the same browser |
| Gagal menyelesaikan proses masuk (couldn't finish logging in) | The code exchange failed (e.g. wrong `AIO_CLIENT_SECRET`, or the code expired) | Check the configuration and cashflow's log |
| Gagal memverifikasi identitas (couldn't verify identity) | The ID token was rejected (issuer, audience, signature or nonce) | Make sure `AIO_ISSUER` and `AIO_CLIENT_ID` are right |
| Nama pengguna ini sudah dipakai akun cashflow lama (username taken by an existing account) | The aio username matches a local cashflow account that isn't linked yet | Move the existing accounts with `users:import` (with `--link-existing` if it's the same person) |
| (on aio's page) `redirect_uri` or *unknown client* error | `AIO_REDIRECT_URL` or `AIO_CLIENT_ID` doesn't match the registration in aio | Match `--id` and `--redirect-uri`; check `all-in-one oidc:client:list` |

Errors that happen on aio's pages (e.g. the shared demo account being refused, or a login link
opened in another browser) are shown by aio itself, in Indonesian, with a **Kembali** (back) button.

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
aioauth.go         Login through All-in-one (OpenID Connect): /auth/login, /auth/callback, logout
store.go           Data access layer (users, sessions, cashplans, entries)
slug.go            Slug normalization & validation
money.go           Rupiah parsing & formatting, Indonesian date formatting
schema.sql         DB schema (applied automatically on start)
templates/         html/template (layout, partials, pages; autherror.html = aio login errors)
static/            style.css, app.js
Dockerfile         Build the binary (multi-stage, small image)
docker-compose.yml Postgres + application
```

## Data model

```
users(id, username, password_hash /* NULL for All-in-one accounts */, aio_user_id, created_at)
sessions(id, user_id, created_at, expires_at, id_token /* for logging out of aio */)
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
