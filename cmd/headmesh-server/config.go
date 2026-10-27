package main

import (
	"errors"
	"log/slog"
	"os"
	"strings"
)

// config is read from the environment so secrets can come from systemd credentials or Docker secrets files.
type config struct {
	DatabaseURL       string // HEADMESH_DATABASE_URL: application role (no BYPASSRLS, not superuser)
	Listen            string // HEADMESH_LISTEN: public API, default :8080 (behind Caddy)
	CellListen        string // HEADMESH_CELL_LISTEN: mTLS cell endpoint, default :7443
	TLSCertFile       string // HEADMESH_TLS_CERT: server certificate for the cell endpoint
	TLSKeyFile        string // HEADMESH_TLS_KEY
	CellCAFile        string // HEADMESH_CELL_CA: CA that signs cell client certificates
	OpSigningKeyFile  string // HEADMESH_OP_SIGNING_KEY: Ed25519 seed (headmesh-server keygen)
	PublicURL         string // HEADMESH_PUBLIC_URL, e.g. https://api.headmesh.dev
	InstallURL        string // HEADMESH_INSTALL_URL, e.g. https://get.headmesh.dev
	TrustForwardedFor bool   // HEADMESH_TRUST_FORWARDED_FOR=true when every request comes through Caddy
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func loadConfig() (config, error) {
	c := config{
		DatabaseURL:       os.Getenv("HEADMESH_DATABASE_URL"),
		Listen:            env("HEADMESH_LISTEN", ":8080"),
		CellListen:        env("HEADMESH_CELL_LISTEN", ":7443"),
		TLSCertFile:       os.Getenv("HEADMESH_TLS_CERT"),
		TLSKeyFile:        os.Getenv("HEADMESH_TLS_KEY"),
		CellCAFile:        os.Getenv("HEADMESH_CELL_CA"),
		OpSigningKeyFile:  os.Getenv("HEADMESH_OP_SIGNING_KEY"),
		PublicURL:         strings.TrimRight(env("HEADMESH_PUBLIC_URL", "https://api.headmesh.dev"), "/"),
		InstallURL:        env("HEADMESH_INSTALL_URL", "https://get.headmesh.dev"),
		TrustForwardedFor: os.Getenv("HEADMESH_TRUST_FORWARDED_FOR") == "true",
	}
	if c.DatabaseURL == "" {
		return c, errors.New("HEADMESH_DATABASE_URL is required")
	}
	return c, nil
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(env("HEADMESH_LOG_LEVEL", "info"))); err != nil {
		return slog.LevelInfo
	}
	return l
}
