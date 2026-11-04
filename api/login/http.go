package login

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const (
	SessionCookie = "__Host-hm_session"
	flowCookie    = "__Host-hm_login"
)

// Accounts is implemented by the Postgres Store. The interface also allows real
// HTTP flows to be exercised without contacting identity providers in unit tests.
type Accounts interface {
	SaveFlow(context.Context, string, string, Flow) error
	ConsumeFlow(context.Context, string, string, string) (Flow, error)
	Complete(context.Context, Identity, string, string) (User, string, error)
	Session(context.Context, string) (User, error)
	Logout(context.Context, string) error
}

type Handler struct {
	PublicURL string
	Store     Accounts
	Providers []Provider
}

// HTTP routes are registered before the future OAuth authorization server. This
// signs users in; it does not itself issue MCP access tokens.
func (h *Handler) HTTP() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/login", h.page)
	mux.HandleFunc("POST /oauth/login/{provider}", h.begin)
	mux.HandleFunc("GET /oauth/callback/{provider}", h.callback)
	mux.HandleFunc("GET /oauth/account", h.account)
	mux.HandleFunc("POST /oauth/logout", h.logout)
	mux.HandleFunc("GET /.well-known/oauth-cimd", h.cimd)
	protected := http.NewCrossOriginProtection().Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' https://github.com https://huggingface.co; frame-ancestors 'none'; base-uri 'none'")
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		protected.ServeHTTP(w, r)
	})
}

func (h *Handler) provider(name string) Provider {
	for _, p := range h.Providers {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	var providers []string
	for _, p := range h.Providers {
		providers = append(providers, p.Name())
	}
	h.render(w, http.StatusOK, pageData{Providers: providers, Next: safeNext(r.URL.Query().Get("next"))})
}

// safeNext accepts only the account page or this service's consent flow. It
// prevents protocol-relative URLs, encoded paths and arbitrary external returns.
func safeNext(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(next, "\\\r\n") || !slices.Contains([]string{"/oauth/account", "/oauth/consent"}, u.Path) {
		return "/oauth/account"
	}
	return u.String()
}

func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

func setCookie(w http.ResponseWriter, name, value string, seconds int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: seconds})
}

func (h *Handler) begin(w http.ResponseWriter, r *http.Request) {
	p := h.provider(r.PathValue("provider"))
	if p == nil {
		http.Error(w, "Sign-in provider is not configured", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid sign-in request", http.StatusBadRequest)
		return
	}
	f := Flow{Provider: p.Name(), ReturnPath: safeNext(r.PostForm.Get("next"))}
	if r.PostForm.Get("intent") == "link" {
		user, err := h.Store.Session(r.Context(), cookieValue(r, SessionCookie))
		if err != nil {
			http.Error(w, "Sign in before linking another provider", http.StatusUnauthorized)
			return
		}
		f.LinkUserID = user.ID
	} else if intent := r.PostForm.Get("intent"); intent != "" && intent != "login" {
		http.Error(w, "Invalid sign-in intent", http.StatusBadRequest)
		return
	}
	state, e1 := randomSecret()
	browser, e2 := randomSecret()
	verifier, e3 := randomSecret()
	nonce, e4 := randomSecret()
	if errors.Join(e1, e2, e3, e4) != nil {
		http.Error(w, "Unable to start sign-in", http.StatusInternalServerError)
		return
	}
	f.Verifier, f.Nonce = verifier, nonce
	if err := h.Store.SaveFlow(r.Context(), state, browser, f); err != nil {
		http.Error(w, "Unable to save sign-in request", http.StatusServiceUnavailable)
		return
	}
	setCookie(w, flowCookie, browser, 600)
	http.Redirect(w, r, p.AuthURL(state, verifier, nonce), http.StatusSeeOther)
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	p := h.provider(r.PathValue("provider"))
	state, browser := r.URL.Query().Get("state"), cookieValue(r, flowCookie)
	if p == nil || len(state) != 43 || len(browser) != 43 {
		http.Error(w, "Sign-in state is missing or invalid", http.StatusBadRequest)
		return
	}
	f, err := h.Store.ConsumeFlow(r.Context(), state, browser, p.Name())
	if err != nil {
		http.Error(w, "Sign-in expired or browser state did not match; start again", http.StatusBadRequest)
		return
	}
	setCookie(w, flowCookie, "", -1)
	if r.URL.Query().Get("error") != "" || r.URL.Query().Get("code") == "" {
		http.Error(w, "Provider sign-in was not completed", http.StatusBadRequest)
		return
	}
	identity, err := p.Exchange(r.Context(), r.URL.Query().Get("code"), f.Verifier, f.Nonce)
	if err != nil {
		http.Error(w, "Provider identity could not be verified; start again", http.StatusUnauthorized)
		return
	}
	_, token, err := h.Store.Complete(r.Context(), identity, f.LinkUserID, cookieValue(r, SessionCookie))
	if err != nil {
		if errors.Is(err, ErrAccountLink) {
			http.Error(w, ErrAccountLink.Error(), http.StatusConflict)
		} else {
			http.Error(w, "Unable to complete sign-in", http.StatusForbidden)
		}
		return
	}
	setCookie(w, SessionCookie, token, 12*60*60)
	http.Redirect(w, r, safeNext(f.ReturnPath), http.StatusSeeOther)
}

func (h *Handler) account(w http.ResponseWriter, r *http.Request) {
	user, err := h.Store.Session(r.Context(), cookieValue(r, SessionCookie))
	if err != nil {
		http.Redirect(w, r, "/oauth/login", http.StatusSeeOther)
		return
	}
	var providers []string
	for _, p := range h.Providers {
		providers = append(providers, p.Name())
	}
	h.render(w, http.StatusOK, pageData{User: &user, Providers: providers, Next: "/oauth/account"})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.Logout(r.Context(), cookieValue(r, SessionCookie)); err != nil {
		http.Error(w, "Unable to sign out", http.StatusServiceUnavailable)
		return
	}
	setCookie(w, SessionCookie, "", -1)
	http.Redirect(w, r, "/oauth/login", http.StatusSeeOther)
}

func (h *Handler) cimd(w http.ResponseWriter, _ *http.Request) {
	base := strings.TrimRight(h.PublicURL, "/")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"client_id": base + "/.well-known/oauth-cimd", "client_name": "Headmesh", "client_uri": "https://headmesh.dev",
		"redirect_uris": []string{base + "/oauth/callback/huggingface"}, "token_endpoint_auth_method": "none",
		"grant_types": []string{"authorization_code"}, "response_types": []string{"code"},
	})
}

