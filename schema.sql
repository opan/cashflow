-- Cashflow schema. Applied automatically at startup (idempotent).

CREATE TABLE IF NOT EXISTS users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    username      text        NOT NULL UNIQUE,   -- unique, first-come-first-served
    password_hash text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
    id         text        PRIMARY KEY,          -- random session token
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user_idx ON sessions (user_id);

CREATE TABLE IF NOT EXISTS cashplans (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id    uuid        NOT NULL REFERENCES users(id),
    slug        text        NOT NULL UNIQUE,      -- user-defined, unique, first-come-first-served
    title       text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS cashplans_owner_idx ON cashplans (owner_id);

CREATE TABLE IF NOT EXISTS entries (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    cashplan_id uuid        NOT NULL REFERENCES cashplans(id),
    type        text        NOT NULL CHECK (type IN ('income', 'expense')),
    party       text        NOT NULL DEFAULT '',  -- income: payer (pembayar); expense: payee (penerima)
    description text        NOT NULL DEFAULT '',  -- income: notes (catatan); expense: reason (keterangan)
    amount      bigint      NOT NULL CHECK (amount > 0),  -- whole rupiah (IDR has no sub-unit)
    occurred_at date        NOT NULL DEFAULT CURRENT_DATE,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS entries_plan_idx
    ON entries (cashplan_id, occurred_at DESC, created_at DESC);

-- Optional receipt attachment (link only; the file lives in Nextcloud).
-- Set at INSERT time and, like the rest of the row, immutable thereafter.
ALTER TABLE entries ADD COLUMN IF NOT EXISTS attachment_url  text NOT NULL DEFAULT '';
ALTER TABLE entries ADD COLUMN IF NOT EXISTS attachment_name text NOT NULL DEFAULT '';

-- Truthfulness guarantee: entries are append-only. Block UPDATE/DELETE at the
-- database level so the ledger is immutable even outside the application.
CREATE OR REPLACE FUNCTION entries_no_modify() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'entries are append-only: they cannot be modified or deleted';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS entries_immutable ON entries;
CREATE TRIGGER entries_immutable
    BEFORE UPDATE OR DELETE ON entries
    FOR EACH ROW EXECUTE FUNCTION entries_no_modify();
