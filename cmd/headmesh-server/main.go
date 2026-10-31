// Command headmesh-server runs the Headmesh control server: the public REST API and the mutual-TLS endpoint cell
// agents dial into.
//
//	headmesh-server serve [-recovery]     run the API and the cell endpoint (-recovery after a restore)
//	headmesh-server migrate               apply database migrations (as the schema owner)
//	headmesh-server migrate -down-to N    revert migrations newer than N (a deploy rollback; schema owner)
//	headmesh-server bootstrap ...         create a cell, an org and an admin API key
//	headmesh-server keygen -out FILE      create the Ed25519 operation-signing key
//	headmesh-server ca-init -dir DIR      create the CA that signs cell client certificates
//	headmesh-server cell-cert -dir DIR -cell NAME   issue a client certificate for a cell agent
//
// Configuration comes from HEADMESH_* environment variables (see config.go).
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/headmesh-dev/headmesh/api"
	"github.com/headmesh-dev/headmesh/core/cellserver"
	"github.com/headmesh-dev/headmesh/gen/cell/v1/cellv1connect"
	"github.com/headmesh-dev/headmesh/internal/service"
	"github.com/headmesh-dev/headmesh/internal/store"
)

var version = "dev"

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(logger)
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: headmesh-server serve|migrate|bootstrap|keygen|ca-init|cell-cert|version")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch args[0] {
	case "serve":
		err = serve(ctx, logger, args[1:])
	case "migrate":
		err = migrate(ctx, args[1:])
	case "bootstrap":
		err = bootstrap(ctx, args[1:])
	case "keygen":
		err = keygen(args[1:])
	case "ca-init":
		err = caInit(args[1:])
	case "cell-cert":
		err = cellCert(args[1:])
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		logger.Error(args[0]+" failed", "error", err)
		return 1
	}
	return 0
}

