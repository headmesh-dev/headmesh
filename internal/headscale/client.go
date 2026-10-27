// Package headscale wraps the Headscale v0.29 gRPC API with the narrow set of calls Headmesh needs.
//
// Production cell agents dial Headscale's local unix socket, which Headscale trusts without an API key, so no
// all-powerful Headscale API key ever leaves the cell host. DialRemote exists only for the approval-gated
// fallback described in the plan (§9.4).
package headscale

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	headscalev1 "github.com/headmesh-dev/headmesh/gen/headscale/v1"
)

// API is the subset of Headscale used by Headmesh. Client implements it; tests use fakes.
type API interface {
	ListNodes(ctx context.Context) ([]*headscalev1.Node, error)
	CreatePreAuthKey(ctx context.Context, req PreAuthKeyRequest) (*headscalev1.PreAuthKey, error)
	SetTags(ctx context.Context, nodeID uint64, tags []string) (*headscalev1.Node, error)
	RenameNode(ctx context.Context, nodeID uint64, name string) (*headscalev1.Node, error)
	DeleteNode(ctx context.Context, nodeID uint64) error
	ExpireNode(ctx context.Context, nodeID uint64, disableExpiry bool) (*headscalev1.Node, error)
	SetApprovedRoutes(ctx context.Context, nodeID uint64, routes []string) (*headscalev1.Node, error)
	GetPolicy(ctx context.Context) (string, error)
	SetPolicy(ctx context.Context, hujson string) error
	CheckPolicy(ctx context.Context, hujson string) error
	Health(ctx context.Context) error
}

// PreAuthKeyRequest describes a pre-auth key. Headmesh always mints tagged keys without a user, so enrolled
// nodes are tagged (owned by tagged-devices) and never expire.
type PreAuthKeyRequest struct {
	Tags       []string
	Reusable   bool
	Ephemeral  bool
	Expiration time.Time
}

// Client is a Headscale gRPC client.
type Client struct {
	conn *grpc.ClientConn
	api  headscalev1.HeadscaleServiceClient
	// callTimeout bounds every call so a wedged Headscale cannot stall the agent.
	callTimeout time.Duration
}

var _ API = (*Client)(nil)

// DialUnix connects to Headscale's local unix socket (default /var/run/headscale/headscale.sock).
func DialUnix(socketPath string) (*Client, error) {
	conn, err := grpc.NewClient("unix://"+socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial headscale socket %s: %w", socketPath, err)
	}
	return &Client{conn: conn, api: headscalev1.NewHeadscaleServiceClient(conn), callTimeout: 10 * time.Second}, nil
}

// DialRemote connects to a remote Headscale gRPC endpoint over TLS with an API key. Headscale API keys have full
// admin rights; use only with Aaryn's approval, behind a firewall rule that admits only the Headmesh server.
func DialRemote(target, apiKey string, tlsConfig *tls.Config) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("headscale: remote connections require an API key")
	}
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	}
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithPerRPCCredentials(bearer(apiKey)))
	if err != nil {
		return nil, fmt.Errorf("dial headscale %s: %w", target, err)
	}
	return &Client{conn: conn, api: headscalev1.NewHeadscaleServiceClient(conn), callTimeout: 10 * time.Second}, nil
}

// Close releases the connection.
func (c *Client) Close() error {
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("close headscale connection: %w", err)
	}
	return nil
}

type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}

func (bearer) RequireTransportSecurity() bool { return true }

func (c *Client) ctx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.callTimeout)
}

func (c *Client) ListNodes(ctx context.Context) ([]*headscalev1.Node, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	resp, err := c.api.ListNodes(ctx, &headscalev1.ListNodesRequest{})
	if err != nil {
		return nil, fmt.Errorf("headscale ListNodes: %w", err)
	}
	return resp.GetNodes(), nil
}

func (c *Client) CreatePreAuthKey(ctx context.Context, req PreAuthKeyRequest) (*headscalev1.PreAuthKey, error) {
	if len(req.Tags) == 0 {
		return nil, errors.New("headscale: Headmesh only mints tagged pre-auth keys")
	}
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	resp, err := c.api.CreatePreAuthKey(ctx, &headscalev1.CreatePreAuthKeyRequest{
		Reusable: req.Reusable, Ephemeral: req.Ephemeral, Expiration: timestamppb.New(req.Expiration), AclTags: req.Tags,
	})
	if err != nil {
		return nil, fmt.Errorf("headscale CreatePreAuthKey: %w", err)
	}
	return resp.GetPreAuthKey(), nil
}

func (c *Client) SetTags(ctx context.Context, nodeID uint64, tags []string) (*headscalev1.Node, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	resp, err := c.api.SetTags(ctx, &headscalev1.SetTagsRequest{NodeId: nodeID, Tags: tags})
	if err != nil {
		return nil, fmt.Errorf("headscale SetTags: %w", err)
	}
	return resp.GetNode(), nil
}

func (c *Client) RenameNode(ctx context.Context, nodeID uint64, name string) (*headscalev1.Node, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	resp, err := c.api.RenameNode(ctx, &headscalev1.RenameNodeRequest{NodeId: nodeID, NewName: name})
	if err != nil {
		return nil, fmt.Errorf("headscale RenameNode: %w", err)
	}
	return resp.GetNode(), nil
}

func (c *Client) DeleteNode(ctx context.Context, nodeID uint64) error {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	if _, err := c.api.DeleteNode(ctx, &headscalev1.DeleteNodeRequest{NodeId: nodeID}); err != nil {
		return fmt.Errorf("headscale DeleteNode: %w", err)
	}
	return nil
}

func (c *Client) ExpireNode(ctx context.Context, nodeID uint64, disableExpiry bool) (*headscalev1.Node, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	resp, err := c.api.ExpireNode(ctx, &headscalev1.ExpireNodeRequest{NodeId: nodeID, DisableExpiry: disableExpiry})
	if err != nil {
		return nil, fmt.Errorf("headscale ExpireNode: %w", err)
	}
	return resp.GetNode(), nil
}

func (c *Client) SetApprovedRoutes(ctx context.Context, nodeID uint64, routes []string) (*headscalev1.Node, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	resp, err := c.api.SetApprovedRoutes(ctx, &headscalev1.SetApprovedRoutesRequest{NodeId: nodeID, Routes: routes})
	if err != nil {
		return nil, fmt.Errorf("headscale SetApprovedRoutes: %w", err)
	}
	return resp.GetNode(), nil
}

func (c *Client) GetPolicy(ctx context.Context) (string, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	resp, err := c.api.GetPolicy(ctx, &headscalev1.GetPolicyRequest{})
	if err != nil {
		return "", fmt.Errorf("headscale GetPolicy: %w", err)
	}
	return resp.GetPolicy(), nil
}

func (c *Client) SetPolicy(ctx context.Context, hujson string) error {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	if _, err := c.api.SetPolicy(ctx, &headscalev1.SetPolicyRequest{Policy: hujson}); err != nil {
		return fmt.Errorf("headscale SetPolicy: %w", err)
	}
	return nil
}

func (c *Client) CheckPolicy(ctx context.Context, hujson string) error {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	if _, err := c.api.CheckPolicy(ctx, &headscalev1.CheckPolicyRequest{Policy: hujson}); err != nil {
		return fmt.Errorf("headscale CheckPolicy: %w", err)
	}
	return nil
}

func (c *Client) Health(ctx context.Context) error {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	if _, err := c.api.Health(ctx, &headscalev1.HealthRequest{}); err != nil {
		return fmt.Errorf("headscale Health: %w", err)
	}
	return nil
}
