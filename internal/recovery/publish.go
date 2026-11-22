package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// PublishOptions names the primary inputs and the scoped R2 credentials file.
type PublishOptions struct {
	SQLite, NoiseKey, LitestreamConfig string
	Endpoint, Netrc                    string
}

// Publish replaces manifest.json only after the SQLite replica can be restored
// and pgBackRest can read the candidate WAL segment from its repository.
func Publish(ctx context.Context, db *pgx.Conn, opt PublishOptions) error {
	object, err := newRecoveryObject(opt.Endpoint, opt.Netrc, "/hm-pg/manifest.json")
	if err != nil {
		return err
	}
	scratch, err := os.MkdirTemp("", "headmesh-manifest-")
	if err != nil {
		return fmt.Errorf("create manifest work directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	candidate, err := Capture(ctx, db, opt.SQLite, opt.NoiseKey)
	if err != nil {
		return err
	}
	sqliteCopy := filepath.Join(scratch, "restored.sqlite")
	// #nosec G204 -- Paths are operator configuration and arguments are passed without a shell.
	if err := exec.CommandContext(ctx, "litestream", "restore", "-config", opt.LitestreamConfig, "-o", sqliteCopy, opt.SQLite).Run(); err != nil {
		return fmt.Errorf("restore the SQLite replica before publishing: %w", err)
	}
	if err := verifyHeadscale(ctx, sqliteCopy, opt.NoiseKey, candidate.Headscale); err != nil {
		return fmt.Errorf("SQLite replica has not caught up to the manifest: %w", err)
	}
	if err := verifyArchive(ctx, db, scratch, candidate); err != nil {
		return err
	}

	return putManifest(ctx, object, scratch, candidate)
}

var walFilePattern = regexp.MustCompile(`^[0-9A-F]{24}$`)

func verifyArchive(ctx context.Context, db *pgx.Conn, scratch string, candidate Manifest) error {
	var wal string
	if err := db.QueryRow(ctx, `SELECT pg_walfile_name($1::pg_lsn)`, candidate.PostgresLSN).Scan(&wal); err != nil {
		return fmt.Errorf("resolve the manifest WAL segment: %w", err)
	}
	if !walFilePattern.MatchString(wal) {
		return errors.New("PostgreSQL returned an invalid WAL segment name")
	}
	backrest := func(args ...string) *exec.Cmd {
		flags := []string{"--repo=1", "--stanza=headmesh", "--log-level-file=off", "--lock-path=" + scratch}
		// #nosec G204 -- Fixed program and stanza, scratch destination and validated WAL segment.
		return exec.CommandContext(ctx, "pgbackrest", append(flags, args...)...)
	}
	info, err := backrest("--output=json", "info").Output()
	if err != nil {
		return fmt.Errorf("inspect PostgreSQL backup repository: %w", err)
	}
	if err := matchingBaseBackup(info, candidate, wal); err != nil {
		return err
	}

	target := filepath.Join(scratch, "archived-wal")
	// pgBackRest returns 1 for a missing segment. On the primary, close the
	// current segment once so the normal archive_command can pick it up.
	fetch := func() error {
		return backrest("--no-archive-async", "archive-get", wal, target).Run()
	}
	err = fetch()
	if missingArchiveSegment(err) {
		// PostgreSQL skips a switch on an idle segment. A named WAL record
		// ensures the archiver has a segment to close even on a quiet primary.
		label := "headmesh-manifest-" + strconv.FormatInt(candidate.CapturedAt.UnixNano(), 10)
		if _, err := db.Exec(ctx, "SELECT pg_create_restore_point($1)", label); err != nil {
			return fmt.Errorf("write a WAL restore point for the manifest: %w", err)
		}
		if _, err := db.Exec(ctx, "SELECT pg_switch_wal()"); err != nil {
			return fmt.Errorf("request archival of the manifest WAL segment: %w", err)
		}
		deadline := time.NewTimer(30 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for missingArchiveSegment(err) {
			select {
			case <-ctx.Done():
				return fmt.Errorf("wait for the manifest WAL segment: %w", ctx.Err())
			case <-deadline.C:
				return errors.New("manifest WAL segment did not reach the backup repository")
			case <-tick.C:
				err = fetch()
			}
		}
	}
	if err != nil {
		return fmt.Errorf("fetch the manifest WAL segment from the repository: %w", err)
	}
	stat, err := os.Stat(target)
	if err != nil || stat.Size() == 0 {
		return errors.New("pgBackRest returned an empty WAL segment")
	}
	return nil
}

func missingArchiveSegment(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1
}

func matchingBaseBackup(info []byte, candidate Manifest, segment string) error {
	var stanzas []struct {
		DB []struct {
			ID       int    `json:"id"`
			SystemID uint64 `json:"system-id"`
		} `json:"db"`
		Backup []struct {
			Type     string `json:"type"`
			Error    bool   `json:"error"`
			Database struct {
				ID int `json:"id"`
			} `json:"database"`
			Archive struct {
				Stop string `json:"stop"`
			} `json:"archive"`
			LSN struct {
				Stop string `json:"stop"`
			} `json:"lsn"`
		} `json:"backup"`
	}
	if err := json.Unmarshal(info, &stanzas); err != nil || len(stanzas) != 1 || len(stanzas[0].DB) == 0 {
		return errors.New("could not identify the active database in pgBackRest info")
	}
	stanza := stanzas[0]
	active := stanza.DB[len(stanza.DB)-1]
	if strconv.FormatUint(active.SystemID, 10) != candidate.PostgresSystemID {
		return errors.New("pgBackRest is backing up a different PostgreSQL cluster")
	}
	for _, backup := range stanza.Backup {
		if backup.Type == "full" && !backup.Error && backup.Database.ID == active.ID &&
			walFilePattern.MatchString(backup.Archive.Stop) && backup.Archive.Stop[:8] == segment[:8] &&
			validLSN(backup.LSN.Stop) && lsnValue(backup.LSN.Stop) <= lsnValue(candidate.PostgresLSN) {
			return nil
		}
	}
	return errors.New("pgBackRest has no completed full backup on the manifest WAL timeline")
}

func lsnValue(lsn string) uint64 {
	high, low, _ := strings.Cut(lsn, "/")
	h, _ := strconv.ParseUint(high, 16, 32)
	l, _ := strconv.ParseUint(low, 16, 32)
	return h<<32 | l
}

func putManifest(ctx context.Context, object recoveryObject, scratch string, candidate Manifest) error {
	previous := filepath.Join(scratch, "previous.json")
	headers := filepath.Join(scratch, "headers")
	status, err := object.request(ctx, "--max-filesize", "16384", "--dump-header", headers, "--output", previous)
	if err != nil {
		return err
	}
	condition := "If-None-Match: *"
	switch status {
	case "200":
		data, err := os.ReadFile(previous) // #nosec G304 -- Curl writes only inside the private scratch directory.
		if err != nil {
			return fmt.Errorf("read current recovery manifest: %w", err)
		}
		existing, err := ReadManifest(bytes.NewReader(data))
		if err != nil {
			return err
		}
		if existing.PostgresSystemID != candidate.PostgresSystemID || existing.PostgresDatabase != candidate.PostgresDatabase {
			return errors.New("current manifest belongs to a different PostgreSQL database")
		}
		if !candidate.CapturedAt.After(existing.CapturedAt) || lsnValue(candidate.PostgresLSN) < lsnValue(existing.PostgresLSN) {
			return errors.New("candidate recovery evidence is older than the current manifest")
		}
		condition, err = objectMatchHeader(headers)
		if err != nil {
			return err
		}
	case "404":
	default:
		return fmt.Errorf("object store refused manifest read (HTTP %s)", status)
	}
	data, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return fmt.Errorf("encode recovery manifest: %w", err)
	}
	path := filepath.Join(scratch, "candidate.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write candidate manifest: %w", err)
	}
	status, err = object.request(ctx, "--upload-file", path, "--header", "Content-Type: application/json",
		"--header", condition, "--output", os.DevNull)
	if err != nil {
		return err
	}
	if status != "200" {
		return fmt.Errorf("object store refused conditional manifest write (HTTP %s)", status)
	}
	return nil
}
