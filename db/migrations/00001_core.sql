-- Headmesh core schema (PostgreSQL 18).
--
-- Conventions:
--   * UUIDv7 primary keys (uuidv7() is built into PostgreSQL 18), timestamptz everywhere.
--   * Every tenant-owned table carries org_id with an index that starts with it, and has row-level security
--     forced on. Request handlers set `headmesh.org_ids` (comma-separated UUIDs) per transaction; background
--     workers that need a whole-cell view set `headmesh.system = 'on'`. RLS is defence in depth: queries are
--     always scoped in application code as well.
--   * The audit log is append-only: UPDATE and DELETE raise, and each row carries a per-org SHA-256 hash chain
--     computed by the application (see internal/store/audit.go).

-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION headmesh_visible(org uuid) RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE AS $$
  SELECT coalesce(current_setting('headmesh.system', true), '') = 'on'
      OR org = ANY (string_to_array(nullif(current_setting('headmesh.org_ids', true), ''), ',')::uuid[])
$$;
-- +goose StatementEnd

CREATE TABLE cells (
    id                      uuid PRIMARY KEY DEFAULT uuidv7(),
    name                    text NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9-]{0,30}$'),
    server_url              text NOT NULL CHECK (server_url ~ '^https://'),
    base_domain             text NOT NULL,
    mode                    text NOT NULL DEFAULT 'observe' CHECK (mode IN ('observe', 'enforce')),
    headscale_version       text,
    agent_cert_fingerprint  text,
    max_nodes               integer NOT NULL DEFAULT 1000 CHECK (max_nodes > 0),
    max_orgs                integer NOT NULL DEFAULT 300 CHECK (max_orgs > 0),
    last_seen_at            timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE orgs (
    id              uuid PRIMARY KEY DEFAULT uuidv7(),
    short_id        text NOT NULL UNIQUE CHECK (short_id ~ '^[a-z2-7]{10}$'),
    slug            text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9](?:[a-z0-9-]{0,22}[a-z0-9])?$'),
    name            text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    cell_id         uuid NOT NULL REFERENCES cells (id),
    parent_org_id   uuid REFERENCES orgs (id),
    is_platform     boolean NOT NULL DEFAULT false,
    template        text NOT NULL DEFAULT 'team' CHECK (template IN ('team', 'locked')),
    headscale_users text[] NOT NULL DEFAULT '{}',
    settings        jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(settings) = 'object'),
    status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'deleted')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (parent_org_id IS DISTINCT FROM id),
    CHECK (NOT (is_platform AND parent_org_id IS NOT NULL))
);
CREATE INDEX orgs_parent_idx ON orgs (parent_org_id) WHERE parent_org_id IS NOT NULL;
CREATE INDEX orgs_cell_idx ON orgs (cell_id);

CREATE TABLE users (
    id                uuid PRIMARY KEY DEFAULT uuidv7(),
    email             text NOT NULL CHECK (email = lower(email) AND email LIKE '%_@_%'),
    name              text,
    email_verified_at timestamptz,
    disabled_at       timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_key ON users (email);

CREATE TABLE memberships (
    org_id      uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role        text NOT NULL CHECK (role IN ('owner', 'admin', 'member', 'viewer', 'billing')),
    expires_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);
CREATE INDEX memberships_user_idx ON memberships (user_id);

CREATE TABLE devices (
    id                 uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id             uuid NOT NULL REFERENCES orgs (id),
    cell_id            uuid NOT NULL REFERENCES cells (id),
    short_id           text NOT NULL UNIQUE CHECK (short_id ~ '^[a-z2-7]{10}$'),
    headscale_node_id  bigint,
    name               text NOT NULL CHECK (name ~ '^[a-z0-9](?:[a-z0-9-]{0,40}[a-z0-9])?$'),
    given_name         text NOT NULL,
    kind               text NOT NULL CHECK (kind IN ('server', 'gpu', 'laptop', 'sandbox', 'agent', 'ingress', 'other')),
    roles              text[] NOT NULL DEFAULT '{}',
    headscale_user     text,
    ipv4               inet,
    ipv6               inet,
    online             boolean NOT NULL DEFAULT false,
    last_seen_at       timestamptz,
    enrolled_via       uuid,
    state              text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'quarantined', 'removed')),
    external_ref       text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CHECK (headscale_user IS NULL OR cardinality(roles) = 0)
);
CREATE INDEX devices_org_idx ON devices (org_id, state);
CREATE UNIQUE INDEX devices_node_key ON devices (cell_id, headscale_node_id) WHERE headscale_node_id IS NOT NULL;
CREATE UNIQUE INDEX devices_given_name_key ON devices (cell_id, given_name) WHERE state <> 'removed';

