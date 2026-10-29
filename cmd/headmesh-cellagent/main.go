// Command headmesh-cellagent runs beside a Headscale server and applies Headmesh's desired state to it under
// local guardrails. It talks to Headscale over the local unix socket and dials the Headmesh server outbound, so
// the cell host never exposes Headscale's API or opens an inbound port.
//
//	headmesh-cellagent run       [-config /etc/headmesh/cellagent.json]
//	headmesh-cellagent status    — print mode, freeze state and active policy revision
//	headmesh-cellagent freeze    — refuse every change until unfreeze (works while the agent runs)
//	headmesh-cellagent unfreeze
//	headmesh-cellagent rollback  — re-apply the last policy that applied cleanly (no server needed)
//	headmesh-cellagent version
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/headmesh-dev/headmesh/core/cellagent"
	cellv1 "github.com/headmesh-dev/headmesh/gen/cell/v1"
	"github.com/headmesh-dev/headmesh/gen/cell/v1/cellv1connect"
	"github.com/headmesh-dev/headmesh/internal/headscale"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

type config struct {
	CellName        string                `json:"cell_name"`
	Mode            string                `json:"mode"` // observe | enforce
	StateDir        string                `json:"state_dir"`
	HeadscaleSocket string                `json:"headscale_socket"`
	HeadscaleHTTP   string                `json:"headscale_http"` // local HTTP listener, for /version and /key
	ServerURL       string                `json:"server_url"`
	ClientCert      string                `json:"client_cert"`
	ClientKey       string                `json:"client_key"`
	ServerCA        string                `json:"server_ca,omitempty"`   // empty: system roots
	ServerName      string                `json:"server_name,omitempty"` // empty: server_url's host
	ServerKeys      []string              `json:"server_keys"`           // base64 Ed25519 public keys
	ProtectedNodes  []uint64              `json:"protected_nodes"`
	Invariants      []cellagent.Invariant `json:"invariants"`
}

func loadConfig(path string) (config, error) {
	cfg := config{
		StateDir: "/var/lib/headmesh-cellagent", HeadscaleSocket: "/var/run/headscale/headscale.sock",
		HeadscaleHTTP: "http://127.0.0.1:8080", Mode: "observe",
	}
	b, err := os.ReadFile(path) //nolint:gosec // the operator chooses the config path on the command line
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.CellName == "" || cfg.ServerURL == "" || len(cfg.ServerKeys) == 0 {
		return cfg, errors.New("config needs cell_name, server_url and at least one server key")
	}
	return cfg, nil
}

func (c config) agentConfig() (cellagent.Config, error) {
	ac := cellagent.Config{CellName: c.CellName, StateDir: c.StateDir, ProtectedNodes: c.ProtectedNodes, Invariants: c.Invariants}
	switch c.Mode {
	case "observe":
		ac.Mode = cellv1.Mode_MODE_OBSERVE
	case "enforce":
		ac.Mode = cellv1.Mode_MODE_ENFORCE
	default:
		return ac, fmt.Errorf("mode must be observe or enforce, got %q", c.Mode)
	}
	for _, k := range c.ServerKeys {
		raw, err := base64.StdEncoding.DecodeString(k)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return ac, fmt.Errorf("server key %q is not a base64 Ed25519 public key", k)
		}
		ac.ServerKeys = append(ac.ServerKeys, raw)
	}
	return ac, nil
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: headmesh-cellagent run|status|freeze|unfreeze|rollback|version [-config path]")
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/headmesh/cellagent.json", "config file")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if cmd == "version" {
		fmt.Println(version)
		return 0
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		logger.Error("config", "error", err)
		return 1
	}
	ac, err := cfg.agentConfig()
	if err != nil {
		logger.Error("config", "error", err)
		return 1
	}
	hs, err := headscale.DialUnix(cfg.HeadscaleSocket)
	if err != nil {
		logger.Error("headscale", "error", err)
		return 1
	}
	defer func() { _ = hs.Close() }()
	agent, err := cellagent.New(ac, hs)
	if err != nil {
		logger.Error("agent", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "status":
		return printJSON(agent.Status())
	case "freeze":
		return check(logger, agent.Freeze())
	case "unfreeze":
		return check(logger, agent.Unfreeze())
	case "rollback":
		return check(logger, agent.Rollback(ctx))
	case "run":
		client, err := serverClient(cfg)
		if err != nil {
			logger.Error("server client", "error", err)
			return 1
		}
		hello := cellagent.HelloInfo{AgentVersion: version}
		hello.HeadscaleVersion, hello.NoisePublicKeySHA256 = headscaleIdentity(ctx, cfg.HeadscaleHTTP, logger)
		logger.Info("starting", "cell", cfg.CellName, "mode", cfg.Mode, "headscale", hello.HeadscaleVersion)
		if err := agent.Run(ctx, client, hello, cellagent.RunOptions{Logger: logger}); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("run", "error", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		return 2
	}
}

// serverClient builds an mTLS HTTP/2 Connect client for the Headmesh server.
func serverClient(cfg config) (cellv1connect.CellServiceClient, error) {
	tlsCfg, err := serverTLS(cfg)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true, IdleConnTimeout: 90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
	}}
	return cellv1connect.NewCellServiceClient(connect.NewClient(connecthttp.NewTransport(httpClient, cfg.ServerURL))), nil
}

// serverTLS is the agent's side of the mTLS handshake. server_name lets an agent dial an address (loopback on the
// server's own host, or a tailnet IP) and still verify the certificate issued for the server's public name.
func serverTLS(cfg config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("load client certificate: %w", err)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ServerName: cfg.ServerName}
	if cfg.ServerCA != "" {
		pem, err := os.ReadFile(cfg.ServerCA)
		if err != nil {
			return nil, fmt.Errorf("read server CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("server CA file has no certificates")
		}
		tlsCfg.RootCAs = pool
	}
	return tlsCfg, nil
}

// headscaleIdentity reads Headscale's version and Noise public key from its local HTTP listener. The key
// fingerprint lets the server notice if this cell has silently become a different tailnet (for example after an
// empty standby was promoted). Failures are logged, not fatal.
func headscaleIdentity(ctx context.Context, base string, logger *slog.Logger) (ver, noiseSHA string) {
	get := func(path string) []byte {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return nil
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			logger.Warn("headscale identity", "path", path, "error", err)
			return nil
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if err != nil || resp.StatusCode != http.StatusOK {
			return nil
		}
		return b
	}
	var v struct {
		Version string `json:"version"`
	}
	if b := get("/version"); b != nil && json.Unmarshal(b, &v) == nil {
		ver = v.Version
	}
	var k struct {
		PublicKey string `json:"publicKey"`
	}
	if b := get("/key?v=109"); b != nil && json.Unmarshal(b, &k) == nil && k.PublicKey != "" {
		sum := sha256.Sum256([]byte(k.PublicKey))
		noiseSHA = hex.EncodeToString(sum[:])
	}
	return ver, noiseSHA
}

func printJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return 1
	}
	return 0
}

func check(logger *slog.Logger, err error) int {
	if err != nil {
		logger.Error("failed", "error", err)
		return 1
	}
	return 0
}
