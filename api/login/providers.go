// Package login signs users into Headmesh with GitHub or Hugging Face.
// Provider credentials establish identity; resource grants separately determine
// which sandbox, if any, that identity may access.
package login

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zitadel/oidc/v3/pkg/client/rp"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"golang.org/x/oauth2"
)

// Identity uses an immutable upstream subject, never an email, as its key.
type Identity struct {
	Provider string
	Subject  string
	Name     string
	Email    string
	Verified bool
}

// Provider implements the upstream authorization-code flow. Tokens remain on
// the server and are discarded after the identity has been verified.
type Provider interface {
	Name() string
	AuthURL(state, verifier, nonce string) string
	Exchange(context.Context, string, string, string) (Identity, error)
}

type nonceKey struct{}

type github struct {
	config  oauth2.Config
	client  *http.Client
	apiBase string
}

func (p *github) Name() string { return "github" }

func (p *github) AuthURL(state, verifier, _ string) string {
	return p.config.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

func (p *github) Exchange(ctx context.Context, code, verifier, _ string) (Identity, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, p.client)
	token, err := p.config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, errors.New("GitHub code exchange failed")
	}
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := providerJSON(ctx, p.client, p.apiBase+"/user", token.AccessToken, &user); err != nil || user.ID <= 0 || user.Login == "" {
		return Identity{}, errors.New("GitHub identity could not be verified")
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := providerJSON(ctx, p.client, p.apiBase+"/user/emails", token.AccessToken, &emails); err != nil {
		return Identity{}, errors.New("GitHub email could not be verified")
	}
	for _, email := range emails {
		if email.Primary && email.Verified && email.Email != "" {
			return Identity{Provider: p.Name(), Subject: fmt.Sprint(user.ID), Name: user.Login, Email: strings.ToLower(email.Email), Verified: true}, nil
		}
	}
	return Identity{}, errors.New("GitHub requires a verified primary email")
}

type huggingface struct{ rp rp.RelyingParty }

func (p *huggingface) Name() string { return "huggingface" }

func (p *huggingface) AuthURL(state, verifier, nonce string) string {
	return p.rp.OAuthConfig().AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("nonce", nonce))
}

func (p *huggingface) Exchange(ctx context.Context, code, verifier, nonce string) (Identity, error) {
	ctx = context.WithValue(ctx, nonceKey{}, nonce)
	tokens, err := rp.CodeExchange[*oidc.IDTokenClaims](ctx, code, p.rp, rp.WithCodeVerifier(verifier))
	if err != nil {
		return Identity{}, errors.New("Hugging Face token verification failed")
	}
	info, err := rp.Userinfo[*oidc.UserInfo](ctx, tokens.AccessToken, tokens.TokenType, tokens.IDTokenClaims.Subject, p.rp)
	if err != nil || info == nil || info.Subject == "" || info.Email == "" || !bool(info.EmailVerified) {
		return Identity{}, errors.New("Hugging Face requires a verified email and matching subject")
	}
	return Identity{Provider: p.Name(), Subject: info.Subject, Name: info.PreferredUsername, Email: strings.ToLower(info.Email), Verified: true}, nil
}

// Config contains application credentials, not personal access tokens.
type Config struct {
	PublicURL      string
	GitHubClientID string
	GitHubSecret   string
	HuggingFace    bool
	HFClientID     string
	HFSecret       string
}

// Providers initializes configured providers. Hugging Face can use CIMD + PKCE
// without an app secret; GitHub requires a registered OAuth app.
func Providers(ctx context.Context, c Config) ([]Provider, error) {
	base := strings.TrimRight(c.PublicURL, "/")
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("browser sign-in needs an HTTPS public origin")
	}
	if (c.GitHubClientID == "") != (c.GitHubSecret == "") {
		return nil, errors.New("GitHub sign-in needs both client ID and client secret")
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var out []Provider
	if c.GitHubClientID != "" {
		out = append(out, &github{config: oauth2.Config{
			ClientID: c.GitHubClientID, ClientSecret: c.GitHubSecret,
			RedirectURL: base + "/oauth/callback/github", Scopes: []string{"read:user", "user:email"},
			Endpoint: oauth2.Endpoint{AuthURL: "https://github.com/login/oauth/authorize", TokenURL: "https://github.com/login/oauth/access_token", AuthStyle: oauth2.AuthStyleInParams},
		}, client: client, apiBase: "https://api.github.com"})
	}
	if c.HuggingFace {
		if c.HFSecret != "" && c.HFClientID == "" {
			return nil, errors.New("Hugging Face client secret needs its registered client ID")
		}
		if c.HFClientID == "" {
			c.HFClientID = base + "/.well-known/oauth-cimd"
		}
		relyingParty, err := rp.NewRelyingPartyOIDC(ctx, "https://huggingface.co", c.HFClientID, c.HFSecret,
			base+"/oauth/callback/huggingface", []string{"openid", "profile", "email"},
			rp.WithHTTPClient(client), rp.WithSigningAlgsFromDiscovery(), rp.WithVerifierOpts(rp.WithNonce(func(ctx context.Context) string {
				nonce, _ := ctx.Value(nonceKey{}).(string)
				return nonce
			})))
		if err != nil {
			return nil, errors.New("Hugging Face OIDC discovery failed")
		}
		if c.HFSecret == "" {
			relyingParty.OAuthConfig().Endpoint.AuthStyle = oauth2.AuthStyleInParams
		}
		out = append(out, &huggingface{rp: relyingParty})
	}
	return out, nil
}

func providerJSON(ctx context.Context, client *http.Client, endpoint, token string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("invalid provider endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Headmesh-Login/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("provider request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errors.New("provider refused identity request")
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target); err != nil {
		return errors.New("invalid provider identity response")
	}
	return nil
}
