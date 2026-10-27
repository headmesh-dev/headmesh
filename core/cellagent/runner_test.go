package cellagent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	"google.golang.org/protobuf/types/known/timestamppb"

	cellv1 "github.com/headmesh-dev/headmesh/gen/cell/v1"
	"github.com/headmesh-dev/headmesh/gen/cell/v1/cellv1connect"
)

// fakeServer drives one session: Welcome, a desired policy, one signed op; it records what the agent sends.
type fakeServer struct {
	cellv1connect.UnimplementedCellServiceHandler
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	now  time.Time

	mu       sync.Mutex
	received []*cellv1.SyncRequest
	done     chan struct{}
}

func (f *fakeServer) Sync(ctx context.Context, stream cellv1connect.CellServiceSyncServerStream) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	f.record(first)
	if first.GetHello() == nil {
		return connect.NewError(connect.CodeInvalidArgument, "first message must be Hello")
	}
	if err := stream.Send(&cellv1.SyncResponse{Msg: &cellv1.SyncResponse_Welcome{Welcome: &cellv1.Welcome{
		SessionId: "s1", ServerTime: timestamppb.New(f.now), OpSigningKey: f.pub,
	}}}); err != nil {
		return err
	}
	if err := stream.Send(&cellv1.SyncResponse{Msg: &cellv1.SyncResponse_DesiredPolicy{DesiredPolicy: &cellv1.DesiredPolicy{
		Timeline: []*cellv1.PolicyRevision{rev(1, goodPolicy, f.now)},
	}}}); err != nil {
		return err
	}
	op := &cellv1.Op{
		Id: "op-delete", NotAfter: timestamppb.New(f.now.Add(time.Minute)),
		Kind: &cellv1.Op_DeleteNode{DeleteNode: &cellv1.DeleteNode{NodeId: 42}},
	}
	if err := SignOp(f.priv, op); err != nil {
		return err
	}
	if err := stream.Send(&cellv1.SyncResponse{Msg: &cellv1.SyncResponse_Op{Op: op}}); err != nil {
		return err
	}
	for {
		m, err := stream.Receive()
		if errors.Is(err, io.EOF) || ctx.Err() != nil {
			return nil //nolint:nilerr // the agent hung up or the test finished; both end the session cleanly
		}
		if err != nil {
			return err
		}
		f.record(m)
		if f.complete() {
			close(f.done)
			<-ctx.Done()
			return nil
		}
	}
}

func (f *fakeServer) record(m *cellv1.SyncRequest) {
	f.mu.Lock()
	f.received = append(f.received, m)
	f.mu.Unlock()
}

func (f *fakeServer) complete() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	var applied, op bool
	for _, m := range f.received {
		applied = applied || m.GetPolicyApplied().GetOk()
		op = op || m.GetOpResult().GetOk()
	}
	return applied && op
}

func TestRunnerSyncsPolicyAndOperations(t *testing.T) {
	h := newHarness(t, nil)
	srv := &fakeServer{pub: h.agent.cfg.ServerKeys[0], priv: h.key, now: h.clk.Now(), done: make(chan struct{})}
	server := connect.NewServer()
	cellv1connect.RegisterCellServiceHandler(server, srv)
	client := cellv1connect.NewCellServiceClient(connect.NewClient(connectinprocess.New(server)))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		errc <- h.agent.Run(ctx, client, HelloInfo{AgentVersion: "test"}, RunOptions{
			TickInterval: 10 * time.Millisecond, ObserveInterval: time.Hour,
			Logger: slog.New(slog.DiscardHandler),
		})
	}()
	select {
	case <-srv.done:
	case <-ctx.Done():
		t.Fatal("agent did not report an applied policy and an executed op in time")
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.received[0].GetHello().GetCellName() != "test" {
		t.Fatal("the first message must be Hello with the cell name")
	}
	if h.hs.policy != goodPolicy || len(h.hs.deleted) != 1 || h.hs.deleted[0] != 42 {
		t.Fatalf("headscale state: policy applied=%v deleted=%v", h.hs.policy == goodPolicy, h.hs.deleted)
	}
}
