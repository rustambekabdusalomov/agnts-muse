-- agnts-muse budget ledger, schema 1.
--
-- The no-overspend rule is a constraint, not just code: an account's
-- reserved money can never exceed what it has, whatever writes the rows.

CREATE TABLE IF NOT EXISTS muse_accounts (
    id         text PRIMARY KEY,
    total      bigint      NOT NULL DEFAULT 0, -- micro-dollars: deposited minus charged
    held       bigint      NOT NULL DEFAULT 0, -- reserved by open holds
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT muse_accounts_held_nonnegative CHECK (held >= 0),
    CONSTRAINT muse_accounts_no_overspend CHECK (total >= held)
);

CREATE TABLE IF NOT EXISTS muse_holds (
    id         bigserial PRIMARY KEY,
    key        text        NOT NULL UNIQUE,
    state      text        NOT NULL CHECK (state IN ('open', 'committed', 'released', 'expired')),
    amount     bigint      NOT NULL CHECK (amount > 0),
    ttl_ms     bigint      NOT NULL,
    expires_at timestamptz NOT NULL,
    charged    bigint,
    released   bigint,
    unbilled   bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    closed_at  timestamptz
);

-- Reap scans only open holds.
CREATE INDEX IF NOT EXISTS muse_holds_open_expiry ON muse_holds (expires_at) WHERE state = 'open';

CREATE TABLE IF NOT EXISTS muse_hold_legs (
    hold_id    bigint  NOT NULL REFERENCES muse_holds (id) ON DELETE CASCADE,
    position   int     NOT NULL,
    account_id text    NOT NULL REFERENCES muse_accounts (id),
    is_cap     boolean NOT NULL,
    held       bigint  NOT NULL CHECK (held >= 0),
    charged    bigint  NOT NULL DEFAULT 0 CHECK (charged >= 0),
    PRIMARY KEY (hold_id, position)
);

-- The journal: every deposit and charge, for audit and reconciliation.
CREATE TABLE IF NOT EXISTS muse_entries (
    id            bigserial PRIMARY KEY,
    account_id    text        NOT NULL REFERENCES muse_accounts (id),
    kind          text        NOT NULL CHECK (kind IN ('deposit', 'charge')),
    amount        bigint      NOT NULL,
    balance_after bigint      NOT NULL,
    key           text,
    hold_id       bigint REFERENCES muse_holds (id),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS muse_entries_deposit_key ON muse_entries (key) WHERE kind = 'deposit';
CREATE INDEX IF NOT EXISTS muse_entries_account ON muse_entries (account_id, id);
