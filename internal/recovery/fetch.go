package recovery

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const manifestObjectPath = "/hm-pg/manifest.json"

// MaxManifestBytes limits local reads and downloads of a recovery manifest.
const MaxManifestBytes = 16 << 10

// Fetch reads the published evidence over TLS using the scoped PostgreSQL bucket credential.
func Fetch(ctx context.Context, endpoint, netrc string) (Manifest, error) {
	object, err := newRecoveryObject(endpoint, netrc, manifestObjectPath)
	if err != nil {
		return Manifest{}, err
	}
	scratch, err := os.MkdirTemp("", "headmesh-manifest-fetch-")
	if err != nil {
		return Manifest{}, fmt.Errorf("create manifest download workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	path := filepath.Join(scratch, "manifest.json")
	status, err := object.request(ctx, "--max-filesize", strconv.Itoa(MaxManifestBytes), "--output", path)
	if err != nil {
		return Manifest{}, err
	}
	if status != "200" {
		return Manifest{}, fmt.Errorf("object store refused manifest download (HTTP %s)", status)
	}
	raw, err := readBundleFile(path, MaxManifestBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("read downloaded manifest: %w", err)
	}
	return ReadManifest(bytes.NewReader(raw))
}
