package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
)

func revokeMe(ctx context.Context) (*me, error) {
	return &me{}, auth.RevokeUser(ctx, auth.Current(ctx).ID)
}

type pwIn struct {
	Password string `json:"password"`
}

func changePassword(ctx context.Context, in pwIn) (*me, error) {
	return &me{}, auth.SetPassword(ctx, auth.Current(ctx), in.Password)
}

type echoIn struct {
	Text string `json:"text"`
}

func echo(sess *nexus.WSSession, p nexus.Params[echoIn]) error {
	return sess.Send("echo", p.Args)
}

func sessionsApp(t *testing.T, rules auth.SessionRules) *httptest.Server {
	t.Helper()
	s := testSettings
	s.Sessions = rules
	app, stop, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Users: auth.StaticUsers(newTestUsers()), Settings: &s}),
		nexus.AsRest("POST", "/login", signIn, auth.Public()),
		nexus.AsRest("POST", "/revoke-me", revokeMe),
		nexus.AsRest("POST", "/password", changePassword),
		nexus.AsRest("GET", "/me", whoAmI),
		nexus.AsRest("GET", "/health", health, auth.Public()),
		nexus.AsWS("/ws", "echo", echo),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

func signedIn(t *testing.T, srv *httptest.Server, scheme string) *browser {
	t.Helper()
	b := newBrowser(t, srv)
	b.do("GET", "/health", "")
	code, body := b.do("POST", "/login", `{"login":"ana","password":"correct horse","scheme":"`+scheme+`"}`)
	if code != 201 {
		t.Fatalf("sign-in = %d %s", code, body)
	}
	if scheme != "" {
		var c auth.Credential
		_ = json.Unmarshal([]byte(body), &c)
		b.bearer = c.AccessToken
	}
	return b
}

func meCode(b *browser) (int, string) { return b.do("GET", "/me", "") }

func TestRevokeUser(t *testing.T) {
	t.Setenv("NEXUS_DEV", "1")
	srv := sessionsApp(t, auth.SessionRules{})
	one, two, api := signedIn(t, srv, ""), signedIn(t, srv, ""), signedIn(t, srv, "api")
	if code, _ := one.do("POST", "/revoke-me", ""); code != 201 {
		t.Fatalf("revoke = %d", code)
	}
	for name, b := range map[string]*browser{"this session": one, "another session": two, "a token": api} {
		if code, body := meCode(b); code != 401 || !strings.Contains(body, "signed out everywhere") {
			t.Errorf("%s after RevokeUser = %d %s", name, code, body)
		}
	}
	if again := signedIn(t, srv, ""); again.me().ID != "1" {
		t.Fatal("signing in again works")
	}
}

func TestSingleSession(t *testing.T) {
	srv := sessionsApp(t, auth.SessionRules{Single: true})
	first := signedIn(t, srv, "")
	second := signedIn(t, srv, "")
	if code, _ := meCode(first); code != 401 {
		t.Fatalf("the first session after a second sign-in = %d, want 401", code)
	}
	if second.me().ID != "1" {
		t.Fatal("the new session works")
	}
}

func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	srv := sessionsApp(t, auth.SessionRules{})
	other, api, changer := signedIn(t, srv, ""), signedIn(t, srv, "api"), signedIn(t, srv, "")
	if code, body := changer.do("POST", "/password", `{"password":"a much longer passphrase"}`); code != 201 {
		t.Fatalf("set password = %d %s", code, body)
	}
	if changer.me().ID != "1" {
		t.Fatal("the session that changed the password stays signed in")
	}
	for name, b := range map[string]*browser{"another session": other, "a token": api} {
		if code, _ := meCode(b); code != 401 {
			t.Errorf("%s after a password change = %d, want 401", name, code)
		}
	}
	if code, body := changer.do("POST", "/password", `{"password":"12345678"}`); code != 422 || !strings.Contains(body, "password") {
		t.Fatalf("a refused password = %d %s", code, body)
	}

	off := false
	srv = sessionsApp(t, auth.SessionRules{EndOnPasswordChange: &off})
	other, changer = signedIn(t, srv, ""), signedIn(t, srv, "")
	changer.do("POST", "/password", `{"password":"a much longer passphrase"}`)
	if code, _ := meCode(other); code != 200 {
		t.Fatalf("end_on_password_change = false keeps other sessions: %d", code)
	}
}

func TestIdleSession(t *testing.T) {
	t.Setenv("NEXUS_DEV", "1")
	srv := sessionsApp(t, auth.SessionRules{Idle: 400 * time.Millisecond})
	b := signedIn(t, srv, "")
	time.Sleep(200 * time.Millisecond)
	if code, _ := meCode(b); code != 200 {
		t.Fatal("a session used within its idle time stays")
	}
	time.Sleep(1100 * time.Millisecond)
	if code, body := meCode(b); code != 401 || !strings.Contains(body, "idle") {
		t.Fatalf("an idle session = %d %s", code, body)
	}
}

func TestRevokedWebSocketCloses(t *testing.T) {
	srv := sessionsApp(t, auth.SessionRules{})
	api := signedIn(t, srv, "api")
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws",
		http.Header{"Authorization": {"Bearer " + api.bearer}, "Origin": {srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	read := func() map[string]any {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			var m map[string]any
			if err := conn.ReadJSON(&m); err != nil {
				return nil
			}
			if m["type"] != "connection.established" {
				return m
			}
		}
	}
	_ = conn.WriteJSON(map[string]any{"type": "echo", "data": map[string]string{"text": "hi"}})
	if m := read(); m == nil || m["type"] != "echo" {
		t.Fatalf("before revocation: %v", m)
	}
	if code, _ := api.do("POST", "/revoke-me", ""); code != 201 {
		t.Fatalf("revoke = %d", code)
	}
	_ = conn.WriteJSON(map[string]any{"type": "echo", "data": map[string]string{"text": "again"}})
	if m := read(); m == nil || m["type"] != "error" {
		t.Fatalf("after revocation the next message gets an error: %v", m)
	}
	if m := read(); m != nil {
		t.Fatalf("and the connection closes: %v", m)
	}
}
