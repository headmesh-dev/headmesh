-- Grant expiries are enforced by the policy timeline (no database write is needed for access to end), but each
-- expiry must still produce an audit entry exactly once. The sweeper claims expired grants and stamps this column.

-- +goose Up
ALTER TABLE grants ADD COLUMN expiry_audited_at timestamptz;
CREATE INDEX grants_unaudited_expiry_idx ON grants (expires_at)
    WHERE revoked_at IS NULL AND expiry_audited_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS grants_unaudited_expiry_idx;
ALTER TABLE grants DROP COLUMN expiry_audited_at;