CREATE TABLE services (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id      uuid NOT NULL REFERENCES orgs (id),
    device_id   uuid NOT NULL REFERENCES devices (id),
    name        text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,40}$'),
    ports       text[] NOT NULL CHECK (cardinality(ports) > 0),
    description text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);

-- Selectors are stored as {"kind": "all"|"users"|"role"|"device", "role": ..., "device": ...}.
CREATE TABLE rules (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id      uuid NOT NULL REFERENCES orgs (id),
    src         jsonb NOT NULL,
    dst         jsonb NOT NULL,
    ports       text[] NOT NULL CHECK (cardinality(ports) > 0),
    starts_at   timestamptz,
    expires_at  timestamptz,
    version     integer NOT NULL DEFAULT 1,
    created_by  uuid,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at IS NULL OR starts_at IS NULL OR expires_at > starts_at)
);
CREATE INDEX rules_org_idx ON rules (org_id);

CREATE TABLE platform_rules (
    id               uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id           uuid NOT NULL REFERENCES orgs (id), -- the platform org
    src              jsonb NOT NULL,
    dst              jsonb NOT NULL,
    ports            text[] NOT NULL CHECK (cardinality(ports) > 0),
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX platform_rules_org_idx ON platform_rules (org_id);

CREATE TABLE grants (
    id              uuid PRIMARY KEY DEFAULT uuidv7(),
    grantor_org_id  uuid NOT NULL REFERENCES orgs (id),
    grantee_org_id  uuid NOT NULL REFERENCES orgs (id),
    src             jsonb NOT NULL,
    dst_device_id   uuid NOT NULL REFERENCES devices (id),
    ports           text[] NOT NULL CHECK (cardinality(ports) > 0 AND NOT ('*' = ANY (ports))),
    starts_at       timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    reason          text,
    external_ref    text,
    created_by      uuid,
    created_at      timestamptz NOT NULL DEFAULT now(),
    revoked_at      timestamptz,
    revoked_by      uuid,
    CHECK (grantor_org_id <> grantee_org_id),
    CHECK (expires_at > starts_at)
);
CREATE INDEX grants_grantor_idx ON grants (grantor_org_id, expires_at);
CREATE INDEX grants_grantee_idx ON grants (grantee_org_id, expires_at);
CREATE INDEX grants_boundaries_idx ON grants (expires_at) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX grants_external_ref_key ON grants (grantor_org_id, external_ref) WHERE external_ref IS NOT NULL;

CREATE TABLE enrollment_tokens (
    id                uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id            uuid NOT NULL REFERENCES orgs (id),
    token_hash        bytea NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    prefix            text NOT NULL,
    kind              text NOT NULL CHECK (kind IN ('device', 'member')),
    roles             text[] NOT NULL DEFAULT '{}',
    device_kind       text NOT NULL DEFAULT 'server',
    max_uses          integer NOT NULL DEFAULT 1 CHECK (max_uses > 0),
    uses              integer NOT NULL DEFAULT 0 CHECK (uses >= 0),
    ephemeral         boolean NOT NULL DEFAULT false,
    approval_required boolean NOT NULL DEFAULT false,
    expires_at        timestamptz NOT NULL,
    created_by        uuid,
    created_at        timestamptz NOT NULL DEFAULT now(),
    revoked_at        timestamptz,
    CHECK (uses <= max_uses)
);
CREATE INDEX enrollment_tokens_org_idx ON enrollment_tokens (org_id, created_at DESC);

CREATE TABLE enrollments (
    id                 uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id             uuid NOT NULL REFERENCES orgs (id),
    token_id           uuid NOT NULL REFERENCES enrollment_tokens (id),
    cell_id            uuid NOT NULL REFERENCES cells (id),
    status             text NOT NULL CHECK (status IN ('pending_approval', 'minted', 'completed', 'expired', 'rejected')),
    preauth_key_id     bigint,
    hostname           text NOT NULL,
    os                 text,
    arch               text,
    headscale_node_id  bigint,
    device_id          uuid REFERENCES devices (id),
    client_ip          inet,
    created_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,
    completed_at       timestamptz
);
CREATE INDEX enrollments_org_idx ON enrollments (org_id, created_at DESC);
CREATE INDEX enrollments_pending_idx ON enrollments (expires_at) WHERE status IN ('pending_approval', 'minted');

CREATE TABLE api_keys (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id       uuid NOT NULL REFERENCES orgs (id),
    name         text NOT NULL,
    prefix       text NOT NULL,
    key_hash     bytea NOT NULL UNIQUE CHECK (length(key_hash) = 32),
    scopes       text[] NOT NULL CHECK (cardinality(scopes) > 0),
    created_by   uuid,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    last_used_at timestamptz,
    revoked_at   timestamptz
);
CREATE INDEX api_keys_org_idx ON api_keys (org_id);

-- Audit log. org_id is NULL for events that belong to no tenant (cell lifecycle, system). The chain is per
-- org (NULL org uses the all-zero UUID as its chain key).
CREATE TABLE audit_events (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id       uuid,
    occurred_at  timestamptz NOT NULL,
    actor_type   text NOT NULL CHECK (actor_type IN ('user', 'api_key', 'system', 'cell', 'platform')),
    actor_id     text NOT NULL,
    action       text NOT NULL CHECK (action ~ '^[a-z_]+(\.[a-z_]+)+$'),
    target_type  text NOT NULL,
    target_id    text NOT NULL,
    request_id   text,
    ip           inet,
    user_agent   text,
    data         jsonb NOT NULL DEFAULT '{}'::jsonb,
    prev_hash    bytea NOT NULL CHECK (length(prev_hash) = 32),
    hash         bytea NOT NULL UNIQUE CHECK (length(hash) = 32)
);
CREATE INDEX audit_events_org_idx ON audit_events (org_id, id);
CREATE INDEX audit_events_action_idx ON audit_events (action, occurred_at);

CREATE TABLE audit_chain_heads (
    chain_key   uuid PRIMARY KEY,
    head_hash   bytea NOT NULL CHECK (length(head_hash) = 32),
    last_id     bigint NOT NULL
);

-- +goose StatementBegin
CREATE FUNCTION audit_events_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'audit_events is append-only (% rejected)', TG_OP USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER audit_events_no_update BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_immutable();
CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_immutable();

CREATE TABLE webhook_endpoints (
    id                uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id            uuid NOT NULL REFERENCES orgs (id),
    url               text NOT NULL CHECK (url ~ '^https://'),
    secret_ciphertext bytea NOT NULL,
    events            text[] NOT NULL CHECK (cardinality(events) > 0),
    status            text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    failing_since     timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhook_endpoints_org_idx ON webhook_endpoints (org_id);

CREATE TABLE webhook_deliveries (
    id              uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id          uuid NOT NULL REFERENCES orgs (id),
    endpoint_id     uuid NOT NULL REFERENCES webhook_endpoints (id) ON DELETE CASCADE,
    audit_event_id  bigint NOT NULL REFERENCES audit_events (id),
    attempt         integer NOT NULL DEFAULT 0,
    status          text NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed', 'abandoned')),
    response_code   integer,
    error           text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    next_attempt_at timestamptz
);
CREATE INDEX webhook_deliveries_org_idx ON webhook_deliveries (org_id, created_at DESC);

CREATE TABLE policy_revisions (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    cell_id           uuid NOT NULL REFERENCES cells (id),
    revision          bigint NOT NULL,
    sha256            text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    body              text NOT NULL,
    compiler_version  text NOT NULL,
    effective_at      timestamptz NOT NULL,
    compiled_at       timestamptz NOT NULL DEFAULT now(),
    applied_at        timestamptz,
    status            text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'applied', 'failed', 'superseded')),
    error             text,
    UNIQUE (cell_id, revision)
);

