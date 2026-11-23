package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	waitlistObjectPath  = "/hm-keys/waitlist/prod.db.age"
	waitlistSchemaCheck = `PRAGMA integrity_check;
SELECT count(*) FROM schema_migrations WHERE name = '0001_create_waitlist.sql';
SELECT id, email, consent_version, confirmed_at, unsubscribed_at FROM waitlist LIMIT 0;`
)

// PublishWaitlist takes a consistent online SQLite snapshot, encrypts it to
// both recovery recipients and conditionally replaces the private object.
func PublishWaitlist(ctx context.Context, db string, recipients []string, endpoint, netrc string) error {
	if !validAgeRecipients(recipients) {
		return errors.New("provide two distinct native age recipients")
	}
	info, err := os.Stat(db)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("a nonempty waitlist database is required")
	}
	db, err = filepath.Abs(db)
	if err != nil {
		return fmt.Errorf("resolve waitlist database path: %w", err)
	}
	object, err := newRecoveryObject(endpoint, netrc, waitlistObjectPath)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "headmesh-waitlist-upload-")
	if err != nil {
		return fmt.Errorf("create private waitlist workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	// Read the ETag before capturing the snapshot so a concurrent publisher
	// cannot be silently overwritten by an earlier capture.
	condition, err := object.writeCondition(ctx, filepath.Join(work, "headers"))
	if err != nil {
		return fmt.Errorf("check waitlist object before upload: %w", err)
	}
	snapshot := filepath.Join(work, "waitlist.db")
	// #nosec G204 -- The input is the local database; the destination is inside a private directory.
	if err := exec.CommandContext(ctx, "sqlite3", "-batch", "-bail", "-readonly", "-cmd", ".timeout 5000", db,
		".backup "+strconv.Quote(snapshot)).Run(); err != nil {
		return fmt.Errorf("take waitlist SQLite snapshot: %w", err)
	}
	if err := verifyWaitlistSQLite(ctx, snapshot); err != nil {
		return err
	}
	ciphertext := filepath.Join(work, "waitlist.db.age")
	// #nosec G204 -- The recipients are validated public keys and files are in the private workspace.
	if err := exec.CommandContext(ctx, "age", "-e", "-r", recipients[0], "-r", recipients[1], "-o", ciphertext, snapshot).Run(); err != nil {
		return fmt.Errorf("encrypt waitlist snapshot: %w", err)
	}
	status, err := object.request(ctx, "--upload-file", ciphertext, "--header", condition,
		"--header", "Content-Type: application/octet-stream", "--output", os.DevNull)
	if err != nil {
		return err
	}
	if status != "200" {
		return fmt.Errorf("object store refused conditional waitlist write (HTTP %s)", status)
	}
	return nil
}

// StageWaitlist decrypts the latest off-host snapshot, validates it and
// publishes a mode-0600 database file at a previously absent local path.
func StageWaitlist(ctx context.Context, output, identity, endpoint, netrc string) error {
	object, err := newRecoveryObject(endpoint, netrc, waitlistObjectPath)
	if err != nil {
		return err
	}
	return stageWaitlist(ctx, output, identity, func(work string) (string, error) {
		ciphertext := filepath.Join(work, "waitlist.db.age")
		status, err := object.request(ctx, "--output", ciphertext)
		if err != nil {
			return "", err
		}
		if status != "200" {
			return "", fmt.Errorf("object store refused waitlist download (HTTP %s)", status)
		}
		return ciphertext, nil
	})
}

// StageWaitlistBundle stages a previously downloaded encrypted snapshot.
func StageWaitlistBundle(ctx context.Context, output, identity, ciphertext string) error {
	return stageWaitlist(ctx, output, identity, func(string) (string, error) {
		info, err := os.Stat(ciphertext)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return "", errors.New("provide a nonempty regular encrypted waitlist file")
		}
		return ciphertext, nil
	})
}

func stageWaitlist(ctx context.Context, output, identity string, load func(string) (string, error)) error {
	info, err := os.Stat(identity)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("waitlist staging requires a private age identity file")
	}
	if output == "" {
		return errors.New("provide a new waitlist staging file")
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		return errors.New("the waitlist staging file must not already exist")
	}
	work, err := os.MkdirTemp(filepath.Dir(output), ".headmesh-waitlist-")
	if err != nil {
		return fmt.Errorf("create private waitlist staging workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	ciphertext, err := load(work)
	if err != nil {
		return err
	}
	snapshot := filepath.Join(work, "waitlist.db")
	// #nosec G204 -- The operator selects the local input and identity. The output is in the private workspace.
	if err := exec.CommandContext(ctx, "age", "-d", "-i", identity, "-o", snapshot, "--", ciphertext).Run(); err != nil {
		return fmt.Errorf("decrypt waitlist snapshot: %w", err)
	}
	if err := verifyWaitlistSQLite(ctx, snapshot); err != nil {
		return err
	}
	if err := os.Chmod(snapshot, 0o600); err != nil {
		return fmt.Errorf("restrict staged waitlist permissions: %w", err)
	}
	if err := os.Link(snapshot, output); err != nil {
		return fmt.Errorf("install staged waitlist snapshot: %w", err)
	}
	return nil
}

func verifyWaitlistSQLite(ctx context.Context, snapshot string) error {
	// #nosec G204 -- This is a regular file in the private workspace and a fixed read-only query.
	out, err := exec.CommandContext(ctx, "sqlite3", "-batch", "-bail", "-readonly", snapshot, waitlistSchemaCheck).Output()
	if err != nil || strings.TrimSpace(string(out)) != "ok\n1" {
		return errors.New("the waitlist snapshot failed SQLite integrity or schema validation")
	}
	return nil
}
