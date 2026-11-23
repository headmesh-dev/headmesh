package recovery

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const runtimeFileLimit = 64 << 10

var runtimeRequired = []string{
	"headmesh/prod/backend.env", "headmesh/prod/db.env", "headmesh/prod/db-owner.env",
	"headmesh/prod/secrets.env", "headmesh/prod/waitlist.env",
	"headmesh/prod/backend/op-signing.key", "headmesh/prod/backend/cell-ca/ca.crt",
	"headmesh/tls/fullchain.pem", "headmesh/tls/privkey.pem",
	"headmesh-cellagent/cellagent.json", "headmesh-cellagent/client.crt", "headmesh-cellagent/client.key",
}

var runtimeOAuthPath = regexp.MustCompile(`^/etc/headmesh/oauth/[A-Za-z0-9][A-Za-z0-9._-]*$`)

type runtimeBundle struct {
	Version int               `json:"version"`
	Noise   string            `json:"noise_public_key_sha256"`
	Files   map[string][]byte `json:"files"`
}

// RuntimeSources identifies the primary's deployed configuration and Noise key.
type RuntimeSources struct{ EtcDir, NoiseKey string }

// RuntimeStage selects the encrypted runtime snapshot and a new private directory.
type RuntimeStage struct {
	Bundle, Identity, OutputDir string
	Endpoint, Netrc             string
}

// PackRuntime writes the primary's runtime snapshot encrypted to both recipients.
func PackRuntime(ctx context.Context, src RuntimeSources, recipients []string, out io.Writer) error {
	noise, err := machinePublicHash(src.NoiseKey)
	if err != nil {
		return fmt.Errorf("read runtime source Noise key: %w", err)
	}
	ciphertext, err := encryptRuntime(ctx, src, noise, recipients)
	if err != nil {
		return err
	}
	if _, err := out.Write(ciphertext); err != nil {
		return fmt.Errorf("write encrypted runtime snapshot: %w", err)
	}
	return nil
}

// PublishRuntime conditionally writes the runtime snapshot for this cell identity.
func PublishRuntime(ctx context.Context, src RuntimeSources, recipients []string, endpoint, netrc string) error {
	noise, err := machinePublicHash(src.NoiseKey)
	if err != nil {
		return fmt.Errorf("read runtime source Noise key: %w", err)
	}
	object, err := newRecoveryObject(endpoint, netrc, runtimeObjectPath(noise))
	if err != nil {
		return err
	}
	scratch, err := os.MkdirTemp("", "headmesh-runtime-")
	if err != nil {
		return fmt.Errorf("create runtime publication workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	condition, err := object.writeCondition(ctx, filepath.Join(scratch, "headers"))
	if err != nil {
		return fmt.Errorf("read runtime destination before upload: %w", err)
	}
	ciphertext, err := encryptRuntime(ctx, src, noise, recipients)
	if err != nil {
		return err
	}
	path := filepath.Join(scratch, "runtime.age")
	if err := os.WriteFile(path, ciphertext, 0o600); err != nil {
		return fmt.Errorf("write encrypted runtime upload: %w", err)
	}
	status, err := object.request(ctx, "--upload-file", path, "--header", condition,
		"--header", "Content-Type: application/octet-stream", "--output", os.DevNull)
	if err != nil {
		return err
	}
	if status != "200" {
		return fmt.Errorf("object store refused conditional runtime write (HTTP %s)", status)
	}
	return nil
}

func runtimeObjectPath(noise string) string { return "/hm-keys/runtime/" + noise + ".age" }

func encryptRuntime(ctx context.Context, src RuntimeSources, noise string, recipients []string) ([]byte, error) {
	if !validAgeRecipients(recipients) {
		return nil, errors.New("provide two distinct native age recipients")
	}
	files := make(map[string][]byte)
	read := func(names []string) error {
		for _, name := range names {
			data, err := readBundleFile(filepath.Join(src.EtcDir, name), runtimeFileLimit)
			if err != nil {
				return fmt.Errorf("read runtime file %s: %w", name, err)
			}
			files[name] = data
		}
		return nil
	}
	if err := read(runtimeRequired); err != nil {
		return nil, err
	}
	optional, err := runtimeReferencedFiles(files)
	if err != nil {
		return nil, err
	}
	if err := read(optional); err != nil {
		return nil, err
	}
	bundle := runtimeBundle{Version: 1, Noise: noise, Files: files}
	if err := bundle.validate(noise); err != nil {
		return nil, err
	}
	// Reject capture if the cell identity changed while reading the runtime files.
	if current, err := machinePublicHash(src.NoiseKey); err != nil || current != noise {
		return nil, errors.New("source Noise identity changed during runtime capture")
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("encode runtime snapshot: %w", err)
	}
	// #nosec G204 -- Both public recipients have been validated.
	cmd := exec.CommandContext(ctx, "age", "-e", "-r", recipients[0], "-r", recipients[1])
	cmd.Stdin = bytes.NewReader(data)
	ciphertext, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("encrypt runtime snapshot: %w", err)
	}
	return ciphertext, nil
}

// StageRuntime verifies the snapshot before exposing any files in a private stage.
func StageRuntime(ctx context.Context, manifest Manifest, opt RuntimeStage) error {
	if err := manifest.validate(); err != nil {
		return err
	}
	if (opt.Bundle == "") == (opt.Netrc == "") || opt.OutputDir == "" {
		return errors.New("select a local encrypted snapshot or R2, and a new output directory")
	}
	info, err := os.Stat(opt.Identity)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("runtime staging requires a private age identity file")
	}
	path := opt.Bundle
	if opt.Netrc != "" {
		object, err := newRecoveryObject(opt.Endpoint, opt.Netrc, runtimeObjectPath(manifest.Headscale.NoiseKeySHA256))
		if err != nil {
			return err
		}
		scratch, err := os.MkdirTemp("", "headmesh-runtime-download-")
		if err != nil {
			return fmt.Errorf("create runtime download workspace: %w", err)
		}
		defer func() { _ = os.RemoveAll(scratch) }()
		path = filepath.Join(scratch, "runtime.age")
		status, err := object.request(ctx, "--max-filesize", strconv.Itoa(maxCiphertext), "--output", path)
		if err != nil {
			return err
		}
		if status != "200" {
			return fmt.Errorf("object store refused runtime download (HTTP %s)", status)
		}
	}
	ciphertext, err := readBundleFile(path, maxCiphertext)
	if err != nil {
		return fmt.Errorf("read encrypted runtime snapshot: %w", err)
	}
	data, err := decryptKeyBundle(ctx, ciphertext, "age", opt.Identity)
	if err != nil {
		return fmt.Errorf("unlock runtime snapshot: %w", err)
	}
	var bundle runtimeBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return errors.New("invalid runtime snapshot format")
	}
	if err := bundle.validate(manifest.Headscale.NoiseKeySHA256); err != nil {
		return err
	}
	if err := os.Mkdir(opt.OutputDir, 0o700); err != nil {
		return fmt.Errorf("create private runtime stage: %w", err)
	}
	for name, data := range bundle.Files {
		path := filepath.Join(opt.OutputDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			_ = os.RemoveAll(opt.OutputDir)
			return fmt.Errorf("create runtime staging directory: %w", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			_ = os.RemoveAll(opt.OutputDir)
			return fmt.Errorf("stage runtime file %s: %w", name, err)
		}
	}
	return nil
}

