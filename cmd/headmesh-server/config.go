package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
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
	// Recovery (serve -recovery, or HEADMESH_RECOVERY=true) starts in recovery mode after a restore (plan §13.4).
	Recovery bool
	// HEADMESH_RECOVERY_TIMEOUT: how long recovery mode waits for enforce-mode cells to confirm a current policy.
	RecoveryTimeout time.Duration
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
		Recovery:          os.Getenv("HEADMESH_RECOVERY") == "true",
		RecoveryTimeout:   5 * time.Minute,
	}
	if v := os.Getenv("HEADMESH_RECOVERY_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("HEADMESH_RECOVERY_TIMEOUT: %w", err)
		}
		c.RecoveryTimeout = d
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
