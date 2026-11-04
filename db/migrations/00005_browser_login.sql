-- +goose Up
-- These global identity records grant no tenant or sandbox access. Memberships
-- and resource grants remain separate. Provider subjects are immutable IDs;
-- matching email addresses never merge accounts.
CREATE TABLE login_identities (
    provider text NOT NULL CHECK (provider IN ('github', 'huggingface')),
    subject text NOT NULL CHECK (length(subject) BETWEEN 1 AND 512),
    user_id uuid NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(provider, subject),
    UNIQUE(user_id, provider)
);
CREATE INDEX login_identities_user_idx ON login_identities(user_id);

CREATE TABLE login_flows (
    state_hash bytea PRIMARY KEY CHECK (length(state_hash) = 32),
    browser_hash bytea NOT NULL CHECK (length(browser_hash) = 32),
    provider text NOT NULL CHECK (provider IN ('github', 'huggingface')),
    verifier text NOT NULL CHECK (length(verifier) BETWEEN 43 AND 128),
    nonce text NOT NULL,
    return_path text NOT NULL,
    link_user_id uuid REFERENCES users(id),
    expires_at timestamptz NOT NULL
);
CREATE INDEX login_flows_expiry_idx ON login_flows(expires_at);

CREATE TABLE browser_sessions (
    token_hash bytea PRIMARY KEY CHECK (length(token_hash) = 32),
    user_id uuid NOT NULL REFERENCES users(id),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);
CREATE INDEX browser_sessions_user_idx ON browser_sessions(user_id);
CREATE INDEX browser_sessions_expiry_idx ON browser_sessions(expires_at);

-- +goose Down
DROP TABLE browser_sessions, login_flows, login_identities;
