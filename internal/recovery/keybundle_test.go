package recovery

import (
	"archive/tar"
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyBundleRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("age-keygen"); err != nil {
		t.Skip("age-keygen is required for recovery identity tests")
	}
	dir := t.TempDir()
	source := KeyBundleSources{
		NoiseKey: filepath.Join(dir, noiseName), DERPKey: filepath.Join(dir, derpName), Configuration: filepath.Join(dir, configName),
	}
	parts := map[string][]byte{configName: []byte("server_url: https://headscale.example.test\n")}
	for _, name := range []string{noiseName, derpName} {
		key, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		parts[name] = []byte("privkey:" + hex.EncodeToString(key.Bytes()) + "\n")
	}
	for name, data := range parts {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	expected := validManifest()
	var err error
	expected.Headscale.NoiseKeySHA256, err = machinePublicHash(source.NoiseKey)
	if err != nil {
		t.Fatal(err)
	}
	var identities, recipients []string
	for _, name := range []string{"owner", "recovery", "unrelated"} {
		identity := filepath.Join(dir, name+".agekey")
		// #nosec G204 -- These commands use a disposable local test identity.
		if err := exec.CommandContext(t.Context(), "age-keygen", "-o", identity).Run(); err != nil {
			t.Fatal(err)
		}
		// #nosec G204 -- Read the public recipient for the test identity.
		recipient, err := exec.CommandContext(t.Context(), "age-keygen", "-y", identity).Output()
		if err != nil {
			t.Fatal(err)
		}
		identities = append(identities, identity)
		recipients = append(recipients, strings.TrimSpace(string(recipient)))
	}
	for _, format := range []string{"age", "sops"} {
		t.Run(format, func(t *testing.T) {
			if _, err := exec.LookPath(format); err != nil {
				t.Skipf("%s is required to test this format", format)
			}
			var encrypted bytes.Buffer
			if err := PackKeyBundle(t.Context(), source, recipients[:2], format, &encrypted); err != nil {
				t.Fatal(err)
			}
			for _, data := range parts {
				if bytes.Contains(encrypted.Bytes(), bytes.TrimSpace(data)) {
					t.Fatal("encrypted bundle contains plaintext source data")
				}
			}
			bundle := filepath.Join(dir, "bundle."+format)
			if err := os.WriteFile(bundle, encrypted.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, identity := range identities[:2] {
				stage := filepath.Join(dir, format+"-"+filepath.Base(identity))
				opt := KeyBundleStage{BundledFile: bundle, IdentityFile: identity, Format: format, OutputDir: stage}
				if err := StageKeyBundle(t.Context(), expected, opt); err != nil {
					t.Fatal(err)
				}
				if err := StageKeyBundle(t.Context(), expected, opt); !errors.Is(err, os.ErrExist) {
					t.Fatalf("second stage must preserve the existing directory: %v", err)
				}
				assertStagedKeyParts(t, stage, parts)
			}
			wrongNoise := validManifest()
			for _, test := range []struct {
				name     string
				identity string
				manifest Manifest
				message  string
			}{
				{"wrong-identity", identities[2], expected, "decrypt Headscale key bundle"},
				{"wrong-noise-key", identities[0], wrongNoise, "Noise identity differs"},
			} {
				stage := filepath.Join(dir, format+"-"+test.name)
				opt := KeyBundleStage{BundledFile: bundle, IdentityFile: test.identity, Format: format, OutputDir: stage}
				if err := StageKeyBundle(t.Context(), test.manifest, opt); err == nil || !strings.Contains(err.Error(), test.message) {
					t.Fatalf("%s stage returned %v", test.name, err)
				}
				if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed decryption or validation created a staging directory")
				}
			}
		})
	}
}

func assertStagedKeyParts(t *testing.T, stage string, parts map[string][]byte) {
	t.Helper()
	if info, err := os.Stat(stage); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("expected a private staging directory: %v", err)
	}
	for name, want := range parts {
		path := filepath.Join(stage, name)
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("expected a private %s: %v", name, err)
		}
		got, err := readBundleFile(path, keyPartLimit(name))
		if err != nil || !bytes.Equal(want, got) {
			t.Fatalf("incorrect staged %s: %v", name, err)
		}
	}
}

func TestUnpackKeyBundleRejectsUnexpectedEntries(t *testing.T) {
	regular := tar.Header{Name: configName, Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}
	for name, entries := range map[string][]tar.Header{
		"duplicate": {regular, regular},
		"traversal": {{Name: "../config.yaml", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}},
		"symlink":   {{Name: noiseName, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
	} {
		t.Run(name, func(t *testing.T) {
			var data bytes.Buffer
			writer := tar.NewWriter(&data)
			for _, entry := range entries {
				if err := writer.WriteHeader(&entry); err != nil {
					t.Fatal(err)
				}
				if entry.Size == 1 {
					if _, err := writer.Write([]byte("x")); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := unpackKeyBundle(data.Bytes()); err == nil {
				t.Fatal("accepted an unexpected archive entry")
			}
		})
	}
}
