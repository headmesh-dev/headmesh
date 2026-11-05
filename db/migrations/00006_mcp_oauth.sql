-- +goose Up
CREATE TABLE oauth_resources (
    resource text PRIMARY KEY CHECK(resource ~ '^https://'),
    name text NOT NULL
);
CREATE TABLE oauth_resource_grants (
    user_id uuid NOT NULL REFERENCES users(id),
    resource text NOT NULL REFERENCES oauth_resources(resource),
    scopes text[] NOT NULL CHECK(cardinality(scopes)>0 AND scopes <@ ARRAY['sandbox:read','sandbox:exec','sandbox:write']::text[]),
    revoked_at timestamptz,
    PRIMARY KEY(user_id,resource)
);
ALTER TABLE oauth_resource_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE oauth_resource_grants FORCE ROW LEVEL SECURITY;
CREATE POLICY oauth_grants_user ON oauth_resource_grants
    USING (user_id::text=current_setting('headmesh.user_id',true) OR current_setting('headmesh.system',true)='on')
    WITH CHECK (user_id::text=current_setting('headmesh.user_id',true) OR current_setting('headmesh.system',true)='on');

CREATE TABLE oauth_clients (
    id text PRIMARY KEY,
    metadata jsonb NOT NULL,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE oauth_requests (
    id text PRIMARY KEY,
    data jsonb NOT NULL,
    code_hash bytea UNIQUE,
    csrf_hash bytea,
    browser_hash bytea,
    expires_at timestamptz NOT NULL
);
CREATE INDEX oauth_requests_expiry_idx ON oauth_requests(expires_at);
CREATE TABLE oauth_families (
    id text PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id),
    client_id text NOT NULL REFERENCES oauth_clients(id),
    resource text NOT NULL REFERENCES oauth_resources(resource),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);
CREATE INDEX oauth_families_user_idx ON oauth_families(user_id,client_id);
CREATE TABLE oauth_refresh_tokens (
    token_hash bytea PRIMARY KEY CHECK(length(token_hash)=32),
    family_id text NOT NULL REFERENCES oauth_families(id),
    data jsonb NOT NULL,
    used_at timestamptz
);
CREATE TABLE oauth_access_tokens (
    id text PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id),
    client_id text NOT NULL REFERENCES oauth_clients(id),
    resource text NOT NULL REFERENCES oauth_resources(resource),
    family_id text REFERENCES oauth_families(id),
    scopes text[] NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);
CREATE INDEX oauth_access_tokens_family_idx ON oauth_access_tokens(family_id);
CREATE INDEX oauth_access_tokens_user_idx ON oauth_access_tokens(user_id,client_id);

-- Restoring a grant must never reactivate its previously revoked credentials.
-- +goose StatementBegin
CREATE FUNCTION invalidate_oauth_grant_tokens() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF OLD.scopes IS NOT DISTINCT FROM NEW.scopes AND OLD.revoked_at IS NOT DISTINCT FROM NEW.revoked_at THEN
            RETURN NULL;
        END IF;
    END IF;
    UPDATE oauth_families SET revoked_at=coalesce(revoked_at,now())
        WHERE user_id=OLD.user_id AND resource=OLD.resource;
    UPDATE oauth_access_tokens SET revoked_at=coalesce(revoked_at,now())
        WHERE user_id=OLD.user_id AND resource=OLD.resource;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER oauth_grant_token_revocation AFTER UPDATE OR DELETE ON oauth_resource_grants
    FOR EACH ROW EXECUTE FUNCTION invalidate_oauth_grant_tokens();

CREATE TABLE oauth_signing_keys (
    id text PRIMARY KEY,
    jwk jsonb NOT NULL,
    expires_at timestamptz NOT NULL
);

-- +goose Down
DROP TRIGGER oauth_grant_token_revocation ON oauth_resource_grants;
DROP FUNCTION invalidate_oauth_grant_tokens();
DROP TABLE oauth_signing_keys, oauth_access_tokens, oauth_refresh_tokens, oauth_families, oauth_requests, oauth_clients, oauth_resource_grants, oauth_resources;
