package auth_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
)

type account struct {
	id, login, hash, kind string
	perms                 []string
	disabled              bool
}

type testUsers struct {
	mu    sync.Mutex
	byID  map[string]*account
	loads atomic.Int32
	sets  []string
}

func newTestUsers() *testUsers {
	bc := auth.BCryptCost(4)
	ana, _ := bc.Hash("correct horse")
	old, _ := auth.PBKDF2().Hash("old format pw")
	off, _ := bc.Hash("disabled pw")
	return &testUsers{byID: map[string]*account{
		"1": {id: "1", login: "ana", hash: ana, kind: "staff", perms: []string{"orders.*"}},
		"2": {id: "2", login: "bo", hash: old, kind: "customer"},
		"3": {id: "3", login: "cy", hash: off, disabled: true},
	}}
}

func (u *testUsers) identity(a *account) *auth.Identity {
	return &auth.Identity{ID: a.id, Kind: a.kind, Perms: a.perms, User: a}
}

func (u *testUsers) FindLogin(ctx context.Context, login string) (*auth.Identity, string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, a := range u.byID {
		if a.login == login {
			return u.identity(a), a.hash, nil
		}
	}
	return nil, "", nil
}

func (u *testUsers) Load(ctx context.Context, id string) (*auth.Identity, error) {
	u.loads.Add(1)
	u.mu.Lock()
	defer u.mu.Unlock()
	if a, ok := u.byID[id]; ok {
		return u.identity(a), nil
	}
	return nil, nil
}

func (u *testUsers) SetPassword(ctx context.Context, id, encoded string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.byID[id].hash = encoded
	u.sets = append(u.sets, id)
	return nil
}

func (u *testUsers) CheckLogin(ctx context.Context, id *auth.Identity) error {
	if id.User.(*account).disabled {
		return nexus.Err(nexus.Forbidden, "this account is disabled")
	}
	return nil
}

type loginIn struct {
	Login    string `json:"login"`
	Password string `json:"password"`
	Scheme   string `json:"scheme"`
}

type me struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Scheme string `json:"scheme"`
}

func signIn(ctx context.Context, in loginIn) (*auth.Credential, error) {
	id, err := auth.Login(ctx, auth.Password{Login: in.Login, Password: in.Password})
	if err != nil {
		return nil, err
	}
	var opts []auth.SignInOption
	if in.Scheme != "" {
		opts = append(opts, auth.Using(in.Scheme))
	}
	return auth.SignIn(ctx, id, opts...)
}

func signOut(ctx context.Context) (*me, error) { return &me{}, auth.SignOut(ctx) }

func whoAmI(ctx context.Context) (*me, error) {
	m := &me{}
	if id := auth.Current(ctx); id != nil {
		m.ID, m.Kind, m.Scheme = id.ID, id.Kind, id.Scheme
	}
	return m, nil
}

func health(ctx context.Context) (*me, error) { return &me{}, nil }

var testSettings = auth.Settings{
	Schemes: map[string]auth.SchemeSettings{
		"web":  {Type: auth.SchemeSession},
		"api":  {Type: auth.SchemeBearer},
		"keys": {Type: auth.SchemeAPIKey, Header: "X-Key"},
	},
	Passwords: auth.PasswordSettings{Hashers: []string{"bcrypt", "pbkdf2"}},
}

func configApp(t *testing.T, users *testUsers, s auth.Settings) *httptest.Server {
	t.Helper()
	app, stop, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Users: auth.UseUsers(func() *testUsers { return users }), Settings: &s}),
		nexus.AsRest("POST", "/login", signIn, auth.Public()),
		nexus.AsRest("POST", "/logout", signOut, auth.Public()),
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

// browser is a cookie-carrying client that echoes the CSRF cookie.
type browser struct {
	t      *testing.T
	srv    *httptest.Server
	client *http.Client
	bearer string
	header map[string]string
}

func newBrowser(t *testing.T, srv *httptest.Server) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, srv: srv, client: &http.Client{Jar: jar}, header: map[string]string{}}
}

func (b *browser) do(method, path, body string) (int, string) {
	b.t.Helper()
	req, _ := http.NewRequest(method, b.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	u, _ := url.Parse(b.srv.URL)
	for _, c := range b.client.Jar.Cookies(u) {
		if c.Name == "csrftoken" {
			req.Header.Set("X-CSRFToken", c.Value)
		}
	}
	if b.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+b.bearer)
	}
	for k, v := range b.header {
		req.Header.Set(k, v)
	}
	res, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(out)
}

func (b *browser) me() me {
	b.t.Helper()
	code, body := b.do("GET", "/me", "")
	if code != 200 {
		b.t.Fatalf("GET /me = %d %s", code, body)
	}
	var m me
	_ = json.Unmarshal([]byte(body), &m)
	return m
}