CREATE TABLE cell_ops (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    cell_id     uuid NOT NULL REFERENCES cells (id),
    kind        text NOT NULL CHECK (kind IN ('create_pre_auth_key', 'set_tags', 'rename_node', 'delete_node', 'expire_node', 'set_approved_routes')),
    args        jsonb NOT NULL,
    status      text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'succeeded', 'failed', 'expired')),
    result      jsonb,
    error       text,
    not_after   timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    sent_at     timestamptz,
    acked_at    timestamptz
);
CREATE INDEX cell_ops_pending_idx ON cell_ops (cell_id, created_at) WHERE status IN ('pending', 'sent');

CREATE TABLE idempotency_keys (
    principal        text NOT NULL,
    key              text NOT NULL CHECK (length(key) BETWEEN 8 AND 255),
    request_hash     bytea NOT NULL,
    response_status  integer,
    response_body    jsonb,
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL DEFAULT now() + interval '24 hours',
    PRIMARY KEY (principal, key)
);
CREATE INDEX idempotency_keys_expiry_idx ON idempotency_keys (expires_at);

-- Row-level security on every tenant-owned table.
ALTER TABLE orgs ENABLE ROW LEVEL SECURITY;
ALTER TABLE orgs FORCE ROW LEVEL SECURITY;
CREATE POLICY orgs_tenant ON orgs USING (headmesh_visible(id)) WITH CHECK (headmesh_visible(id));

ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY memberships_tenant ON memberships USING (headmesh_visible(org_id)) WITH CHECK (headmesh_visible(org_id));

