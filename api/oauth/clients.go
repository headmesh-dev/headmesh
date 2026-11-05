package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zitadel/oidc/v3/pkg/oidc"
)

// ClientResolver fetches CIMD only over public HTTPS. DNS is checked at the
// actual socket dial, so redirects, proxies and DNS rebinding cannot turn a
// metadata URL into access to this host's private services.
type ClientResolver struct {
	Pool *pgxpool.Pool
	HTTP *http.Client
}

func NewClientResolver(pool *pgxpool.Pool) *ClientResolver {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = publicDial
	return &ClientResolver{Pool: pool, HTTP: &http.Client{Timeout: 10 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	// IsPrivate excludes shared-address space. In particular, 100.64/10 is
	// this product's private tailnet and must never be reachable through CIMD.
	for _, prefix := range nonPublicRanges {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

var nonPublicRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
}

func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid metadata address")
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("metadata DNS lookup failed")
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return nil, errors.New("metadata address is not public")
		}
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("metadata connection failed")
}

func validMetadataURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Path == "" || u.Path == "/" {
		return false
	}
	if u.Port() != "" && u.Port() != "443" {
		return false
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		return publicIP(ip)
	}
	return !strings.EqualFold(u.Hostname(), "localhost") && strings.Contains(u.Hostname(), ".")
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

func validRedirect(raw, metadataURL string) bool {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Host == "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(raw, "*\\\r\n") {
		return false
	}
	if u.Scheme == "http" {
		return loopbackHost(u.Hostname())
	}
	if u.Scheme != "https" {
		return false
	}
	if u.Port() != "" && u.Port() != "443" {
		return false
	}
	if loopbackHost(u.Hostname()) {
		return false
	}
	allowed := []string{"chatgpt.com", "chat.openai.com", "claude.ai", "claude.com", "vscode.dev", "insiders.vscode.dev"}
	if slices.Contains(allowed, u.Hostname()) {
		return true
	}
	if metadataURL != "" {
		meta, err := url.Parse(metadataURL)
		return err == nil && strings.EqualFold(u.Host, meta.Host)
	}
	return false
}

func validateClient(c *ClientMetadata, cimd bool) error {
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 100 || len(c.Redirects) == 0 || len(c.Redirects) > 10 {
		return oidc.ErrInvalidClient().WithDescription("Client name and exact redirect URIs are required")
	}
	if c.Auth != "none" {
		return oidc.ErrInvalidClient().WithDescription("Use token_endpoint_auth_method none with S256 PKCE")
	}
	metadata := ""
	if cimd {
		metadata = c.ID
	}
	for _, uri := range c.Redirects {
		if !validRedirect(uri, metadata) {
			return oidc.ErrInvalidRequest().WithDescription("Unsupported redirect URI")
		}
	}
	for _, grant := range c.Grants {
		if grant != "authorization_code" && grant != "refresh_token" {
			return oidc.ErrUnsupportedGrantType()
		}
	}
	for _, response := range c.Responses {
		if response != "code" {
			return oidc.ErrInvalidClient().WithDescription("Only the code response type is supported")
		}
	}
	c.Grants = []string{"authorization_code", "refresh_token"}
	c.Responses = []string{"code"}
	return nil
}

func (r *ClientResolver) Resolve(ctx context.Context, id string) (*ClientMetadata, error) {
	if len(id) > 2048 {
		return nil, oidc.ErrInvalidClient()
	}
	var data []byte
	err := r.Pool.QueryRow(ctx, `SELECT metadata FROM oauth_clients WHERE id=$1 AND (expires_at IS NULL OR expires_at>now())`, id).Scan(&data)
	if err == nil {
		var c ClientMetadata
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, oidc.ErrInvalidClient()
		}
		return &c, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("client store unavailable")
	}
	if !validMetadataURL(id) {
		return nil, oidc.ErrInvalidClient()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return nil, oidc.ErrInvalidClient()
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Headmesh-OAuth-CIMD/1.0")
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, oidc.ErrInvalidClient().WithDescription("Client metadata could not be fetched")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, oidc.ErrInvalidClient().WithDescription("Client metadata must return HTTP 200 without redirects")
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, oidc.ErrInvalidClient()
	}
	var c ClientMetadata
	if err = json.Unmarshal(data, &c); err != nil || c.ID != id {
		return nil, oidc.ErrInvalidClient().WithDescription("Client metadata ID did not match")
	}
	if err = validateClient(&c, true); err != nil {
		return nil, err
	}
	data, err = json.Marshal(c)
	if err != nil {
		return nil, err
	}
	_, err = r.Pool.Exec(ctx, `INSERT INTO oauth_clients(id,metadata,expires_at) VALUES($1,$2,now()+interval '1 hour') ON CONFLICT(id) DO UPDATE SET metadata=excluded.metadata,expires_at=excluded.expires_at`, id, data)
	if err != nil {
		return nil, errors.New("cannot persist client metadata")
	}
	return &c, nil
}

func (r *ClientResolver) Register(ctx context.Context, c ClientMetadata) (*ClientMetadata, error) {
	id, err := secret()
	if err != nil {
		return nil, err
	}
	c.ID = "hmc_" + id
	if c.Auth == "" {
		c.Auth = "none"
	}
	if err := validateClient(&c, false); err != nil {
		return nil, err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	_, err = r.Pool.Exec(ctx, `INSERT INTO oauth_clients(id,metadata) VALUES($1,$2)`, c.ID, data)
	if err != nil {
		return nil, errors.New("cannot register OAuth client")
	}
	return &c, nil
}
