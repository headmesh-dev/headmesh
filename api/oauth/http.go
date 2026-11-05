package oauth

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"html/template"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"

	"github.com/headmesh-dev/headmesh/api/login"
)

type Sessions interface {
	Session(context.Context, string) (login.User, error)
}

type Handler struct {
	Storage  *Storage
	Sessions Sessions
	provider *op.Provider
	register windowLimit
}

// NewHandler uses the plan's pinned, certified OIDC provider for the protocol.
// This adapter adds RFC 8707 resource binding and Headmesh's explicit consent.
func NewHandler(storage *Storage, sessions Sessions, cryptoKey [32]byte) (*Handler, error) {
	if cryptoKey == [32]byte{} {
		return nil, errors.New("OAuth encryption key must be configured")
	}
	provider, err := op.NewProvider(&op.Config{
		CryptoKey: cryptoKey, CodeMethodS256: true, GrantTypeRefreshToken: true,
		SupportedScopes: AllowedScopes, DefaultLogoutRedirectURI: storage.Issuer + "/oauth/account",
	}, storage, op.StaticIssuer(storage.Issuer),
		op.WithCustomAuthEndpoint(op.NewEndpoint("/oauth/authorize")),
		op.WithCustomTokenEndpoint(op.NewEndpoint("/oauth/token")),
		op.WithCustomRevocationEndpoint(op.NewEndpoint("/oauth/revoke")),
		op.WithCustomKeysEndpoint(op.NewEndpoint("/oauth/jwks")),
		op.WithCustomUserinfoEndpoint(op.NewEndpoint("/oauth/userinfo")),
		op.WithAccessTokenVerifierOpts(op.WithSupportedAccessTokenSigningAlgorithms("EdDSA")),
	)
	if err != nil {
		return nil, err
	}
	return &Handler{Storage: storage, Sessions: sessions, provider: provider}, nil
}

// A process-wide bounded registration rate prevents unbounded anonymous DB
// writes, including when the server is behind a proxy. Ingress may limit further.
type windowLimit struct {
	sync.Mutex
	start time.Time
	count int
}

func (l *windowLimit) allow() bool {
	l.Lock()
	defer l.Unlock()
	if time.Since(l.start) >= time.Minute {
		l.start, l.count = time.Now(), 0
	}
	if l.count >= 20 {
		return false
	}
	l.count++
	return true
}

func (h *Handler) HTTP() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", h.metadata)
	mux.HandleFunc("GET /.well-known/openid-configuration", h.metadata)
	mux.HandleFunc("POST /oauth/register", h.registration)
	mux.HandleFunc("GET /oauth/consent", h.consent)
	mux.HandleFunc("POST /oauth/consent", h.consent)
	mux.HandleFunc("GET /oauth/token-status", h.tokenStatus)
	for _, path := range []string{"GET /oauth/authorize", "GET /oauth/authorize/callback", "POST /oauth/token", "POST /oauth/revoke", "GET /oauth/jwks", "GET /oauth/userinfo"} {
		mux.Handle(path, http.HandlerFunc(h.protocol))
	}
	protected := http.NewCrossOriginProtection().Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-transform")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' https://headmesh.dev; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		browser := r.URL.Path == "/oauth/consent"
		if browser {
			protected.ServeHTTP(w, r)
			return
		}
		// Machine endpoints carry no browser credentials. Allow public clients
		// to fetch metadata and exchange their own PKCE-bound credentials.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (h *Handler) metadata(w http.ResponseWriter, _ *http.Request) {
	base := h.Storage.Issuer
	jsonResponse(w, http.StatusOK, map[string]any{
		"issuer": base, "authorization_endpoint": base + "/oauth/authorize", "token_endpoint": base + "/oauth/token",
		"registration_endpoint": base + "/oauth/register", "revocation_endpoint": base + "/oauth/revoke",
		"jwks_uri": base + "/oauth/jwks", "userinfo_endpoint": base + "/oauth/userinfo",
		"response_types_supported": []string{"code"}, "response_modes_supported": []string{"query"},
		"grant_types_supported": []string{"authorization_code", "refresh_token"}, "scopes_supported": AllowedScopes,
		"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"},
		"revocation_endpoint_auth_methods_supported": []string{"none"}, "subject_types_supported": []string{"public"},
		"id_token_signing_alg_values_supported":          []string{"EdDSA"},
		"authorization_response_iss_parameter_supported": true, "client_id_metadata_document_supported": true,
	})
}

