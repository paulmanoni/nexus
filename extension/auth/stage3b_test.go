package auth_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
)

func refreshIt(ctx context.Context, in struct {
	Refresh string `json:"refresh"`
}) (*auth.Credential, error) {
	return auth.RefreshToken(ctx, in.Refresh)
}

func tokensApp(t *testing.T, schemes map[string]auth.SchemeSettings) *httptest.Server {
	t.Helper()
	s := testSettings
	s.Schemes = schemes
	s.Endpoints = auth.EndpointSettings{Token: "/oauth/token", Revoke: "/oauth/revoke"}
	app, stop, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Users: auth.StaticUsers(newTestUsers()), Settings: &s}),
		nexus.AsRest("POST", "/login", signIn, auth.Public()),
		nexus.AsRest("POST", "/refresh", refreshIt, auth.Public()),
		nexus.AsRest("POST", "/revoke-me", revokeMe),
		nexus.AsRest("GET", "/me", whoAmI),
		nexus.AsRest("GET", "/health", health, auth.Public()),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

var withRefresh = map[string]auth.SchemeSettings{
	"web": {Type: auth.SchemeSession},
	"api": {Type: auth.SchemeBearer, TTL: time.Hour, Refresh: 24 * time.Hour},
}

func bearerAs(t *testing.T, srv *httptest.Server, tok string) *browser {
	b := newBrowser(t, srv)
	b.bearer = tok
	return b
}

func TestRefreshTokens(t *testing.T) {
	srv := tokensApp(t, withRefresh)
	b := signedIn(t, srv, "api")
	_, body := newBrowserLoggedBody(t, srv)
	var first auth.Credential
	_ = json.Unmarshal([]byte(body), &first)
	if first.RefreshToken == "" || first.AccessToken == "" {
		t.Fatalf("a bearer sign-in with refresh = %s", body)
	}
	if code, _ := meCode(bearerAs(t, srv, first.RefreshToken)); code != 401 {
		t.Fatal("a refresh token is not an access token")
	}
	code, body := b.do("POST", "/refresh", `{"refresh":"`+first.RefreshToken+`"}`)
	var second auth.Credential
	_ = json.Unmarshal([]byte(body), &second)
	if code != 201 || second.AccessToken == "" || second.RefreshToken == "" || second.RefreshToken == first.RefreshToken {
		t.Fatalf("refresh = %d %s", code, body)
	}
	if bearerAs(t, srv, second.AccessToken).me().ID != "1" {
		t.Fatal("the new access token works")
	}
	if code, _ := b.do("POST", "/refresh", `{"refresh":"`+first.RefreshToken+`"}`); code != 401 {
		t.Fatalf("a used refresh token = %d, want 401", code)
	}
	bearerAs(t, srv, second.AccessToken).do("POST", "/revoke-me", "")
	if code, _ := b.do("POST", "/refresh", `{"refresh":"`+second.RefreshToken+`"}`); code != 401 {
		t.Fatalf("a refresh token after RevokeUser = %d, want 401", code)
	}
}

// newBrowserLoggedBody signs ana in through the api scheme and returns the
// response body.
func newBrowserLoggedBody(t *testing.T, srv *httptest.Server) (*browser, string) {
	b := newBrowser(t, srv)
	b.do("GET", "/health", "")
	_, body := b.do("POST", "/login", `{"login":"ana","password":"correct horse","scheme":"api"}`)
	return b, body
}

func postForm(t *testing.T, srv *httptest.Server, path string, form url.Values) (int, map[string]any) {
	t.Helper()
	res, err := http.PostForm(srv.URL+path, form) // no cookies, no CSRF token
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return res.StatusCode, m
}

