package cellagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cellv1 "github.com/headmesh-dev/headmesh/gen/cell/v1"
	"github.com/headmesh-dev/headmesh/gen/cell/v1/cellv1connect"
)

// HelloInfo identifies this agent to the server.
type HelloInfo struct {
	AgentVersion         string
	HeadscaleVersion     string
	NoisePublicKeySHA256 string
}

// RunOptions tune the run loop. Zero values use production defaults.
type RunOptions struct {
	TickInterval    time.Duration // how often scheduled revisions are checked (default 1s)
	ObserveInterval time.Duration // how often a full node snapshot is sent (default 30s)
	MinBackoff      time.Duration // reconnect backoff floor (default 1s)
	MaxBackoff      time.Duration // reconnect backoff ceiling (default 30s)
	Logger          *slog.Logger
}

func (o *RunOptions) defaults() {
	if o.TickInterval == 0 {
		o.TickInterval = time.Second
	}
	if o.ObserveInterval == 0 {
		o.ObserveInterval = 30 * time.Second
	}
	if o.MinBackoff == 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff == 0 {
		o.MaxBackoff = 30 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Run keeps a Sync stream to the server open, reconnecting with jittered exponential backoff, and applies the
// policy timeline on every tick whether or not the server is reachable. It returns when ctx is cancelled.
func (a *Agent) Run(ctx context.Context, client cellv1connect.CellServiceClient, hello HelloInfo, opt RunOptions) error {
	opt.defaults()
	outbox := newOutbox(512)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.tickLoop(ctx, outbox, opt)
	}()

	backoff := opt.MinBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := a.session(ctx, client, hello, outbox, opt)
		if ctx.Err() != nil {
			break
		}
		if time.Since(started) > time.Minute {
			backoff = opt.MinBackoff // the session was healthy; reconnect quickly
		}
		opt.Logger.Warn("cell stream ended; reconnecting", "error", err, "in", backoff)
		jitter := time.Duration(rand.Int64N(int64(backoff) / 2)) //nolint:gosec // jitter, not security sensitive
		select {
		case <-ctx.Done():
		case <-time.After(backoff/2 + jitter):
		}
		backoff = min(backoff*2, opt.MaxBackoff)
	}
	wg.Wait()
	return context.Cause(ctx) //nolint:wrapcheck // callers compare against context.Canceled
}

// tickLoop applies due revisions and queues periodic observations, independent of connectivity.
func (a *Agent) tickLoop(ctx context.Context, out *outbox, opt RunOptions) {
	tick := time.NewTicker(opt.TickInterval)
	defer tick.Stop()
	observe := time.NewTicker(opt.ObserveInterval)
	defer observe.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if res := a.Tick(ctx); res != nil {
				if !res.GetOk() {
					opt.Logger.Error("policy revision not applied", "revision", res.GetRevision(), "error", res.GetError(),
						"guardrail", res.GetRefusedByGuardrail())
				}
				out.push(&cellv1.SyncRequest{Msg: &cellv1.SyncRequest_PolicyApplied{PolicyApplied: res}})
			}
		case <-observe.C:
			out.push(&cellv1.SyncRequest{Msg: &cellv1.SyncRequest_Observed{Observed: a.Observe(ctx)}})
		}
	}
}