type pageData struct {
	User      *User
	Providers []string
	Next      string
}

var page = template.Must(template.New("login").Funcs(template.FuncMap{"label": func(provider string) string {
	if provider == "github" {
		return "GitHub"
	}
	return "Hugging Face"
}}).Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Headmesh · Sign in</title>
<style>body{margin:0;background:#101318;color:#edf0f5;font:16px/1.6 system-ui,sans-serif;display:grid;place-items:center;min-height:100vh}main{max-width:30rem;padding:2.5rem}h1{letter-spacing:-.04em}p{color:#bac2cf}button{width:100%;padding:.9rem 1.2rem;border:1px solid #495264;background:#202733;color:inherit;border-radius:.6rem;font:inherit;cursor:pointer}button:hover{background:#303c4e}button:focus-visible,a:focus-visible{outline:3px solid #9ab6ff;outline-offset:3px}form{margin:1rem 0}a{color:#bbceff}.brand{font-weight:650;color:inherit;text-decoration:none}.small{font-size:.875rem}</style>
<main><a class="brand" href="https://headmesh.dev">Headmesh</a>{{if .User}}<h1>You’re signed in</h1><p>{{.User.Name}}<br>{{.User.Email}}</p><p>Sandbox access is granted separately. Sign-in alone does not grant access to a machine.</p>{{range .Providers}}<form method="post" action="/oauth/login/{{.}}"><input type="hidden" name="intent" value="link"><input type="hidden" name="next" value="{{$.Next}}"><button>Link {{label .}} to this account</button></form>{{end}}<form method="post" action="/oauth/logout"><button>Sign out</button></form>{{else}}<h1>Sign in to Headmesh</h1><p>Use your account to continue. You’ll review access before connecting an app to a sandbox.</p>{{range .Providers}}<form method="post" action="/oauth/login/{{.}}"><input type="hidden" name="next" value="{{$.Next}}"><button>Continue with {{label .}}</button></form>{{else}}<p>Sign-in providers haven’t been configured yet.</p>{{end}}{{end}}<p class="small"><a href="https://headmesh.dev/privacy">Privacy</a> · <a href="https://headmesh.dev/terms">Terms</a></p></main></html>`))

func (h *Handler) render(w http.ResponseWriter, status int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = page.Execute(w, data)
}
