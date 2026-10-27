// Package cellserver is the server side of the cell protocol: it accepts outbound Sync streams from cell agents,
// pushes each cell's compiled policy timeline, and delivers signed operations whose results callers wait for.
package cellserver

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/headmesh-dev/headmesh/core/cellagent"
	"github.com/headmesh-dev/headmesh/core/policy"
	cellv1 "github.com/headmesh-dev/headmesh/gen/cell/v1"
	"github.com/headmesh-dev/headmesh/gen/cell/v1/cellv1connect"
)

// Store is what the hub needs from persistence.
type Store interface {
	// Snapshot loads the cell's tenancy state for policy compilation.
	Snapshot(ctx context.Context, cell string, now time.Time, horizon time.Duration) (policy.Snapshot, error)
	// RecordHello, RecordObserved and RecordApplied persist what agents report (audit, drift alerts, metrics).
	RecordHello(ctx context.Context, cell string, hello *cellv1.Hello) error
	RecordObserved(ctx context.Context, cell string, obs *cellv1.Observed) error
	RecordApplied(ctx context.Context, cell string, applied *cellv1.PolicyApplied) error
	// NextRevision returns a revision number greater than any previously issued for the cell.
	NextRevision(ctx context.Context, cell string) (uint64, error)
}

// Authenticator maps an incoming stream to the cell it is allowed to speak for (normally by the fingerprint of
// its mTLS client certificate). A Hello naming any other cell is rejected.
type Authenticator func(ctx context.Context) (cell string, err error)

// Config configures a Hub.
type Config struct {
	Signer       ed25519.PrivateKey
	Store        Store
	Authenticate Authenticator
	// Horizon and MaxRevisions bound the precomputed policy timeline (defaults 24h and 32).
	Horizon      time.Duration
	MaxRevisions int
	Policy       policy.Options
	// HeartbeatInterval is how often the server pings each agent (default 15s).
	HeartbeatInterval time.Duration
	Now               func() time.Time
	Logger            *slog.Logger
}

// Hub tracks connected cells. It implements cellv1connect.CellServiceHandler.
type Hub struct {
	cellv1connect.UnimplementedCellServiceHandler
	cfg Config

	mu       sync.Mutex
	sessions map[string]*session
	opSeq    atomic.Uint64
}

// ErrCellOffline is returned when an operation targets a cell with no connected agent.
var ErrCellOffline = errors.New("cell agent is not connected")

