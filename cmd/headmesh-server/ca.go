package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// The cell CA is small and offline-friendly: an ECDSA P-256 root that only signs client certificates for cell
// agents (CommonName = cell name, ExtKeyUsage = client auth, 90-day lifetime). Keep ca.key off the server if you
// can; only ca.crt is needed at runtime.

var cellNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

func caInit(args []string) error {
	fs := flag.NewFlagSet("ca-init", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory for ca.crt and ca.key (must not already contain them)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *dir == "" {
		return errors.New("-dir is required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate CA key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "Headmesh cell CA", Organization: []string{"Headmesh"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true,
		MaxPathLen: 0, MaxPathLenZero: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create CA certificate: %w", err)
	}
	if err := writePEM(filepath.Join(*dir, "ca.crt"), "CERTIFICATE", der, 0o444); err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("encode CA key: %w", err)
	}
	return writePEM(filepath.Join(*dir, "ca.key"), "PRIVATE KEY", keyDER, 0o400)
}

func cellCert(args []string) error {
	fs := flag.NewFlagSet("cell-cert", flag.ContinueOnError)
	dir := fs.String("dir", "", "CA directory (ca.crt, ca.key)")
	cell := fs.String("cell", "", "cell name; becomes the certificate CommonName")
	out := fs.String("out", ".", "directory for <cell>.crt and <cell>.key")
	days := fs.Int("days", 90, "validity in days")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if !cellNamePattern.MatchString(*cell) {
		return errors.New("-cell must be a cell name (lowercase letters, digits, dashes)")
	}
	caCert, caKey, err := loadCA(*dir)
	if err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: *cell, Organization: []string{"Headmesh cell"}},
		NotBefore: time.Now().Add(-5 * time.Minute), NotAfter: time.Now().AddDate(0, 0, *days),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("sign certificate: %w", err)
	}
	if err := writePEM(filepath.Join(*out, *cell+".crt"), "CERTIFICATE", der, 0o444); err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("encode key: %w", err)
	}
	if err := writePEM(filepath.Join(*out, *cell+".key"), "PRIVATE KEY", keyDER, 0o400); err != nil {
		return err
	}
	sum := sha256.Sum256(der)
	fmt.Println(hex.EncodeToString(sum[:]))
	fmt.Fprintln(os.Stderr, "certificate SHA-256 printed above; copy the .crt and .key to the cell host's /etc/headmesh")
	return nil
}

func loadCA(dir string) (*x509.Certificate, any, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt")) //nolint:gosec // operator-chosen path
	if err != nil {
		return nil, nil, fmt.Errorf("read ca.crt: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key")) //nolint:gosec // operator-chosen path
	if err != nil {
		return nil, nil, fmt.Errorf("read ca.key: %w", err)
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, nil, errors.New("CA files are not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	key, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	return cert, key, nil
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	return writeExclusive(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return n
}
