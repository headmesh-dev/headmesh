// Package recovery captures and checks the evidence needed before starting a
// restored Headmesh control plane.
package recovery

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Manifest holds public fingerprints and recovery positions from a healthy primary.
type Manifest struct {
	Version           int            `json:"version"`
	CapturedAt        time.Time      `json:"captured_at"`
	Headscale         HeadscaleState `json:"headscale"`
	PostgresSystemID  string         `json:"postgres_system_id"`
	PostgresDatabase  string         `json:"postgres_database"`
	PostgresLSN       string         `json:"postgres_lsn"`
	LastGrantBoundary *GrantBoundary `json:"last_grant_boundary"`
}

// HeadscaleState describes the live rows and most recent stored policy.
type HeadscaleState struct {
	NodeCount      int64  `json:"node_count"`
	UserCount      int64  `json:"user_count"`
	NoiseKeySHA256 string `json:"noise_public_key_sha256"`
	PolicySHA256   string `json:"policy_sha256"`
}

// GrantBoundary anchors the last audited grant lifecycle event. A nil boundary
// means the database had no audited grant events at capture time.
type GrantBoundary struct {
	AuditID int64  `json:"audit_id"`
	SHA256  string `json:"sha256"`
}

const sqliteState = `SELECT
	(SELECT count(*) FROM nodes WHERE deleted_at IS NULL) AS node_count,
	(SELECT count(*) FROM users WHERE deleted_at IS NULL) AS user_count,
	(SELECT data FROM policies WHERE deleted_at IS NULL ORDER BY id DESC LIMIT 1) AS policy`

const grantActions = `action IN ('grant.created', 'grant.revoked', 'grant.expired', 'grant.activated')`