// NewHub returns a Hub.
func NewHub(cfg Config) (*Hub, error) {
	if len(cfg.Signer) != ed25519.PrivateKeySize || cfg.Store == nil || cfg.Authenticate == nil {
		return nil, errors.New("cellserver: signer, store and authenticator are required")
	}
	if cfg.Horizon == 0 {
		cfg.Horizon = 24 * time.Hour
	}
	if cfg.MaxRevisions == 0 {
		cfg.MaxRevisions = 32
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 15 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Hub{cfg: cfg, sessions: map[string]*session{}}, nil
}

type session struct {
	cell     string
	out      chan *cellv1.SyncResponse
	done     chan struct{}
	mu       sync.Mutex
	pending  map[string]chan *cellv1.OpResult
	lastSent string // digest of the last timeline pushed, to avoid resending identical state
}

func (s *session) send(ctx context.Context, m *cellv1.SyncResponse) error {
	select {
	case s.out <- m:
		return nil
	case <-s.done:
		return ErrCellOffline
	case <-ctx.Done():
		return fmt.Errorf("cellserver: %w", ctx.Err())
	}
}

// Sync handles one agent stream.
func (h *Hub) Sync(ctx context.Context, stream cellv1connect.CellServiceSyncServerStream) error {
	cell, err := h.cfg.Authenticate(ctx)
	if err != nil {
		return connect.NewError(connect.CodeUnauthenticated, err.Error())
	}
	first, err := stream.Receive()
	if err != nil {
		return fmt.Errorf("cellserver: receive hello: %w", err)
	}
	hello := first.GetHello()
	if hello == nil {
		return connect.NewError(connect.CodeInvalidArgument, "the first message must be Hello")
	}
	if hello.GetCellName() != cell {
		return connect.NewError(connect.CodePermissionDenied, fmt.Sprintf("credentials are for cell %q, not %q", cell, hello.GetCellName()))
	}
	if err := h.cfg.Store.RecordHello(ctx, cell, hello); err != nil {
		return fmt.Errorf("cellserver: record hello: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &session{cell: cell, out: make(chan *cellv1.SyncResponse, 64), done: make(chan struct{}), pending: map[string]chan *cellv1.OpResult{}}
	h.register(s)
	defer h.unregister(s)

	errc := make(chan error, 2)
	go func() { errc <- h.sendLoop(ctx, stream, s) }()
	go func() { errc <- h.recvLoop(ctx, stream, s) }()

	pub, ok := h.cfg.Signer.Public().(ed25519.PublicKey)
	if !ok {
		return errors.New("cellserver: signer has no Ed25519 public key")
	}
	if err := s.send(ctx, &cellv1.SyncResponse{Msg: &cellv1.SyncResponse_Welcome{Welcome: &cellv1.Welcome{
		SessionId: fmt.Sprintf("%s-%d", cell, h.cfg.Now().UnixNano()), ServerTime: timestamppb.New(h.cfg.Now()), OpSigningKey: pub,
	}}}); err != nil {
		return err
	}
	if err := h.pushTimeline(ctx, s); err != nil {
		h.cfg.Logger.Error("initial policy push failed", "cell", cell, "error", err)
	}
	h.cfg.Logger.Info("cell connected", "cell", cell, "agent", hello.GetAgentVersion(), "headscale", hello.GetHeadscaleVersion(), "mode", hello.GetMode())
	select {
	case err = <-errc:
	case <-s.done:
		err = errors.New("replaced by a newer session from the same cell")
	}
	h.cfg.Logger.Info("cell disconnected", "cell", cell, "reason", err)
	return nil
}

func (h *Hub) sendLoop(ctx context.Context, stream cellv1connect.CellServiceSyncServerStream, s *session) error {
	hb := time.NewTicker(h.cfg.HeartbeatInterval)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("cellserver: %w", ctx.Err())
		case m := <-s.out:
			if err := stream.Send(m); err != nil {
				return fmt.Errorf("cellserver: send: %w", err)
			}
		case <-hb.C:
			if err := stream.Send(&cellv1.SyncResponse{Msg: &cellv1.SyncResponse_Heartbeat{
				Heartbeat: &cellv1.Heartbeat{At: timestamppb.New(h.cfg.Now())},
			}}); err != nil {
				return fmt.Errorf("cellserver: heartbeat: %w", err)
			}
		}
	}
}

func (h *Hub) recvLoop(ctx context.Context, stream cellv1connect.CellServiceSyncServerStream, s *session) error {
	for {
		m, err := stream.Receive()
		if err != nil {
			return fmt.Errorf("cellserver: receive: %w", err)
		}
		switch msg := m.GetMsg().(type) {
		case *cellv1.SyncRequest_Observed:
			if err := h.cfg.Store.RecordObserved(ctx, s.cell, msg.Observed); err != nil {
				h.cfg.Logger.Error("record observation", "cell", s.cell, "error", err)
			}
		case *cellv1.SyncRequest_PolicyApplied:
			if !msg.PolicyApplied.GetOk() {
				h.cfg.Logger.Error("cell did not apply policy", "cell", s.cell, "revision", msg.PolicyApplied.GetRevision(),
					"error", msg.PolicyApplied.GetError(), "guardrail", msg.PolicyApplied.GetRefusedByGuardrail())
			}
			if err := h.cfg.Store.RecordApplied(ctx, s.cell, msg.PolicyApplied); err != nil {
				h.cfg.Logger.Error("record policy result", "cell", s.cell, "error", err)
			}
		case *cellv1.SyncRequest_OpResult:
			s.mu.Lock()
			ch, ok := s.pending[msg.OpResult.GetOpId()]
			delete(s.pending, msg.OpResult.GetOpId())
			s.mu.Unlock()
			if ok {
				ch <- msg.OpResult
			}
		case *cellv1.SyncRequest_Heartbeat, *cellv1.SyncRequest_Hello:
		}
	}
}

func (h *Hub) register(s *session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old, ok := h.sessions[s.cell]; ok {
		close(old.done) // a reconnect replaces the previous session
	}
	h.sessions[s.cell] = s
}

func (h *Hub) unregister(s *session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions[s.cell] == s {
		delete(h.sessions, s.cell)
		close(s.done)
	}
}

func (h *Hub) sessionFor(cell string) (*session, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[cell]
	return s, ok
}

// Connected reports whether a cell agent is currently connected.
func (h *Hub) Connected(cell string) bool {
	_, ok := h.sessionFor(cell)
	return ok
}

// Refresh recompiles the cell's policy timeline and pushes it if it changed. Callers invoke it after any write
// that affects policy (grant created or revoked, device enrolled or removed, rule changed). Offline cells are
// skipped; they receive the current timeline when they reconnect.
func (h *Hub) Refresh(ctx context.Context, cell string) error {
	s, ok := h.sessionFor(cell)
	if !ok {
		return nil
	}
	return h.pushTimeline(ctx, s)
}

func (h *Hub) pushTimeline(ctx context.Context, s *session) error {
	now := h.cfg.Now()
	snap, err := h.cfg.Store.Snapshot(ctx, s.cell, now, h.cfg.Horizon)
	if err != nil {
		return fmt.Errorf("cellserver: load snapshot: %w", err)
	}
	revs, err := policy.Timeline(snap, now, h.cfg.Horizon, h.cfg.MaxRevisions, h.cfg.Policy)
	if err != nil {
		return fmt.Errorf("cellserver: compile: %w", err)
	}
	digest := ""
	for _, r := range revs {
		digest += r.Compiled.SHA256 + "@" + r.EffectiveAt.UTC().Format(time.RFC3339Nano) + ";"
	}
	s.mu.Lock()
	unchanged := digest == s.lastSent
	s.mu.Unlock()
	if unchanged {
		return nil
	}
	base, err := h.cfg.Store.NextRevision(ctx, s.cell)
	if err != nil {
		return fmt.Errorf("cellserver: allocate revision: %w", err)
	}
	tl := make([]*cellv1.PolicyRevision, len(revs))
	for i, r := range revs {
		tl[i] = &cellv1.PolicyRevision{
			Revision: base + uint64(i), Sha256: r.Compiled.SHA256, Hujson: string(r.Compiled.HuJSON),
			EffectiveAt: timestamppb.New(r.EffectiveAt),
		}
	}
	if err := s.send(ctx, &cellv1.SyncResponse{Msg: &cellv1.SyncResponse_DesiredPolicy{DesiredPolicy: &cellv1.DesiredPolicy{Timeline: tl}}}); err != nil {
		return err
	}
	s.mu.Lock()
	s.lastSent = digest
	s.mu.Unlock()
	return nil
}

// Do signs op, sends it to the cell and waits for its result (bounded by ctx). The op must not have an id or
// signature; Do assigns both and sets a not-after deadline from ctx (or one minute).
func (h *Hub) Do(ctx context.Context, cell string, op *cellv1.Op) (*cellv1.OpResult, error) {
	s, ok := h.sessionFor(cell)
	if !ok {
		return nil, ErrCellOffline
	}
	op.Id = fmt.Sprintf("op-%d-%d", h.cfg.Now().UnixNano(), h.opSeq.Add(1))
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = h.cfg.Now().Add(time.Minute)
	}
	op.NotAfter = timestamppb.New(deadline)
	if err := cellagent.SignOp(h.cfg.Signer, op); err != nil {
		return nil, err
	}
	ch := make(chan *cellv1.OpResult, 1)
	s.mu.Lock()
	s.pending[op.GetId()] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, op.GetId())
		s.mu.Unlock()
	}()
	if err := s.send(ctx, &cellv1.SyncResponse{Msg: &cellv1.SyncResponse_Op{Op: op}}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-s.done:
		return nil, ErrCellOffline
	case <-ctx.Done():
		return nil, fmt.Errorf("cellserver: waiting for op %s: %w", op.GetId(), ctx.Err())
	}
}
