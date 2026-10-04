package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
)

// fakeIdP is an OpenID Connect provider: discovery, an authorize endpoint
// that signs the user in at once, a token endpoint checking PKCE, and JWKS.
type fakeIdP struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	email string
	mu    sync.Mutex
	codes map[string]map[string]string // code → the authorize request
}

func newFakeIdP(t *testing.T, email string) *fakeIdP {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	p := &fakeIdP{key: key, email: email, codes: map[string]map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/authorize",
			"token_endpoint": p.srv.URL + "/token", "jwks_uri": p.srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := "code-" + q.Get("state")[:8]
		p.mu.Lock()
		p.codes[code] = map[string]string{"nonce": q.Get("nonce"), "challenge": q.Get("code_challenge"), "client": q.Get("client_id"), "redirect": q.Get("redirect_uri")}
		p.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		req, ok := p.codes[r.PostForm.Get("code")]
		delete(p.codes, r.PostForm.Get("code"))
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != req["challenge"] || r.PostForm.Get("client_secret") != "shh" ||
			r.PostForm.Get("redirect_uri") != req["redirect"] {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		claims := map[string]any{"iss": p.srv.URL, "aud": req["client"], "sub": "g-123", "email": p.email,
			"email_verified": true, "nonce": req["nonce"], "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": p.sign(claims), "token_type": "Bearer"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "n": b64(p.key.N.Bytes()), "e": b64(big.NewInt(int64(p.key.E)).Bytes()),
		}}})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeIdP) sign(claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1"})
	c, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(c)
	d := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, d[:])
	return in + "." + b64(sig)
}

type provisioningUsers struct{ *testUsers }

func (u provisioningUsers) Provision(ctx context.Context, scheme string, claims map[string]any) (*auth.Identity, error) {
	email, _ := claims["email"].(string)
	u.mu.Lock()
	defer u.mu.Unlock()
	a := &account{id: "new", login: email, kind: "customer"}
	u.byID["new"] = a
	return u.identity(a), nil
}

func oidcApp(t *testing.T, idp *fakeIdP, users auth.Users) *httptest.Server {
	t.Helper()
	s := auth.Settings{Schemes: map[string]auth.SchemeSettings{
		"web": {Type: auth.SchemeSession},
		"google": {Type: auth.SchemeOIDC, Issuer: idp.srv.URL, ClientID: "my-app", ClientSecret: "shh",
			Login: "/auth/google", Redirect: "/auth/google/callback"},
	}}
	app, stop, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Users: auth.StaticUsers(users), Settings: &s}),
		nexus.AsRest("GET", "/me", whoAmI),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

func TestOIDCSignIn(t *testing.T) {
	idp := newFakeIdP(t, "ana")
	srv := oidcApp(t, idp, newTestUsers())
	b := newBrowser(t, srv)
	code, body := b.do("GET", "/auth/google?next=/me", "")
	if code != 200 || !strings.Contains(body, `"id":"1"`) || !strings.Contains(body, `"scheme":"web"`) {
		t.Fatalf("after signing in with the provider, /me = %d %s", code, body)
	}

	// A stale or forged callback is refused.
	other := newBrowser(t, srv)
	if code, _ := other.do("GET", "/auth/google/callback?code=x&state=forged", ""); code != 401 {
		t.Fatalf("a callback with no started sign-in = %d", code)
	}
}

func TestOIDCUnknownAccount(t *testing.T) {
	idp := newFakeIdP(t, "nobody@example.com")
	srv := oidcApp(t, idp, newTestUsers())
	if code, body := newBrowser(t, srv).do("GET", "/auth/google?next=/me", ""); code != 401 || !strings.Contains(body, "no account") {
		t.Fatalf("an email with no account = %d %s", code, body)
	}

	srv = oidcApp(t, idp, provisioningUsers{newTestUsers()})
	if code, body := newBrowser(t, srv).do("GET", "/auth/google?next=/me", ""); code != 200 || !strings.Contains(body, `"id":"new"`) {
		t.Fatalf("a Provisioner creates the account: %d %s", code, body)
	}
}

func TestOIDCNeedsASessionScheme(t *testing.T) {
	_, _, err := nexus.InProcess(config.Runtime{}, auth.Module(auth.Config{Users: auth.StaticUsers(newTestUsers()), Settings: &auth.Settings{
		Schemes: map[string]auth.SchemeSettings{"g": {Type: auth.SchemeOIDC, Issuer: "https://x", ClientID: "c", Login: "/l", Redirect: "/r"}},
	}}))
	if err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("err = %v", err)
	}
}