func TestConfigPathSession(t *testing.T) {
	users := newTestUsers()
	srv := configApp(t, users, testSettings)
	b := newBrowser(t, srv)

	if code, _ := b.do("GET", "/health", ""); code != 200 { // and sets the CSRF cookie
		t.Fatalf("a Public endpoint = %d", code)
	}
	if code, _ := b.do("GET", "/me", ""); code != 401 {
		t.Fatalf("deny by default: anonymous /me = %d", code)
	}
	for _, bad := range []string{`{"login":"ana","password":"nope"}`, `{"login":"nobody","password":"x"}`} {
		code, body := b.do("POST", "/login", bad)
		if code != 422 || !strings.Contains(body, "invalid login or password") {
			t.Fatalf("login %s = %d %s", bad, code, body)
		}
	}
	if code, body := b.do("POST", "/login", `{"login":"cy","password":"disabled pw"}`); code != 403 || !strings.Contains(body, "disabled") {
		t.Fatalf("a disabled account = %d %s", code, body)
	}

	if code, body := b.do("POST", "/login", `{"login":"ana","password":"correct horse"}`); code != 201 {
		t.Fatalf("login = %d %s", code, body)
	}
	if m := b.me(); m.ID != "1" || m.Kind != "staff" || m.Scheme != "web" {
		t.Fatalf("after a session sign-in /me = %+v", m)
	}
	b.me()
	if n := users.loads.Load(); n != 1 {
		t.Fatalf("Users.Load ran %d times for two requests; the per-id cache keeps one", n)
	}
	if code, _ := b.do("POST", "/logout", ""); code != 201 {
		t.Fatalf("logout = %d", code)
	}
	if code, _ := b.do("GET", "/me", ""); code != 401 {
		t.Fatalf("after logout /me = %d", code)
	}
}

func TestConfigPathTokens(t *testing.T) {
	t.Setenv("NEXUS_DEV", "1")
	users := newTestUsers()
	srv := configApp(t, users, testSettings)
	b := newBrowser(t, srv)
	b.do("GET", "/health", "") // sets the CSRF cookie a session app needs

	code, body := b.do("POST", "/login", `{"login":"bo","password":"old format pw","scheme":"api"}`)
	var cred struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	_ = json.Unmarshal([]byte(body), &cred)
	if code != 201 || cred.AccessToken == "" || cred.TokenType != "Bearer" || cred.ExpiresIn != 12*3600 {
		t.Fatalf("token login = %d %s", code, body)
	}
	if len(users.sets) != 1 || !strings.HasPrefix(users.byID["2"].hash, "bcrypt$") {
		t.Fatalf("a pbkdf2 hash is rehashed with the first hasher on sign-in: %v %q", users.sets, users.byID["2"].hash)
	}

	api := newBrowser(t, srv)
	api.bearer = cred.AccessToken
	if m := api.me(); m.ID != "2" || m.Scheme != "api" {
		t.Fatalf("bearer /me = %+v", m)
	}
	if code, _ := api.do("POST", "/logout", ""); code != 201 {
		t.Fatalf("bearer logout = %d", code)
	}
	code, body = api.do("GET", "/me", "")
	if code != 401 || !strings.Contains(body, "api: unknown, expired or revoked token") {
		t.Fatalf("a revoked token = %d %s (nexus dev names the reason)", code, body)
	}

	code, body = b.do("POST", "/login", `{"login":"ana","password":"correct horse","scheme":"keys"}`)
	cred.ExpiresIn, cred.TokenType = 0, ""
	_ = json.Unmarshal([]byte(body), &cred)
	if code != 201 || cred.ExpiresIn != 0 || cred.TokenType != "" {
		t.Fatalf("API key = %d %s", code, body)
	}
	key := newBrowser(t, srv)
	key.header["X-Key"] = cred.AccessToken
	if m := key.me(); m.ID != "1" || m.Scheme != "keys" {
		t.Fatalf("API key /me = %+v", m)
	}
	wrong := newBrowser(t, srv)
	wrong.bearer = cred.AccessToken // a key is not a bearer token
	if code, _ := wrong.do("GET", "/me", ""); code != 401 {
		t.Fatalf("an API key used as a bearer token = %d", code)
	}
}

func TestConfigPathPublicDefault(t *testing.T) {
	s := testSettings
	s.Default = "public"
	srv := configApp(t, newTestUsers(), s)
	if code, body := newBrowser(t, srv).do("GET", "/me", ""); code != 200 || !strings.Contains(body, `"id":""`) {
		t.Fatalf(`[auth] default = "public": /me = %d %s`, code, body)
	}
}

type notUsers struct{}

func (notUsers) FindLogin(ctx context.Context, login string) (*auth.Identity, string, error) {
	return nil, "", nil
}

func TestUseUsersChecksTheType(t *testing.T) {
	_, _, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Users: auth.UseUsers(func() notUsers { return notUsers{} })}))
	if err == nil || !strings.Contains(err.Error(), "no method Load") {
		t.Fatalf("err = %v", err)
	}
}

type wrongChecker struct{ notUsers }

func (wrongChecker) Load(ctx context.Context, id string) (*auth.Identity, error) { return nil, nil }

// CheckLogin with the wrong parameter: nexus would never call it.
func (wrongChecker) CheckLogin(ctx context.Context, id string) error { return nil }

func TestOptionalMethodSignaturesChecked(t *testing.T) {
	_, _, err := nexus.InProcess(config.Runtime{}, auth.Module(auth.Config{Users: auth.StaticUsers(wrongChecker{})}))
	if err == nil || !strings.Contains(err.Error(), "CheckLogin") {
		t.Fatalf("err = %v", err)
	}
}
