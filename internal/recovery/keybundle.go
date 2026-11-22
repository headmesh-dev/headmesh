package recovery

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	maxKeyFile       = 256
	maxHeadscaleYAML = 1 << 20
	maxKeyArchive    = 2 << 20
	maxCiphertext    = 4 << 20
	noiseName        = "noise_private.key"
	derpName         = "derp_server_private.key"
	configName       = "config.yaml"
)

// KeyBundleSources are the configured Noise key, DERP key and Headscale configuration.
type KeyBundleSources struct {
	NoiseKey, DERPKey, Configuration string
}

// KeyBundleStage identifies an encrypted local or R2 object and its private output directory.
type KeyBundleStage struct {
	BundledFile, IdentityFile, Format, OutputDir string
	Endpoint, Netrc                              string
}

var nativeRecipient = regexp.MustCompile(`^age1[0-9a-z]{58}$`)

// PackKeyBundle writes an age archive or a SOPS YAML envelope. The clear tar
// archive stays in memory and is passed to the encryptor on stdin.
func PackKeyBundle(ctx context.Context, source KeyBundleSources, recipients []string, format string, w io.Writer) error {
	ciphertext, _, err := encryptKeyBundle(ctx, source, recipients, format)
	if err != nil {
		return err
	}
	if _, err := w.Write(ciphertext); err != nil {
		return fmt.Errorf("write encrypted key bundle: %w", err)
	}
	return nil
}

// PublishKeyBundle uses the Noise fingerprint from the same key bytes that are
// encrypted, so recovery can select the bundle by its verified manifest.
func PublishKeyBundle(ctx context.Context, source KeyBundleSources, recipients []string, endpoint, netrc string) error {
	ciphertext, fingerprint, err := encryptKeyBundle(ctx, source, recipients, "age")
	if err != nil {
		return err
	}
	object, err := newRecoveryObject(endpoint, netrc, keyObjectPath(fingerprint))
	if err != nil {
		return err
	}
	scratch, err := os.MkdirTemp("", "headmesh-key-upload-")
	if err != nil {
		return fmt.Errorf("create encrypted bundle workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	headers := filepath.Join(scratch, "headers")
	condition, err := object.writeCondition(ctx, headers)
	if err != nil {
		return fmt.Errorf("check key bundle before upload: %w", err)
	}
	path := filepath.Join(scratch, "headscale.tar.age")
	if err := os.WriteFile(path, ciphertext, 0o600); err != nil {
		return fmt.Errorf("write encrypted bundle for upload: %w", err)
	}
	status, err := object.request(ctx, "--upload-file", path, "--header", condition,
		"--header", "Content-Type: application/octet-stream", "--output", os.DevNull)
	if err != nil {
		return err
	}
	if status != "200" {
		return fmt.Errorf("object store refused conditional key bundle write (HTTP %s)", status)
	}
	return nil
}

// StageKeyBundle decrypts and validates the complete payload before creating a
// new private staging directory. It does not install files on a running cell.
func StageKeyBundle(ctx context.Context, expected Manifest, opt KeyBundleStage) error {
	if err := expected.validate(); err != nil {
		return err
	}
	if (opt.BundledFile == "") == (opt.Netrc == "") || opt.OutputDir == "" ||
		(opt.Netrc != "" && opt.Format != "age") {
		return errors.New("select a local encrypted bundle or R2, and a new staging directory")
	}
	info, err := os.Stat(opt.IdentityFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("staging requires a private age identity file")
	}
	path := opt.BundledFile
	if opt.Netrc != "" {
		object, err := newRecoveryObject(opt.Endpoint, opt.Netrc, keyObjectPath(expected.Headscale.NoiseKeySHA256))
		if err != nil {
			return err
		}
		scratch, err := os.MkdirTemp("", "headmesh-key-download-")
		if err != nil {
			return fmt.Errorf("create encrypted download workspace: %w", err)
		}
		defer func() { _ = os.RemoveAll(scratch) }()
		path = filepath.Join(scratch, "headscale.tar.age")
		status, err := object.request(ctx, "--max-filesize", "4194304", "--output", path)
		if err != nil {
			return err
		}
		if status != "200" {
			return fmt.Errorf("object store refused key bundle download (HTTP %s)", status)
		}
	}
	ciphertext, err := readBundleFile(path, maxCiphertext)
	if err != nil {
		return fmt.Errorf("read encrypted bundle: %w", err)
	}
	payload, err := decryptKeyBundle(ctx, ciphertext, opt.Format, opt.IdentityFile)
	if err != nil {
		return err
	}
	parts, err := unpackKeyBundle(payload)
	if err != nil {
		return err
	}
	noise, err := validateKeyParts(parts)
	if err != nil {
		return err
	}
	if noise != expected.Headscale.NoiseKeySHA256 {
		return errors.New("bundled Noise identity differs from the recovery manifest")
	}
	return stageKeyParts(opt.OutputDir, parts)
}

func keyObjectPath(noise string) string { return "/hm-keys/hs1/" + noise + ".tar.age" }

func validAgeRecipients(recipients []string) bool {
	return len(recipients) == 2 && recipients[0] != recipients[1] &&
		nativeRecipient.MatchString(recipients[0]) && nativeRecipient.MatchString(recipients[1])
}

func encryptKeyBundle(ctx context.Context, source KeyBundleSources, recipients []string, format string) ([]byte, string, error) {
	if !validAgeRecipients(recipients) {
		return nil, "", errors.New("provide two distinct native age recipients")
	}
	parts := make(map[string][]byte, 3)
	for name, path := range map[string]string{
		noiseName: source.NoiseKey, derpName: source.DERPKey, configName: source.Configuration,
	} {
		b, err := readBundleFile(path, keyPartLimit(name))
		if err != nil {
			return nil, "", fmt.Errorf("read Headscale %s: %w", name, err)
		}
		parts[name] = b
	}
	fingerprint, err := validateKeyParts(parts)
	if err != nil {
		return nil, "", err
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, name := range []string{configName, noiseName, derpName} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(parts[name]))}); err != nil {
			return nil, "", fmt.Errorf("write key bundle header: %w", err)
		}
		if _, err := writer.Write(parts[name]); err != nil {
			return nil, "", fmt.Errorf("write key bundle archive: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("finish key bundle archive: %w", err)
	}
	var cmd *exec.Cmd
	switch format {
	case "age":
		// #nosec G204 -- These are validated public recipients.
		cmd = exec.CommandContext(ctx, "age", "-e", "-r", recipients[0], "-r", recipients[1])
	case "sops":
		// #nosec G204 -- Explicit empty config and validated public recipients.
		cmd = exec.CommandContext(ctx, "sops", "--config", os.DevNull, "encrypt", "--input-type", "binary",
			"--output-type", "yaml", "--age", strings.Join(recipients, ","), "/dev/stdin")
	default:
		return nil, "", errors.New("bundle format must be age or sops")
	}
	cmd.Stdin = &archive
	out, err := cmd.Output()
	if err != nil {
		return nil, "", fmt.Errorf("encrypt Headscale key bundle: %w", err)
	}
	return out, fingerprint, nil
}

func decryptKeyBundle(ctx context.Context, ciphertext []byte, format, identity string) ([]byte, error) {
	var cmd *exec.Cmd
	switch format {
	case "age":
		// #nosec G204 -- The operator supplies a local age identity file.
		cmd = exec.CommandContext(ctx, "age", "-d", "-i", identity)
	case "sops":
		// #nosec G204 -- The config path is the null device and the flags are fixed.
		cmd = exec.CommandContext(ctx, "sops", "--config", os.DevNull, "decrypt", "--input-type", "yaml", "--output-type", "binary", "/dev/stdin")
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "SOPS_AGE_") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "SOPS_AGE_KEY_FILE="+identity)
	default:
		return nil, errors.New("bundle format must be age or sops")
	}
	cmd.Stdin = bytes.NewReader(ciphertext)
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("decrypt Headscale key bundle: %w", err)
	}
	if len(data) > maxKeyArchive {
		return nil, errors.New("decrypted key bundle exceeds the archive limit")
	}
	return data, nil
}