func serve(ctx context.Context, logger *slog.Logger, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.BoolVar(&cfg.Recovery, "recovery", cfg.Recovery, "start in recovery mode: refuse writes until every enforce-mode cell is current")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()
	st := store.New(pool)
	signer, err := readSigningKey(cfg.OpSigningKeyFile)
	if err != nil {
		return err
	}
	cellCA, err := os.ReadFile(cfg.CellCAFile)
	if err != nil {
		return fmt.Errorf("read cell CA: %w", err)
	}
	cas := x509.NewCertPool()
	if !cas.AppendCertsFromPEM(cellCA) {
		return errors.New("cell CA file contains no certificates")
	}
	hub, err := cellserver.NewHub(cellserver.Config{
		Signer: signer, Store: store.NewCellStore(st), Logger: logger, Authenticate: authenticateCell,
	})
	if err != nil {
		return err
	}
	svc := &service.Service{Store: st, Hub: hub, PublicURL: cfg.PublicURL, InstallURL: cfg.InstallURL}

	// Public API (plain HTTP behind Caddy).
	router := chi.NewRouter()
	// No RealIP middleware: it trusts spoofable headers. The API reads X-Forwarded-For only when configured to.
	recovery := &api.Recovery{}
	if cfg.Recovery {
		recovery.Begin()
	}
	router.Use(middleware.RequestID, middleware.Recoverer, middleware.Timeout(60*time.Second), recovery.Middleware)
	api.Health(router, st, recovery)
	api.New(router, api.Options{Service: svc, Logger: logger, TrustForwardedFor: cfg.TrustForwardedFor, Version: version}) //nolint:contextcheck // handlers use each request's context
	apiServer := &http.Server{
		Addr: cfg.Listen, Handler: router, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 90 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10,
	}

	// Cell endpoint: mutual TLS only; certificates are issued by the Headmesh cell CA.
	cellMux := http.NewServeMux()
	cs := connect.NewServer()
	cellv1connect.RegisterCellServiceHandler(cs, hub)
	connecthttp.Mount(cellMux, cs)
	cellServer := &http.Server{
		Addr: cfg.CellListen, Handler: cellMux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 0,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: cas},
	}

	// Keep every connected cell's timeline current: rolls the horizon forward and picks up writes made by other
	// server instances or directly in the database.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for _, c := range hub.ConnectedCells() {
					if err := hub.Refresh(ctx, c); err != nil {
						logger.Error("periodic refresh", "cell", c, "error", err)
					}
				}
			}
		}
	}()

	errc := make(chan error, 2)
	go func() {
		logger.Info("cell endpoint listening", "addr", cfg.CellListen)
		errc <- cellServer.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	}()
	go func() {
		logger.Info("api listening", "addr", cfg.Listen, "recovery", cfg.Recovery)
		errc <- apiServer.ListenAndServe()
	}()
	go func() {
		if cfg.Recovery {
			recoverControlPlane(ctx, svc, hub, recovery, cfg.RecoveryTimeout, logger)
		}
		sweepLoop(ctx, svc, logger)
	}()
	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	// ctx is already cancelled here; shutdown needs its own deadline.
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = apiServer.Shutdown(shutdown)  //nolint:contextcheck // shutdown deadline, not the cancelled serve context
	_ = cellServer.Shutdown(shutdown) //nolint:contextcheck // shutdown deadline, not the cancelled serve context
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// recoverControlPlane runs recovery mode (plan §13.4) on a control plane restored from backup: audit the grants
// that expired while nothing was running, then wait until every enforce-mode cell has applied a policy compiled
// now, and only then open the API to writes. Normal restarts and blue-green deploys skip this, because the cells
// are still connected to the live instance and their precomputed timelines already enforced every expiry. A cell
// that stays down past the timeout doesn't hold every other tenant hostage; it gets the current policy when it
// reconnects.
func recoverControlPlane(ctx context.Context, svc *service.Service, hub service.RecoveryHub, gate *api.Recovery, timeout time.Duration, logger *slog.Logger) {
	defer gate.End()
	if n, err := svc.SweepExpiredGrants(ctx); err != nil {
		logger.Error("recovery: expiry sweep failed", "error", err)
	} else if n > 0 {
		logger.Warn("recovery: grants expired while the control plane was down", "count", n)
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := svc.AwaitCellsCurrent(waitCtx, hub, logger); err != nil {
		logger.Error("recovery: accepting writes with stale cells", "error", err)
		return
	}
	logger.Info("recovery: every enforce-mode cell is current; accepting writes")
}

// sweepLoop audits grant expiries now and then every 30 seconds.
func sweepLoop(ctx context.Context, svc *service.Service, logger *slog.Logger) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		if n, err := svc.SweepExpiredGrants(ctx); err != nil {
			logger.Error("expiry sweep", "error", err)
		} else if n > 0 {
			logger.Info("expiry sweep: audited expired grants", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// authenticateCell maps the verified client certificate to a cell name. The CA only issues certificates whose
// CommonName is a cell name, so the CN is the identity.
func authenticateCell(ctx context.Context) (string, error) {
	info, ok := connecthttp.ServerInfoForContext(ctx)
	if !ok || info.TLS() == nil || len(info.TLS().PeerCertificates) == 0 {
		return "", errors.New("no client certificate")
	}
	cn := info.TLS().PeerCertificates[0].Subject.CommonName
	if cn == "" {
		return "", errors.New("client certificate has no common name")
	}
	return cn, nil
}

func migrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	downTo := fs.Int64("down-to", -1, "revert migrations newer than this version instead of applying pending ones")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("migrate: unexpected arguments %q", fs.Args())
	}
	dsn := os.Getenv("HEADMESH_MIGRATE_DATABASE_URL")
	if dsn == "" {
		return errors.New("HEADMESH_MIGRATE_DATABASE_URL (schema owner) is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()
	if *downTo >= 0 {
		return store.MigrateDownTo(ctx, pool, *downTo)
	}
	return store.Migrate(ctx, pool)
}

func bootstrap(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	var req service.BootstrapRequest
	users := fs.String("headscale-users", "", "comma-separated Headscale users whose personal nodes belong to the org (adopted cells)")
	fs.StringVar(&req.CellName, "cell", "", "cell name (created if missing)")
	fs.StringVar(&req.ServerURL, "server-url", "", "Headscale server_url for a new cell")
	fs.StringVar(&req.BaseDomain, "base-domain", "", "MagicDNS base domain for a new cell")
	fs.StringVar(&req.CellMode, "cell-mode", "observe", "observe or enforce, for a new cell")
	fs.StringVar(&req.OrgSlug, "org", "", "org slug")
	fs.StringVar(&req.OrgName, "org-name", "", "org display name")
	fs.StringVar(&req.Template, "template", "team", "team or locked")
	fs.BoolVar(&req.Platform, "platform", false, "org may own child orgs")
	fs.StringVar(&req.ParentSlug, "parent", "", "parent platform org slug")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *users != "" {
		req.HeadscaleUsers = strings.Split(*users, ",")
	}
	if req.CellName == "" || req.OrgSlug == "" {
		return errors.New("-cell and -org are required")
	}
	if req.OrgName == "" {
		req.OrgName = req.OrgSlug
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()
	res, err := (&service.Service{Store: store.New(pool)}).Bootstrap(ctx, req)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{
		"cell_id": res.CellID, "org_id": res.OrgID, "org_short_id": res.OrgShort,
		"admin_api_key": res.AdminKey.Key, "note": "the API key is shown once; store it in a password manager",
	}); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "", "file to write the private key seed to (created 0400, must not exist)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *out == "" {
		return errors.New("-out is required")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	if err := writeExclusive(*out, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o400); err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(pub))
	fmt.Fprintln(os.Stderr, "public key printed above: add it to server_keys in every cellagent.json")
	return nil
}

func readSigningKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-configured path
	if err != nil {
		return nil, fmt.Errorf("read op-signing key: %w", err)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("op-signing key file must hold a base64 Ed25519 seed (see headmesh-server keygen)")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // operator-chosen path
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}