// Capture reads a database-mode Headscale cell and the primary PostgreSQL
// cluster. It produces no manifest when either source is unavailable or empty.
func Capture(ctx context.Context, db *pgx.Conn, sqlite, noiseKey string) (Manifest, error) {
	state, err := headscaleState(ctx, sqlite, noiseKey)
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{Version: 1, Headscale: state}
	err = withEvidenceTx(ctx, db, func(tx pgx.Tx) error {
		var primary bool
		if err := tx.QueryRow(ctx, "SELECT NOT pg_is_in_recovery()").Scan(&primary); err != nil {
			return fmt.Errorf("check PostgreSQL role: %w", err)
		}
		if !primary {
			return errors.New("capture requires the primary PostgreSQL cluster")
		}
		event := new(GrantBoundary)
		err := tx.QueryRow(ctx, "SELECT id, encode(hash, 'hex') FROM audit_events WHERE "+grantActions+
			" ORDER BY id DESC LIMIT 1").Scan(&event.AuditID, &event.SHA256)
		switch {
		case err == nil:
			manifest.LastGrantBoundary = event
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("read the grant audit boundary: %w", err)
		}
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp(),
			(SELECT system_identifier::text FROM pg_control_system()),
			current_database(),
			pg_current_wal_flush_lsn()::text`).Scan(
			&manifest.CapturedAt, &manifest.PostgresSystemID, &manifest.PostgresDatabase, &manifest.PostgresLSN); err != nil {
			return fmt.Errorf("read primary PostgreSQL recovery position: %w", err)
		}
		return nil
	})
	if err != nil {
		return Manifest{}, err
	}
	manifest.CapturedAt = manifest.CapturedAt.UTC()
	return manifest, manifest.validate()
}

// ReadManifest checks the JSON version and rejects incomplete or malformed evidence.
func ReadManifest(r io.Reader) (Manifest, error) {
	// A missing user count is different from a valid cell with tagged nodes and zero users.
	manifest := Manifest{
		Headscale: HeadscaleState{UserCount: -1}, LastGrantBoundary: &GrantBoundary{AuditID: -1},
	}
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode backup manifest: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("backup manifest has trailing data")
	}
	return manifest, manifest.validate()
}

// Verify checks restored files and PostgreSQL before Headmesh enters recovery mode.
func Verify(ctx context.Context, db *pgx.Conn, sqlite, noiseKey, derpKey string, expected Manifest) error {
	if err := expected.validate(); err != nil {
		return err
	}
	if err := verifyHeadscale(ctx, sqlite, noiseKey, expected.Headscale); err != nil {
		return err
	}
	if _, err := machinePublicHash(derpKey); err != nil {
		return fmt.Errorf("restored DERP key: %w", err)
	}
	return withEvidenceTx(ctx, db, func(tx pgx.Tx) error {
		var id, database string
		var replayed bool
		if err := tx.QueryRow(ctx, `SELECT
			(SELECT system_identifier::text FROM pg_control_system()),
			current_database(),
			COALESCE(pg_last_wal_replay_lsn() >= $1::pg_lsn, false)`,
			expected.PostgresLSN).Scan(&id, &database, &replayed); err != nil {
			return fmt.Errorf("read restored PostgreSQL recovery position: %w", err)
		}
		if id != expected.PostgresSystemID {
			return errors.New("restored PostgreSQL cluster identity differs from the manifest")
		}
		if database != expected.PostgresDatabase {
			return errors.New("restored connection targets a different PostgreSQL database")
		}
		if !replayed {
			return errors.New("restored PostgreSQL has not replayed the manifest WAL position")
		}
		if boundary := expected.LastGrantBoundary; boundary != nil {
			var matches bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM audit_events WHERE id = $1 AND "+
				grantActions+" AND encode(hash, 'hex') = $2)", boundary.AuditID, boundary.SHA256).
				Scan(&matches); err != nil {
				return fmt.Errorf("read restored grant audit boundary: %w", err)
			}
			if !matches {
				return errors.New("restored grant audit boundary differs from the manifest")
			}
		}
		return nil
	})
}

func verifyHeadscale(ctx context.Context, sqlite, noiseKey string, expected HeadscaleState) error {
	out, err := readSQLite(ctx, sqlite, "PRAGMA integrity_check(1)")
	var checks []struct {
		Result string `json:"integrity_check"`
	}
	if err != nil || json.Unmarshal(out, &checks) != nil || len(checks) != 1 || checks[0].Result != "ok" {
		return errors.New("restored Headscale SQLite failed its integrity check")
	}
	state, err := headscaleState(ctx, sqlite, noiseKey)
	if err != nil {
		return err
	}
	return compareHeadscale(expected, state)
}

func withEvidenceTx(ctx context.Context, db *pgx.Conn, read func(pgx.Tx) error) error {
	err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		// The manifest is system evidence, not tenant-scoped. Set the same
		// transaction-local switch that the Headmesh store uses for workers.
		if _, err := tx.Exec(ctx, "SELECT set_config('headmesh.system', 'on', true)"); err != nil {
			return fmt.Errorf("set recovery evidence scope: %w", err)
		}
		return read(tx)
	})
	if err != nil {
		return fmt.Errorf("read recovery evidence transaction: %w", err)
	}
	return nil
}

func readSQLite(ctx context.Context, path, query string) ([]byte, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("invalid Headscale SQLite path")
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("read Headscale SQLite: expected an existing regular file")
	}
	// #nosec G204 -- The operator supplies the absolute path and the SQL is fixed.
	b, err := exec.CommandContext(ctx, "sqlite3", "-init", os.DevNull, "-readonly", "-batch", "-bail", "-json", absolute, query).Output()
	if err != nil {
		return nil, fmt.Errorf("read Headscale SQLite: %w", err)
	}
	return b, nil
}

func headscaleState(ctx context.Context, sqlite, path string) (HeadscaleState, error) {
	data, err := readSQLite(ctx, sqlite, sqliteState)
	if err != nil {
		return HeadscaleState{}, err
	}
	var rows []struct {
		NodeCount int64  `json:"node_count"`
		UserCount int64  `json:"user_count"`
		Policy    string `json:"policy"`
	}
	if err := json.Unmarshal(data, &rows); err != nil || len(rows) != 1 {
		return HeadscaleState{}, errors.New("could not read Headscale counts and latest policy")
	}
	row := rows[0]
	if row.NodeCount < 1 || strings.TrimSpace(row.Policy) == "" {
		return HeadscaleState{}, errors.New("read Headscale SQLite: no live nodes or stored policy")
	}
	noise, err := machinePublicHash(path)
	if err != nil {
		return HeadscaleState{}, fmt.Errorf("read Headscale Noise key: %w", err)
	}
	return HeadscaleState{
		NodeCount: row.NodeCount, UserCount: row.UserCount, NoiseKeySHA256: noise,
		PolicySHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(row.Policy))),
	}, nil
}

func machinePublicHash(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("private key must be an existing regular file")
	}
	f, err := os.Open(path) // #nosec G304 -- An operator-supplied local key path.
	if err != nil {
		return "", errors.New("cannot read private key file")
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil || len(raw) > 256 {
		return "", errors.New("cannot read private key file")
	}
	return machinePublicHashBytes(raw)
}

func machinePublicHashBytes(raw []byte) (string, error) {
	value, found := bytes.CutPrefix(bytes.TrimSpace(raw), []byte("privkey:"))
	key, err := hex.DecodeString(string(value))
	if !found || err != nil || len(key) != 32 || bytes.Equal(key, make([]byte, 32)) {
		return "", errors.New("invalid Headscale private key encoding")
	}
	private, err := ecdh.X25519().NewPrivateKey(key)
	if err != nil {
		return "", errors.New("invalid Headscale private key")
	}
	// Match the agent's fingerprint of the mkey: value returned by /key.
	public := "mkey:" + hex.EncodeToString(private.PublicKey().Bytes())
	return fmt.Sprintf("%x", sha256.Sum256([]byte(public))), nil
}

func compareHeadscale(expected, restored HeadscaleState) error {
	switch {
	case restored.NodeCount < expected.NodeCount || restored.UserCount < expected.UserCount:
		return errors.New("restored Headscale has fewer live nodes or users than the manifest")
	case restored.NoiseKeySHA256 != expected.NoiseKeySHA256:
		return errors.New("restored Headscale Noise identity differs from the manifest")
	case restored.PolicySHA256 != expected.PolicySHA256:
		return errors.New("restored Headscale policy differs from the manifest")
	}
	return nil
}

func (m Manifest) validate() error {
	if m.Version != 1 || m.CapturedAt.IsZero() {
		return errors.New("unsupported or incomplete backup manifest")
	}
	if m.Headscale.NodeCount < 1 || m.Headscale.UserCount < 0 ||
		!isHash(m.Headscale.NoiseKeySHA256) || !isHash(m.Headscale.PolicySHA256) {
		return errors.New("invalid Headscale evidence in backup manifest")
	}
	id, err := strconv.ParseUint(m.PostgresSystemID, 10, 64)
	if err != nil || id == 0 || m.PostgresDatabase == "" || !validLSN(m.PostgresLSN) {
		return errors.New("invalid PostgreSQL evidence in backup manifest")
	}
	if b := m.LastGrantBoundary; b != nil && (b.AuditID < 1 || !isHash(b.SHA256)) {
		return errors.New("invalid grant audit boundary in backup manifest")
	}
	return nil
}

func isHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func validLSN(value string) bool {
	high, low, found := strings.Cut(value, "/")
	hi, hiErr := strconv.ParseUint(high, 16, 32)
	lo, loErr := strconv.ParseUint(low, 16, 32)
	return found && hiErr == nil && loErr == nil && (hi != 0 || lo != 0)
}
