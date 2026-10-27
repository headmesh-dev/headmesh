-- Enrollment completion: the enrolling client receives a one-time secret with its pre-auth key and must present
-- it to complete enrollment, so nobody else can bind a node to the pending device.

-- +goose Up
ALTER TABLE enrollments
    ADD COLUMN completion_hash bytea CHECK (completion_hash IS NULL OR length(completion_hash) = 32);

-- +goose Down
ALTER TABLE enrollments DROP COLUMN completion_hash;
