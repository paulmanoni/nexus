package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/extension/session"
	"github.com/paulmanoni/nexus/v2/httpx"
)

// "Sign in with …": an oidc scheme runs the OpenID Connect
// authorization-code flow (with PKCE) against a provider, verifies the ID
// token with the provider's keys, finds the account through Users and signs
// it in with a session — after that it is an ordinary session.

// Provisioner is an optional Users method: an oidc sign-in whose account
// FindLogin doesn't find calls it with the ID token's claims, to create
// one. Without it, such a sign-in is refused.
type Provisioner interface {
	Provision(ctx context.Context, scheme string, claims map[string]any) (*Identity, error)
}

// oidcProvider is one oidc scheme: its settings and the provider's
// discovered endpoints.
type oidcProvider struct {
	name string
	sc   SchemeSettings

	mu       sync.Mutex
	disc     *oidcDiscovery
	verifier *jwtVerifier
}

type oidcDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// discover fetches <issuer>/.well-known/openid-configuration once (again
// after a failure), and builds the ID-token verifier from it.
func (p *oidcProvider) discover(ctx context.Context) (*oidcDiscovery, *jwtVerifier, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disc != nil {
		return p.disc, p.verifier, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := strings.TrimSuffix(p.sc.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("oidc discovery: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("oidc discovery: GET %s: %s", u, res.Status)
	}
	var d oidcDiscovery
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&d); err != nil {
		return nil, nil, fmt.Errorf("oidc discovery: %w", err)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.JWKSURI == "" {
		return nil, nil, errors.New("oidc discovery: the document lacks an endpoint")
	}
	if d.Issuer == "" {
		d.Issuer = p.sc.Issuer
	}
	jwks := d.JWKSURI
	if p.sc.JWKS != "" {
		jwks = p.sc.JWKS
	}
	v, err := newJWTVerifier(SchemeSettings{JWKS: jwks, Issuer: d.Issuer, Audience: p.sc.ClientID, Subject: "sub", Leeway: p.sc.Leeway})
	if err != nil {
		return nil, nil, err
	}
	p.disc, p.verifier = &d, v
	return p.disc, p.verifier, nil
}

// oidcFlow is what a started sign-in keeps in the session until its
// callback.
type oidcFlow struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Next     string `json:"next"`
}

func flowKey(scheme string) string { return "_auth_oidc_" + scheme }

// redirectURI is the callback URL the provider sends the user back to.
func (p *oidcProvider) redirectURI(r *http.Request) string {
	if strings.HasPrefix(p.sc.Redirect, "http://") || strings.HasPrefix(p.sc.Redirect, "https://") {
		return p.sc.Redirect
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + p.sc.Redirect
}

// start sends the browser to the provider.
func (st *moduleState) oidcStart(p *oidcProvider) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		ctx := c.Request.Context()
		disc, _, err := p.discover(ctx)
		if err != nil {
			nexus.WriteError(c, nexus.Errf(nexus.Unavailable, "sign-in with %s is unavailable: %v", p.name, err))
			return
		}
		if !session.Present(ctx) {
			nexus.WriteError(c, errors.New("auth: oidc needs a session"))
			return
		}
		f := oidcFlow{State: newToken(), Nonce: newToken(), Verifier: newToken(), Next: safeNext(c.Query(st.config.settings.nextParam))}
		b, _ := json.Marshal(f)
		session.Get(ctx).Set(flowKey(p.name), string(b))
		sum := sha256.Sum256([]byte(f.Verifier))
		q := url.Values{
			"response_type":         {"code"},
			"client_id":             {p.sc.ClientID},
			"redirect_uri":          {p.redirectURI(c.Request)},
			"scope":                 {strings.Join(p.sc.Scopes, " ")},
			"state":                 {f.State},
			"nonce":                 {f.Nonce},
			"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
			"code_challenge_method": {"S256"},
		}
		sep := "?"
		if strings.Contains(disc.AuthorizationEndpoint, "?") {
			sep = "&"
		}
		c.Redirect(http.StatusFound, disc.AuthorizationEndpoint+sep+q.Encode())
	}
}