func TestTokenEndpoint(t *testing.T) {
	srv := tokensApp(t, withRefresh)
	code, m := postForm(t, srv, "/oauth/token", url.Values{"grant_type": {"password"}, "username": {"ana"}, "password": {"correct horse"}})
	if code != 200 || m["access_token"] == nil || m["refresh_token"] == nil || m["token_type"] != "Bearer" {
		t.Fatalf("password grant = %d %v (a session app's CSRF must not block it)", code, m)
	}
	access, refresh := m["access_token"].(string), m["refresh_token"].(string)
	if code, m := postForm(t, srv, "/oauth/token", url.Values{"grant_type": {"password"}, "username": {"ana"}, "password": {"nope"}}); code != 400 || m["error"] != "invalid_grant" {
		t.Fatalf("a wrong password = %d %v", code, m)
	}
	if code, m := postForm(t, srv, "/oauth/token", url.Values{"grant_type": {"authorization_code"}}); code != 400 || m["error"] != "unsupported_grant_type" {
		t.Fatalf("an unsupported grant = %d %v", code, m)
	}
	code, m = postForm(t, srv, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if code != 200 || m["access_token"] == nil {
		t.Fatalf("refresh_token grant = %d %v", code, m)
	}
	if code, _ := postForm(t, srv, "/oauth/revoke", url.Values{"token": {access}}); code != 200 {
		t.Fatalf("revoke = %d", code)
	}
	if code, _ := meCode(bearerAs(t, srv, access)); code != 401 {
		t.Fatal("a revoked access token no longer works")
	}
	if code, _ := postForm(t, srv, "/oauth/revoke", url.Values{"token": {"not-a-token"}}); code != 200 {
		t.Fatal("revoking an unknown token answers 200 (RFC 7009)")
	}
}

// --- jwt -----------------------------------------------------------------

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jwtSign(t *testing.T, alg, kid string, claims map[string]any, sign func([]byte) []byte) string {
	h, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT", "kid": kid})
	c, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(c)
	return in + "." + b64(sign([]byte(in)))
}

func hs256(secret string) func([]byte) []byte {
	return func(in []byte) []byte {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write(in)
		return m.Sum(nil)
	}
}

func claims(sub string, exp time.Duration) map[string]any {
	return map[string]any{"sub": sub, "iss": "https://id.example", "aud": []string{"orders"}, "exp": time.Now().Add(exp).Unix()}
}

func TestJWTScheme(t *testing.T) {
	t.Setenv("NEXUS_DEV", "1")
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "EC", "kid": "k1", "crv": "P-256",
			"x": b64(ecKey.X.FillBytes(make([]byte, 32))), "y": b64(ecKey.Y.FillBytes(make([]byte, 32))),
		}}})
	}))
	defer jwks.Close()

	srv := tokensApp(t, map[string]auth.SchemeSettings{
		"api": {Type: auth.SchemeBearer},
		"hs":  {Type: auth.SchemeJWT, Secret: "s3cret", Issuer: "https://id.example", Audience: "orders"},
	})
	rs := tokensApp(t, map[string]auth.SchemeSettings{"rs": {Type: auth.SchemeJWT, PublicKey: pemKey}})
	es := tokensApp(t, map[string]auth.SchemeSettings{"es": {Type: auth.SchemeJWT, JWKS: jwks.URL}})

	rsSign := func(in []byte) []byte {
		d := sha256.Sum256(in)
		s, _ := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, d[:])
		return s
	}
	esSign := func(in []byte) []byte {
		d := sha256.Sum256(in)
		r, s, _ := ecdsa.Sign(rand.Reader, ecKey, d[:])
		return append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	}

	for name, c := range map[string]struct {
		srv    *httptest.Server
		tok    string
		want   int
		reason string
	}{
		"HS256":            {srv, jwtSign(t, "HS256", "", claims("1", time.Hour), hs256("s3cret")), 200, ""},
		"wrong secret":     {srv, jwtSign(t, "HS256", "", claims("1", time.Hour), hs256("other")), 401, "bad signature"},
		"expired":          {srv, jwtSign(t, "HS256", "", claims("1", -time.Hour), hs256("s3cret")), 401, "token expired"},
		"wrong audience":   {srv, jwtSign(t, "HS256", "", map[string]any{"sub": "1", "iss": "https://id.example", "aud": "billing"}, hs256("s3cret")), 401, "wrong audience"},
		"alg none":         {srv, jwtSign(t, "none", "", claims("1", time.Hour), func([]byte) []byte { return nil }), 401, "not accepted"},
		"unknown user":     {srv, jwtSign(t, "HS256", "", claims("99", time.Hour), hs256("s3cret")), 401, "no longer exists"},
		"RS256":            {rs, jwtSign(t, "RS256", "", claims("2", time.Hour), rsSign), 200, ""},
		"HS256 vs RSA key": {rs, jwtSign(t, "HS256", "", claims("2", time.Hour), hs256(pemKey)), 401, "not accepted"},
		"ES256 via JWKS":   {es, jwtSign(t, "ES256", "k1", claims("1", time.Hour), esSign), 200, ""},
		"unknown kid":      {es, jwtSign(t, "ES256", "k2", claims("1", time.Hour), esSign), 401, "unknown key"},
	} {
		code, body := meCode(bearerAs(t, c.srv, c.tok))
		if code != c.want || c.reason != "" && !strings.Contains(body, c.reason) {
			t.Errorf("%s: /me = %d %s, want %d %q", name, code, body, c.want, c.reason)
		}
	}

	// An opaque nexus token and a JWT share the Authorization header.
	_, body := newBrowserLoggedBody(t, srv)
	var cred auth.Credential
	_ = json.Unmarshal([]byte(body), &cred)
	if m := bearerAs(t, srv, cred.AccessToken).me(); m.Scheme != "api" {
		t.Fatalf("the opaque token goes to the bearer scheme: %+v", m)
	}
	if m := bearerAs(t, srv, jwtSign(t, "HS256", "", claims("1", time.Hour), hs256("s3cret"))).me(); m.Scheme != "hs" {
		t.Fatalf("the JWT goes to the jwt scheme: %+v", m)
	}
}

func TestRevocableJWT(t *testing.T) {
	t.Setenv("NEXUS_DEV", "1")
	srv := tokensApp(t, map[string]auth.SchemeSettings{
		"hs": {Type: auth.SchemeJWT, Secret: "s3cret", Revocable: true},
	})
	old := claims("1", time.Hour)
	old["iat"] = time.Now().Add(-time.Minute).Unix()
	tok := jwtSign(t, "HS256", "", old, hs256("s3cret"))
	b := bearerAs(t, srv, tok)
	if b.me().ID != "1" {
		t.Fatal("a token before any revocation works")
	}
	if code, _ := b.do("POST", "/revoke-me", ""); code != 201 {
		t.Fatalf("revoke = %d", code)
	}
	if code, body := meCode(b); code != 401 || !strings.Contains(body, "signed out everywhere") {
		t.Fatalf("a token issued before RevokeUser = %d %s", code, body)
	}
	fresh := claims("1", time.Hour)
	fresh["iat"] = time.Now().Add(2 * time.Second).Unix()
	if bearerAs(t, srv, jwtSign(t, "HS256", "", fresh, hs256("s3cret"))).me().ID != "1" {
		t.Fatal("a token issued after it works")
	}
	if code, body := meCode(bearerAs(t, srv, jwtSign(t, "HS256", "", claims("1", time.Hour), hs256("s3cret")))); code != 401 || !strings.Contains(body, "iat") {
		t.Fatalf("a revocable scheme refuses a token without iat: %d %s", code, body)
	}
}