func jsonResponse(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func oauthError(w http.ResponseWriter, code, description string) {
	jsonResponse(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": description})
}

func (h *Handler) registration(w http.ResponseWriter, r *http.Request) {
	if !h.register.allow() {
		w.Header().Set("Retry-After", "60")
		jsonResponse(w, http.StatusTooManyRequests, map[string]string{"error": "slow_down"})
		return
	}
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "application/json" {
		oauthError(w, "invalid_client_metadata", "Use application/json")
		return
	}
	var client ClientMetadata
	if err := json.NewDecoder(r.Body).Decode(&client); err != nil {
		oauthError(w, "invalid_client_metadata", "Invalid client metadata")
		return
	}
	created, err := h.Storage.Clients.Register(r.Context(), client)
	if err != nil {
		oauthError(w, "invalid_client_metadata", "Use a public PKCE client with an exact supported redirect URI")
		return
	}
	jsonResponse(w, http.StatusCreated, created)
}

func (h *Handler) protocol(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/oauth/authorize" || r.URL.Path == "/oauth/token" || r.URL.Path == "/oauth/revoke" {
		if r.Method == http.MethodPost {
			media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if media != "application/x-www-form-urlencoded" || r.URL.RawQuery != "" {
				oauthError(w, "invalid_request", "Use a form-encoded body without query parameters")
				return
			}
		}
		if err := r.ParseForm(); err != nil || len(r.URL.RawQuery) > 16<<10 {
			oauthError(w, "invalid_request", "Invalid OAuth parameters")
			return
		}
		for _, values := range r.Form {
			if len(values) != 1 {
				oauthError(w, "invalid_request", "Repeated parameters are not supported")
				return
			}
		}
		if r.Header.Get("Authorization") != "" || r.Form.Get("client_secret") != "" || r.Form.Get("client_assertion") != "" {
			oauthError(w, "invalid_client", "Use a public client and PKCE")
			return
		}
		binding := requestBinding{Resource: r.Form.Get("resource"), ClientID: r.Form.Get("client_id")}
		if binding.ClientID == "" || len(binding.ClientID) > 2048 {
			oauthError(w, "invalid_client", "A client_id is required")
			return
		}
		if r.URL.Path != "/oauth/revoke" {
			u, err := url.Parse(binding.Resource)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || len(binding.Resource) > 2048 {
				oauthError(w, "invalid_target", "An exact HTTPS resource is required")
				return
			}
		}
		if r.URL.Path == "/oauth/authorize" {
			if r.Form.Get("response_type") != "code" || !slices.Contains([]string{"", "query"}, r.Form.Get("response_mode")) || r.Form.Get("code_challenge_method") != "S256" || len(r.Form.Get("code_challenge")) != 43 || r.Form.Get("request") != "" || r.Form.Get("request_uri") != "" {
				oauthError(w, "invalid_request", "Use authorization code with S256 PKCE and query response mode")
				return
			}
		}
		if r.URL.Path == "/oauth/token" && !slices.Contains([]string{"authorization_code", "refresh_token"}, r.Form.Get("grant_type")) {
			oauthError(w, "unsupported_grant_type", "Use authorization_code or refresh_token")
			return
		}
		for _, scope := range strings.Fields(r.Form.Get("scope")) {
			if !slices.Contains(AllowedScopes, scope) {
				oauthError(w, "invalid_scope", "Unknown scope")
				return
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), requestKey{}, binding))
	}
	h.provider.ServeHTTP(&issuerWriter{ResponseWriter: w, issuer: h.Storage.Issuer}, r)
}