// callback finishes a sign-in: checks state, exchanges the code, verifies
// the ID token and its nonce, finds the account and signs it in.
func (st *moduleState) oidcCallback(p *oidcProvider) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		ctx := c.Request.Context()
		fail := func(msg string) {
			if login := st.config.settings.login; login != "" {
				c.Redirect(http.StatusFound, login+"?error=oidc")
				return
			}
			nexus.WriteError(c, nexus.Err(nexus.Unauthenticated, "sign-in with "+p.name+" failed: "+msg))
		}
		if !session.Present(ctx) {
			fail("no session")
			return
		}
		s := session.Get(ctx)
		raw := s.GetString(flowKey(p.name))
		s.Delete(flowKey(p.name))
		var f oidcFlow
		if raw == "" || json.Unmarshal([]byte(raw), &f) != nil || c.Query("state") != f.State || f.State == "" {
			fail("the sign-in was not started here, or it expired")
			return
		}
		if e := c.Query("error"); e != "" {
			fail("the provider said " + e)
			return
		}
		disc, verifier, err := p.discover(ctx)
		if err != nil {
			fail(err.Error())
			return
		}
		idToken, err := p.exchange(ctx, disc, c.Query("code"), f.Verifier, p.redirectURI(c.Request))
		if err != nil {
			fail(err.Error())
			return
		}
		claims, reason := verifier.claims(ctx, idToken)
		if reason != "" {
			fail("the ID token: " + reason)
			return
		}
		if claims["nonce"] != f.Nonce {
			fail("the ID token's nonce doesn't match")
			return
		}
		if v, ok := claims["email_verified"].(bool); ok && !v && p.sc.Claim == "email" {
			fail("the provider hasn't verified that email address")
			return
		}
		login, _ := claims[p.sc.Claim].(string)
		if login == "" {
			fail("no " + p.sc.Claim + " claim")
			return
		}
		id, err := st.oidcAccount(ctx, p, login, claims)
		if err != nil {
			fail(err.Error())
			return
		}
		cred, err := SignIn(ctx, id, ReturnTo(f.Next))
		if err != nil {
			fail(err.Error())
			return
		}
		c.Redirect(http.StatusSeeOther, cred.Next)
	}
}

// exchange trades the authorization code for the ID token.
func (p *oidcProvider) exchange(ctx context.Context, disc *oidcDiscovery, code, verifier, redirect string) (string, error) {
	if code == "" {
		return "", errors.New("no authorization code")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {p.sc.ClientID},
		"code_verifier": {verifier},
	}
	if p.sc.ClientSecret != "" {
		form.Set("client_secret", p.sc.ClientSecret)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, disc.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("the token endpoint: %w", err)
	}
	defer res.Body.Close()
	var out struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("the token endpoint: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("the token endpoint: %s %s", out.Error, out.Desc)
	}
	if res.StatusCode != http.StatusOK || out.IDToken == "" {
		return "", fmt.Errorf("the token endpoint answered %s without an id_token", res.Status)
	}
	return out.IDToken, nil
}

// oidcAccount finds the account a provider's sign-in names, or provisions
// it; CheckLogin then applies as for a password sign-in.
func (st *moduleState) oidcAccount(ctx context.Context, p *oidcProvider, login string, claims map[string]any) (*Identity, error) {
	id, _, err := st.config.users.FindLogin(ctx, login)
	if err != nil {
		return nil, err
	}
	if id == nil {
		pr, ok := st.config.users.(Provisioner)
		if !ok {
			return nil, errors.New("no account for " + login)
		}
		if id, err = pr.Provision(ctx, p.name, claims); err != nil {
			return nil, err
		}
		if id == nil {
			return nil, errors.New("no account for " + login)
		}
	}
	if lc, ok := st.config.users.(LoginChecker); ok {
		if err := lc.CheckLogin(ctx, id); err != nil {
			return nil, err
		}
	}
	return id, nil
}
