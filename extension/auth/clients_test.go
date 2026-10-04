package auth_test

import (
	"context"
	"encoding/json"
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

type dbClients map[string]*auth.Client

func (d dbClients) Client(ctx context.Context, id string) (*auth.Client, error) { return d[id], nil }

func reports(ctx context.Context) (*me, error) { return whoAmI(ctx) }

func clientsApp(t *testing.T, requireClient bool) *httptest.Server {
	t.Helper()
	hash, _ := auth.BCryptCost(4).Hash("db-secret")
	s := testSettings
	s.Schemes = map[string]auth.SchemeSettings{"api": {Type: auth.SchemeBearer, TTL: time.Hour, Refresh: 24 * time.Hour}}
	s.Endpoints = auth.EndpointSettings{Token: "/oauth/token"}
	s.OAuth2 = auth.OAuth2Settings{RequireClient: requireClient, Clients: map[string]auth.ClientSettings{
		"billing": {Secret: "s3cret", Grants: []string{"client_credentials"}, Perms: []string{"reports.view"}},
		"mobile":  {Grants: []string{"password", "refresh_token"}}, // a public client
	}}
	app, stop, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Users: auth.StaticUsers(newTestUsers()), Settings: &s,
			Clients: dbClients{"warehouse": {ID: "warehouse", SecretHash: hash, Kind: "service", Perms: []string{"reports.*"}}}}),
		nexus.AsRest("GET", "/reports", reports, auth.Requires("reports.view")),
		nexus.AsRest("POST", "/revoke-me", revokeMe),
		nexus.AsRest("GET", "/me", whoAmI),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

func token(t *testing.T, srv *httptest.Server, form url.Values, basicID, basicSecret string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicID != "" {
		req.SetBasicAuth(basicID, basicSecret)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return res.StatusCode, m
}

func TestClientCredentials(t *testing.T) {
	srv := clientsApp(t, false)
	cc := url.Values{"grant_type": {"client_credentials"}}

	code, m := token(t, srv, cc, "billing", "s3cret")
	if code != 200 || m["access_token"] == nil || m["refresh_token"] != nil {
		t.Fatalf("client_credentials = %d %v (no refresh token for a client)", code, m)
	}
	b := bearerAs(t, srv, m["access_token"].(string))
	if who := b.me(); who.ID != "client:billing" || who.Kind != "client" {
		t.Fatalf("a client token's identity = %+v", who)
	}
	if code, _ := b.do("GET", "/reports", ""); code != 200 {
		t.Fatalf("the client's perms gate = %d", code)
	}

	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {"warehouse"}, "client_secret": {"db-secret"}}
	if code, m := token(t, srv, form, "", ""); code != 200 {
		t.Fatalf("a Clients (database) client with a hashed secret, in the body = %d %v", code, m)
	}

	for name, c := range map[string]struct {
		form       url.Values
		id, secret string
		code       int
		err        string
	}{
		"wrong secret":      {cc, "billing", "nope", 401, "invalid_client"},
		"unknown client":    {cc, "who", "x", 401, "invalid_client"},
		"no client":         {cc, "", "", 401, "invalid_client"},
		"public client":     {url.Values{"grant_type": {"client_credentials"}, "client_id": {"mobile"}}, "", "", 400, "unauthorized_client"},
		"grant not allowed": {url.Values{"grant_type": {"password"}, "username": {"ana"}, "password": {"correct horse"}}, "billing", "s3cret", 400, "unauthorized_client"},
	} {
		code, m := token(t, srv, c.form, c.id, c.secret)
		if code != c.code || m["error"] != c.err {
			t.Errorf("%s = %d %v, want %d %s", name, code, m, c.code, c.err)
		}
	}

	// A public client signs a user in.
	pw := url.Values{"grant_type": {"password"}, "username": {"ana"}, "password": {"correct horse"}, "client_id": {"mobile"}}
	if code, m := token(t, srv, pw, "", ""); code != 200 || m["refresh_token"] == nil {
		t.Fatalf("password grant through a public client = %d %v", code, m)
	}
}

func TestRequireClient(t *testing.T) {
	srv := clientsApp(t, true)
	pw := url.Values{"grant_type": {"password"}, "username": {"ana"}, "password": {"correct horse"}}
	if code, m := token(t, srv, pw, "", ""); code != 401 || m["error"] != "invalid_client" {
		t.Fatalf("require_client without a client = %d %v", code, m)
	}
	pw.Set("client_id", "mobile")
	if code, _ := token(t, srv, pw, "", ""); code != 200 {
		t.Fatalf("require_client with a known client = %d", code)
	}
}
