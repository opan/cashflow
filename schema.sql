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

-- Version history: each edit snapshots the entry's PREVIOUS party/description/
-- occurred_at here (append-only). The live values stay on entries.
CREATE TABLE IF NOT EXISTS entry_revisions (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    entry_id    uuid        NOT NULL REFERENCES entries(id),
    party       text        NOT NULL DEFAULT '',
    description text        NOT NULL DEFAULT '',
    occurred_at date        NOT NULL,
    edited_by   text        NOT NULL DEFAULT '',   -- username who made the superseding edit
    revised_at  timestamptz NOT NULL DEFAULT now() -- when this value was superseded
);
CREATE INDEX IF NOT EXISTS entry_revisions_entry_idx ON entry_revisions (entry_id, revised_at);
ALTER TABLE entry_revisions ADD COLUMN IF NOT EXISTS edited_by text NOT NULL DEFAULT '';

-- Shared "block all modification" guard (used to keep the revision log immutable).
CREATE OR REPLACE FUNCTION entries_no_modify() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'append-only: rows cannot be modified or deleted';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS entry_revisions_immutable ON entry_revisions;
CREATE TRIGGER entry_revisions_immutable
    BEFORE UPDATE OR DELETE ON entry_revisions
    FOR EACH ROW EXECUTE FUNCTION entries_no_modify();

-- Truthfulness guarantee: entries can never be deleted, and amount/type/
-- attachment/created_at are immutable. Only party/description/occurred_at may be
-- updated, and each such update snapshots the previous values into
-- entry_revisions so the full history is preserved and visible.
CREATE OR REPLACE FUNCTION entries_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'entries are append-only: they cannot be deleted';
    END IF;
    IF NEW.amount IS DISTINCT FROM OLD.amount
       OR NEW.type IS DISTINCT FROM OLD.type
       OR NEW.cashplan_id IS DISTINCT FROM OLD.cashplan_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR NEW.attachment_url IS DISTINCT FROM OLD.attachment_url
       OR NEW.attachment_name IS DISTINCT FROM OLD.attachment_name THEN
        RAISE EXCEPTION 'only party, description, and occurred_at may be edited';
    END IF;
    IF NEW.party IS DISTINCT FROM OLD.party
       OR NEW.description IS DISTINCT FROM OLD.description
       OR NEW.occurred_at IS DISTINCT FROM OLD.occurred_at THEN
        INSERT INTO entry_revisions (entry_id, party, description, occurred_at, edited_by)
        VALUES (OLD.id, OLD.party, OLD.description, OLD.occurred_at,
                COALESCE(current_setting('cashflow.editor', true), ''));
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS entries_immutable ON entries;
DROP TRIGGER IF EXISTS entries_guard ON entries;
CREATE TRIGGER entries_guard
    BEFORE UPDATE OR DELETE ON entries
    FOR EACH ROW EXECUTE FUNCTION entries_guard();