func unpackKeyBundle(data []byte) (map[string][]byte, error) {
	reader := tar.NewReader(bytes.NewReader(data))
	parts := make(map[string][]byte, 3)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("invalid key bundle archive")
		}
		limit := keyPartLimit(header.Name)
		if limit == 0 || header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > limit || parts[header.Name] != nil {
			return nil, errors.New("key bundle must contain only one regular file for each expected entry")
		}
		raw, err := io.ReadAll(reader)
		if err != nil {
			return nil, errors.New("key bundle contains a truncated file")
		}
		parts[header.Name] = raw
	}
	return parts, nil
}

func keyPartLimit(name string) int64 {
	switch name {
	case noiseName, derpName:
		return maxKeyFile
	case configName:
		return maxHeadscaleYAML
	default:
		return 0
	}
}

func validateKeyParts(parts map[string][]byte) (string, error) {
	if len(bytes.TrimSpace(parts[configName])) == 0 {
		return "", errors.New("key bundle has no Headscale configuration")
	}
	noise, err := machinePublicHashBytes(parts[noiseName])
	if err != nil {
		return "", fmt.Errorf("invalid bundled Noise key: %w", err)
	}
	if _, err := machinePublicHashBytes(parts[derpName]); err != nil {
		return "", fmt.Errorf("invalid bundled DERP key: %w", err)
	}
	return noise, nil
}

func readBundleFile(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		return nil, errors.New("expected a non-empty regular file within the bundle limit")
	}
	f, err := os.Open(path) // #nosec G304 -- Sources are explicitly selected local files.
	if err != nil {
		return nil, errors.New("cannot open bundle file")
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("cannot read bundle file within the limit")
	}
	return data, nil
}

func stageKeyParts(destination string, parts map[string][]byte) error {
	if err := os.Mkdir(destination, 0o700); err != nil {
		return fmt.Errorf("create new recovery staging directory: %w", err)
	}
	for _, name := range []string{configName, noiseName, derpName} {
		if err := os.WriteFile(filepath.Join(destination, name), parts[name], 0o600); err != nil {
			_ = os.RemoveAll(destination)
			return fmt.Errorf("stage Headscale %s: %w", name, err)
		}
	}
	return nil
}
