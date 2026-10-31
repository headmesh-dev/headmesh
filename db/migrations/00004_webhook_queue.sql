-- Webhook deliveries are their own queue (ADR 0016). Every audit event fans out to the org's matching active
-- endpoints in the transaction that writes it, so an event is delivered if and only if it happened, and workers claim
-- due deliveries with FOR UPDATE SKIP LOCKED under a short lease.

-- +goose Up
ALTER TABLE webhook_endpoints ADD COLUMN description text CHECK (length(description) <= 500);
ALTER TABLE webhook_deliveries ADD COLUMN last_attempt_at timestamptz;
ALTER TABLE webhook_deliveries ADD COLUMN delivered_at timestamptz;
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX webhook_deliveries_endpoint_idx ON webhook_deliveries (endpoint_id, created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS webhook_deliveries_endpoint_idx;
DROP INDEX IF EXISTS webhook_deliveries_due_idx;
ALTER TABLE webhook_deliveries DROP COLUMN delivered_at;
ALTER TABLE webhook_deliveries DROP COLUMN last_attempt_at;
ALTER TABLE webhook_endpoints DROP COLUMN description;