ALTER TABLE devices ENABLE ROW LEVEL SECURITY;
ALTER TABLE devices FORCE ROW LEVEL SECURITY;
CREATE POLICY devices_tenant ON devices USING (headmesh_visible(org_id)) WITH CHECK (headmesh_visible(org_id));

ALTER TABLE services ENABLE ROW LEVEL SECURITY;
ALTER TABLE services FORCE ROW LEVEL SECURITY;
CREATE POLICY services_tenant ON services USING (headmesh_visible(org_id)) WITH CHECK (headmesh_visible(org_id));

ALTER TABLE rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE rules FORCE ROW LEVEL SECURITY;
CREATE POLICY rules_tenant ON rules USING (headmesh_visible(org_id)) WITH CHECK (headmesh_visible(org_id));

ALTER TABLE platform_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform_rules FORCE ROW LEVEL SECURITY;
CREATE POLICY platform_rules_tenant ON platform_rules USING (headmesh_visible(org_id)) WITH CHECK (headmesh_visible(org_id));

-- A grant is visible to both the granting and the receiving org.
ALTER TABLE grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE grants FORCE ROW LEVEL SECURITY;
CREATE POLICY grants_tenant ON grants
    USING (headmesh_visible(grantor_org_id) OR headmesh_visible(grantee_org_id))
    WITH CHECK (headmesh_visible(grantor_org_id));

ALTER TABLE enrollment_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE enrollment_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY enrollment_tokens_tenant ON enrollment_tokens USING (headmesh_visible(org_id)) WITH CHECK (headmesh_visible(org_id));

ALTER TABLE enrollments ENABLE ROW LEVEL SECURITY;
ALTER TABLE enrollments FORCE ROW LEVEL SECURITY;
CREATE POLICY enrollments_tenant ON enrollments USING (headmesh_visible(org_id)) WITH CHECK (headmesh_visible(org_id));

ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE api_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY api_keys_tenant ON api_keys USING (headmesh_visible(org_id)) WITH CHECK (headmesh_visible(org_id));

ALTER TABLE audit_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_events_tenant ON audit_events
    USING (headmesh_visible(org_id) OR (org_id IS NULL AND coalesce(current_setting('headmesh.system', true), '') = 'on'))
    WITH CHECK (headmesh_visible(org_id) OR (org_id IS NULL AND coalesce(current_setting('headmesh.system', true), '') = 'on'));

ALTER TABLE webhook_endpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_endpoints FORCE ROW LEVEL SECURITY;
CREATE POLICY webhook_endpoints_tenant ON webhook_endpoints USING (headmesh_visible(org_id)) WITH CHECK (headmesh_visible(org_id));

ALTER TABLE webhook_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY webhook_deliveries_tenant ON webhook_deliveries USING (headmesh_visible(org_id)) WITH CHECK (headmesh_visible(org_id));

-- +goose Down
DROP TABLE IF EXISTS idempotency_keys, cell_ops, policy_revisions, webhook_deliveries, webhook_endpoints,
    audit_chain_heads, audit_events, api_keys, enrollments, enrollment_tokens, grants, platform_rules, rules,
    services, devices, memberships, users, orgs, cells;
DROP FUNCTION IF EXISTS audit_events_immutable();
DROP FUNCTION IF EXISTS headmesh_visible(uuid);
