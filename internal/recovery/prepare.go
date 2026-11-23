package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// PrepareOptions selects the off-site recovery store, its read-only credentials,
// and the new directory that will hold the pinned inputs.
type PrepareOptions struct{ Endpoint, Credentials, OutputDir string }

// PreparedInputReport contains the recovery point observed during preparation.
type PreparedInputReport struct {
	ManifestCapturedAt time.Time `json:"manifest_captured_at"`
	ManifestAgeSeconds int64     `json:"manifest_age_seconds"`
}

// PrepareInputs downloads a manifest once and retains the exact ciphertext it
// validated. The caller can carry that set to the standby without refetching.
func PrepareInputs(ctx context.Context, opt PrepareOptions) (PreparedInputReport, error) {
	var report PreparedInputReport
	if opt.Credentials == "" || opt.OutputDir == "" {
		return report, errors.New("provide read-only recovery credentials and a new prepared inputs directory")
	}
	parent := filepath.Dir(opt.OutputDir)
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return report, errors.New("the prepared inputs parent must be a private directory with mode 0700")
	}
	if _, err := os.Lstat(opt.OutputDir); !errors.Is(err, os.ErrNotExist) {
		return report, errors.New("the prepared inputs directory must not already exist")
	}
	work, err := os.MkdirTemp(parent, ".headmesh-prepare-")
	if err != nil {
		return report, fmt.Errorf("create private input preparation workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	ready := filepath.Join(work, "inputs")
	if err := os.Mkdir(ready, 0o700); err != nil {
		return report, fmt.Errorf("create private prepared inputs directory: %w", err)
	}
	manifestFile := filepath.Join(ready, "manifest.json")
	if err := downloadPreparedInput(ctx, opt.Endpoint, filepath.Join(opt.Credentials, "manifest.netrc"),
		manifestObjectPath, manifestFile, MaxManifestBytes); err != nil {
		return report, err
	}
	fetched := time.Now().Truncate(time.Second)
	raw, err := readBundleFile(manifestFile, MaxManifestBytes)
	if err != nil {
		return report, fmt.Errorf("read pinned recovery manifest: %w", err)
	}
	manifest, err := ReadManifest(bytes.NewReader(raw))
	if err != nil {
		return report, err
	}
	if manifest.PostgresDatabase != "headmesh_prod" || manifest.CapturedAt.Truncate(time.Second).After(fetched) {
		return report, errors.New("the recovery manifest must identify headmesh_prod and have a current or past timestamp")
	}
	if err := verifyPreparedArtifacts(ctx, opt, manifest, ready, work); err != nil {
		return report, err
	}
	if err := os.Rename(ready, opt.OutputDir); err != nil {
		return report, fmt.Errorf("release the verified recovery inputs: %w", err)
	}
	report.ManifestCapturedAt = manifest.CapturedAt
	report.ManifestAgeSeconds = int64(fetched.Sub(manifest.CapturedAt.Truncate(time.Second)) / time.Second)
	return report, nil
}

func verifyPreparedArtifacts(ctx context.Context, opt PrepareOptions, manifest Manifest, ready, work string) error {
	netrc := filepath.Join(opt.Credentials, "keys.netrc")
	identity := filepath.Join(opt.Credentials, "recovery.agekey")
	fingerprint := manifest.Headscale.NoiseKeySHA256
	keys := filepath.Join(ready, "headscale.age")
	runtime := filepath.Join(ready, "runtime.age")
	waitlist := filepath.Join(ready, "waitlist.db.age")
	if err := downloadPreparedInput(ctx, opt.Endpoint, netrc, keyObjectPath(fingerprint), keys, maxCiphertext); err != nil {
		return err
	}
	if err := StageKeyBundle(ctx, manifest, KeyBundleStage{
		BundledFile: keys, IdentityFile: identity, Format: "age", OutputDir: filepath.Join(work, "keys"),
	}); err != nil {
		return fmt.Errorf("verify pinned Headscale bundle: %w", err)
	}
	if err := downloadPreparedInput(ctx, opt.Endpoint, netrc, runtimeObjectPath(fingerprint), runtime, maxCiphertext); err != nil {
		return err
	}
	if err := StageRuntime(ctx, manifest, RuntimeStage{
		Bundle: runtime, Identity: identity, OutputDir: filepath.Join(work, "runtime"),
	}); err != nil {
		return fmt.Errorf("verify pinned runtime snapshot: %w", err)
	}
	if err := downloadPreparedInput(ctx, opt.Endpoint, netrc, waitlistObjectPath, waitlist, 0); err != nil {
		return err
	}
	if err := StageWaitlistBundle(ctx, filepath.Join(work, "waitlist.db"), identity, waitlist); err != nil {
		return fmt.Errorf("verify pinned waitlist snapshot: %w", err)
	}
	return nil
}

func downloadPreparedInput(ctx context.Context, endpoint, netrc, key, path string, limit int) error {
	object, err := newRecoveryObject(endpoint, netrc, key)
	if err != nil {
		return err
	}
	args := []string{"--output", path}
	if limit > 0 {
		args = append(args, "--max-filesize", strconv.Itoa(limit))
	}
	status, err := object.request(ctx, args...)
	if err != nil {
		return fmt.Errorf("download recovery input %s: %w", key, err)
	}
	if status != "200" {
		return fmt.Errorf("store refused recovery input %s (HTTP %s)", key, status)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("restrict prepared recovery input permissions: %w", err)
	}
	return nil
}