// The library validates redirect ownership. Add RFC 9207 issuer identification
// only to its resulting code/error redirects, never to its own login redirect.
type issuerWriter struct {
	http.ResponseWriter
	issuer string
}

func (w *issuerWriter) WriteHeader(status int) {
	if status >= 300 && status < 400 {
		if u, err := url.Parse(w.Header().Get("Location")); err == nil && (u.Query().Has("code") || u.Query().Has("error")) {
			q := u.Query()
			q.Set("iss", w.issuer)
			u.RawQuery = q.Encode()
			w.Header().Set("Location", u.String())
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (h *Handler) consent(w http.ResponseWriter, r *http.Request) {
	session, err := r.Cookie(login.SessionCookie)
	if err != nil {
		http.Redirect(w, r, "/oauth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	user, err := h.Sessions.Session(r.Context(), session.Value)
	if err != nil {
		http.Redirect(w, r, "/oauth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil || len(r.Form["request"]) != 1 || len(r.Form.Get("request")) != 43 {
		http.Error(w, "Authorization request is missing or expired", http.StatusBadRequest)
		return
	}
	id := r.Form.Get("request")
	var auth AuthRequest
	var csrf string
	denied := false
	err = h.Storage.userTx(r.Context(), user.ID, func(tx pgx.Tx) error {
		var data, csrfHash, browserHash []byte
		if err := tx.QueryRow(r.Context(), `SELECT data,csrf_hash,browser_hash FROM oauth_requests WHERE id=$1 AND expires_at>now() AND code_hash IS NULL FOR UPDATE`, id).Scan(&data, &csrfHash, &browserHash); err != nil {
			return err
		}
		if err := json.Unmarshal(data, &auth); err != nil || auth.Done() {
			return errors.New("authorization already completed")
		}
		grants, err := grantScopes(r.Context(), tx, user.ID, auth.Resource)
		if err != nil || !resourceScopeSubset(auth.GetScopes(), grants) {
			return errors.New("no sandbox grant for requested scopes")
		}
		if r.Method == http.MethodPost {
			if len(r.Form["csrf"]) != 1 || len(r.Form["decision"]) != 1 || len(csrfHash) != 32 || !hmac.Equal(csrfHash, hash(r.PostForm.Get("csrf"))) || !hmac.Equal(browserHash, hash(session.Value)) || auth.UserID != user.ID {
				return errors.New("consent browser or CSRF did not match")
			}
			switch r.PostForm.Get("decision") {
			case "deny":
				denied = true
				_, err = tx.Exec(r.Context(), `DELETE FROM oauth_requests WHERE id=$1`, id)
				return err
			case "allow":
				auth.Consented = true
			default:
				return errors.New("invalid consent decision")
			}
		} else {
			csrf, err = secret()
			if err != nil {
				return err
			}
			auth.UserID, auth.AuthenticatedAt = user.ID, time.Now()
			csrfHash, browserHash = hash(csrf), hash(session.Value)
		}
		data, err = json.Marshal(auth)
		if err != nil {
			return err
		}
		_, err = tx.Exec(r.Context(), `UPDATE oauth_requests SET data=$2,csrf_hash=$3,browser_hash=$4 WHERE id=$1`, id, data, csrfHash, browserHash)
		return err
	})
	if err != nil {
		http.Error(w, "This account cannot authorize that sandbox, or the request expired. Check your sandbox access and start again.", http.StatusForbidden)
		return
	}
	if denied {
		u, err := url.Parse(auth.GetRedirectURI())
		if err != nil {
			http.Error(w, "Invalid authorization request", http.StatusBadRequest)
			return
		}
		q := u.Query()
		q.Set("error", "access_denied")
		q.Set("state", auth.GetState())
		q.Set("iss", h.Storage.Issuer)
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodPost {
		http.Redirect(w, r, h.Storage.Issuer+"/oauth/authorize/callback?id="+url.QueryEscape(id), http.StatusSeeOther)
		return
	}
	client, err := h.Storage.Clients.Resolve(r.Context(), auth.GetClientID())
	if err != nil {
		http.Error(w, "Client metadata is unavailable", http.StatusServiceUnavailable)
		return
	}
	redirect, _ := url.Parse(auth.GetRedirectURI())
	// Browsers apply form-action to a form submission's redirect chain too.
	// Permit the already validated callback origin, without allowing arbitrary
	// external form destinations or weakening script-src.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' https://headmesh.dev; form-action 'self' "+redirect.Scheme+"://"+redirect.Host+"; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = consentPage.Execute(w, consentData{Request: id, CSRF: csrf, Client: client.Name, Redirect: redirect.Scheme + "://" + redirect.Host, Resource: auth.Resource, Email: user.Email, Scopes: auth.GetScopes()})
}

type consentData struct {
	Request, CSRF, Client, Redirect, Resource, Email string
	Scopes                                           []string
}

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="icon" href="https://headmesh.dev/favicon.svg"><title>Headmesh · Review access</title><style>body{margin:0;background:#101318;color:#edf0f5;font:16px/1.6 system-ui,sans-serif;display:grid;place-items:center;min-height:100vh}main{max-width:34rem;padding:2rem;overflow-wrap:anywhere}p{color:#bac2cf}button{padding:.8rem 1rem;border:1px solid #495264;background:#202733;color:inherit;border-radius:.6rem;font:inherit;cursor:pointer}button:focus-visible,a:focus-visible{outline:3px solid #9ab6ff;outline-offset:3px}a{color:#bbceff}</style><main><a href="https://headmesh.dev">Headmesh</a><h1>Connect {{.Client}}?</h1><p>Signed in as {{.Email}}</p><p>This app will receive access to <strong>{{.Resource}}</strong>. Its return address is {{.Redirect}}.</p><ul>{{range .Scopes}}<li>{{.}}</li>{{end}}</ul><form method="post" action="/oauth/consent"><input type="hidden" name="request" value="{{.Request}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><button name="decision" value="allow">Allow access</button> <button name="decision" value="deny">Cancel</button></form><p><a href="/oauth/account">Your account</a></p></main></html>`))

func (h *Handler) tokenStatus(w http.ResponseWriter, r *http.Request) {
	// The caller may only inspect the bearer credential it already possesses.
	// No user/session cookie or arbitrary token query parameter is accepted.
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") || len(value) > 16<<10 {
		jsonResponse(w, http.StatusUnauthorized, map[string]bool{"active": false})
		return
	}
	claims, err := op.VerifyAccessToken[*oidc.AccessTokenClaims](op.ContextWithIssuer(r.Context(), h.Storage.Issuer), strings.TrimPrefix(value, "Bearer "), h.provider.AccessTokenVerifier(op.ContextWithIssuer(r.Context(), h.Storage.Issuer)))
	if err != nil || claims.Claims["token_use"] != "access" {
		jsonResponse(w, http.StatusUnauthorized, map[string]bool{"active": false})
		return
	}
	record, err := h.Storage.accessByID(r.Context(), claims.JWTID)
	if err != nil || record.UserID != claims.Subject || len(claims.Audience) != 1 || claims.Audience[0] != record.Resource || claims.Claims["scope"] != strings.Join(record.Scopes, " ") {
		jsonResponse(w, http.StatusUnauthorized, map[string]bool{"active": false})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"active": true, "sub": record.UserID, "jti": record.ID, "aud": record.Resource, "scope": strings.Join(record.Scopes, " ")})
}