// session runs one stream until it fails.
func (a *Agent) session(ctx context.Context, client cellv1connect.CellServiceClient, hello HelloInfo, out *outbox, opt RunOptions) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.Sync(ctx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	defer func() { _ = stream.Close() }()

	var sendMu sync.Mutex
	send := func(m *cellv1.SyncRequest) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		if err := stream.Send(m); err != nil {
			return fmt.Errorf("send: %w", err)
		}
		return nil
	}
	st := a.Status()
	if err := send(&cellv1.SyncRequest{Msg: &cellv1.SyncRequest_Hello{Hello: &cellv1.Hello{
		CellName: a.cfg.CellName, AgentVersion: hello.AgentVersion, HeadscaleVersion: hello.HeadscaleVersion,
		NoisePublicKeySha256: hello.NoisePublicKeySHA256, Mode: a.cfg.Mode,
		ActivePolicySha256: st.ActiveSHA256, ActivePolicyRevision: st.ActiveRevision,
	}}}); err != nil {
		return err
	}
	// From here on the receive loop must never block on a send: if both peers send while neither reads, an
	// unbuffered or flow-controlled stream deadlocks. All replies go through the outbox and its sender.
	out.pushFront(&cellv1.SyncRequest{Msg: &cellv1.SyncRequest_Observed{Observed: a.Observe(ctx)}})

	errc := make(chan error, 2)
	go func() { // drain queued reports
		for {
			m, ok := out.pop(ctx)
			if !ok {
				errc <- ctx.Err()
				return
			}
			if err := send(m); err != nil {
				out.unpop(m)
				errc <- err
				return
			}
		}
	}()
	go func() { // handle server messages
		welcomed := false
		for {
			res, err := stream.Receive()
			if err != nil {
				errc <- fmt.Errorf("receive: %w", err)
				return
			}
			switch m := res.GetMsg().(type) {
			case *cellv1.SyncResponse_Welcome:
				if err := a.AcceptServerKey(m.Welcome.GetOpSigningKey()); err != nil {
					errc <- err
					return
				}
				welcomed = true
			case *cellv1.SyncResponse_DesiredPolicy:
				if !welcomed {
					errc <- errors.New("server sent desired state before Welcome")
					return
				}
				if err := a.SetTimeline(m.DesiredPolicy.GetTimeline()); err != nil {
					opt.Logger.Error("rejected desired policy timeline", "error", err)
					continue
				}
				if r := a.Tick(ctx); r != nil {
					out.push(&cellv1.SyncRequest{Msg: &cellv1.SyncRequest_PolicyApplied{PolicyApplied: r}})
				}
			case *cellv1.SyncResponse_Op:
				r := a.HandleOp(ctx, m.Op)
				if !r.GetOk() {
					opt.Logger.Warn("operation not executed", "op", r.GetOpId(), "error", r.GetError(), "guardrail", r.GetRefusedByGuardrail())
				}
				// Operation results go ahead of queued observations: the server is waiting on them.
				out.pushFront(&cellv1.SyncRequest{Msg: &cellv1.SyncRequest_OpResult{OpResult: r}})
			case *cellv1.SyncResponse_Heartbeat:
				out.pushFront(&cellv1.SyncRequest{Msg: &cellv1.SyncRequest_Heartbeat{
					Heartbeat: &cellv1.Heartbeat{At: timestamppb.New(a.cfg.Now())},
				}})
			}
		}
	}()
	err = <-errc
	cancel()
	return err
}

// outbox is a bounded FIFO of reports. When full, the oldest report is dropped: observations are periodic and
// policy results are superseded by the next Hello, so losing the oldest is safe.
type outbox struct {
	mu    sync.Mutex
	items []*cellv1.SyncRequest
	limit int
	ready chan struct{}
}

func newOutbox(limit int) *outbox { return &outbox{limit: limit, ready: make(chan struct{}, 1)} }

func (o *outbox) push(m *cellv1.SyncRequest) {
	o.mu.Lock()
	if len(o.items) >= o.limit {
		o.items = o.items[1:]
	}
	o.items = append(o.items, m)
	o.mu.Unlock()
	select {
	case o.ready <- struct{}{}:
	default:
	}
}

// pushFront queues m ahead of everything else (op results, heartbeats, and a message whose send failed).
func (o *outbox) pushFront(m *cellv1.SyncRequest) {
	o.mu.Lock()
	o.items = append([]*cellv1.SyncRequest{m}, o.items...)
	if len(o.items) > o.limit {
		o.items = o.items[:o.limit]
	}
	o.mu.Unlock()
	select {
	case o.ready <- struct{}{}:
	default:
	}
}

func (o *outbox) unpop(m *cellv1.SyncRequest) { o.pushFront(m) }

func (o *outbox) pop(ctx context.Context) (*cellv1.SyncRequest, bool) {
	for {
		o.mu.Lock()
		if len(o.items) > 0 {
			m := o.items[0]
			o.items = o.items[1:]
			o.mu.Unlock()
			return m, true
		}
		o.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false
		case <-o.ready:
		}
	}
}