func runtimeReferencedFiles(files map[string][]byte) ([]string, error) {
	var agent struct {
		ServerCA string `json:"server_ca"`
	}
	if err := json.Unmarshal(files["headmesh-cellagent/cellagent.json"], &agent); err != nil {
		return nil, errors.New("invalid cell agent configuration in runtime snapshot")
	}
	var names []string
	if agent.ServerCA != "" {
		if agent.ServerCA != "/etc/headmesh-cellagent/server-ca.pem" {
			return nil, errors.New("cell agent trust file must use its deployed path")
		}
		names = append(names, "headmesh-cellagent/server-ca.pem")
	}
	// The deployment uses one assignment per line for these path variables.
	refs := map[string]string{
		"HEADMESH_GITHUB_CLIENT_SECRET_FILE": "", "HEADMESH_HF_CLIENT_SECRET_FILE": "",
		"HEADMESH_OAUTH_SIGNING_KEY_FILE": "", "HEADMESH_OAUTH_CRYPTO_KEY_FILE": "",
	}
	// Match the order in which deployctl builds the backend's environment.
	for _, name := range []string{"secrets.env", "db.env", "backend.env"} {
		for line := range strings.SplitSeq(string(files["headmesh/prod/"+name]), "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			key = strings.TrimSpace(key)
			if _, recognized := refs[key]; ok && recognized {
				refs[key] = strings.Trim(strings.TrimSpace(value), `"'`)
			}
		}
	}
	seen := make(map[string]bool)
	for name, path := range refs {
		if path == "" || seen[path] {
			continue
		}
		if !runtimeOAuthPath.MatchString(path) {
			return nil, fmt.Errorf("%s must name a file directly under /etc/headmesh/oauth", name)
		}
		seen[path] = true
		names = append(names, strings.TrimPrefix(path, "/etc/"))
	}
	return names, nil
}

func (bundle runtimeBundle) validate(noise string) error {
	if bundle.Version != 1 || bundle.Noise != noise {
		return errors.New("runtime snapshot has an unsupported version or a different Noise identity")
	}
	optional, err := runtimeReferencedFiles(bundle.Files)
	if err != nil {
		return err
	}
	if len(bundle.Files) != len(runtimeRequired)+len(optional) {
		return errors.New("runtime snapshot has missing or unexpected files")
	}
	for _, name := range slices.Concat(runtimeRequired, optional) {
		if n := len(bundle.Files[name]); n == 0 || n > runtimeFileLimit {
			return fmt.Errorf("invalid or missing runtime file %s", name)
		}
	}
	if _, err := tls.X509KeyPair(bundle.Files["headmesh/tls/fullchain.pem"], bundle.Files["headmesh/tls/privkey.pem"]); err != nil {
		return errors.New("runtime TLS certificate and private key do not match")
	}
	if _, err := tls.X509KeyPair(bundle.Files["headmesh-cellagent/client.crt"], bundle.Files["headmesh-cellagent/client.key"]); err != nil {
		return errors.New("runtime cell client certificate and private key do not match")
	}
	return bundle.validateAgent()
}

func (bundle runtimeBundle) validateAgent() error {
	var agent struct {
		ClientCert string   `json:"client_cert"`
		ClientKey  string   `json:"client_key"`
		ServerKeys []string `json:"server_keys"`
	}
	if err := json.Unmarshal(bundle.Files["headmesh-cellagent/cellagent.json"], &agent); err != nil ||
		agent.ClientCert != "/etc/headmesh-cellagent/client.crt" || agent.ClientKey != "/etc/headmesh-cellagent/client.key" {
		return errors.New("runtime cell agent must use the deployed client certificate and key paths")
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(bundle.Files["headmesh/prod/backend/op-signing.key"])))
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("invalid runtime operation-signing key")
	}
	public, ok := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if ok {
		want := base64.StdEncoding.EncodeToString(public)
		for _, key := range agent.ServerKeys {
			if key == want {
				return nil
			}
		}
	}
	return errors.New("runtime cell agent trusts a different operation-signing key")
}
