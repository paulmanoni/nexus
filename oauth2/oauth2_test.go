package oauth2_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/auth"
	"github.com/paulmanoni/nexus/oauth2"
)

// TestModule_PasswordGrantEndToEnd boots a real nexus app with the
// OAuth2 module wired in, runs a password-grant token request, then
// uses the returned bearer to call a /me endpoint guarded by
// auth.Required. Locks in the contract: Authenticator → token → identity.
func TestModule_PasswordGrantEndToEnd(t *testing.T) {
	addr := startApp(t, ":8997", oauth2.Config{
		Authenticator: func(_ context.Context, _, username, password string) (string, error) {
			if username == "alice" && password == "hunter2" {
				return "user-1", nil
			}
			return "", oauth2.ErrInvalidCredentials
		},
		ClientStore: oauth2.NewStaticClientStore(
			oauth2.StaticClient{ID: "cli", Secret: "topsecret"},
		),
		IncludeJTI: true,
		TokenType:  "Bearer",
	})

	form := url.Values{
		"grant_type": {"password"},
		"client_id":  {"cli"},
		"username":   {"alice"},
		"password":   {"hunter2"},
		"scope":      {"read"},
	}
	req, _ := http.NewRequest("POST", addr+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("cli", "topsecret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("token request: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("token endpoint: status=%d body=%s", res.StatusCode, body)
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		JTI         string `json:"jti"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatalf("no access_token in body: %s", body)
	}
	if tok.TokenType != "Bearer" {
		t.Errorf("token_type=%q, want Bearer", tok.TokenType)
	}
	if tok.JTI == "" {
		t.Error("IncludeJTI=true should populate jti")
	}

	// Use the token on a protected endpoint. Identity.ID should be
	// the userID Authenticator returned.
	req2, _ := http.NewRequest("GET", addr+"/me", nil)
	req2.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("me request: %v", err)
	}
	body2, _ := io.ReadAll(res2.Body)
	res2.Body.Close()
	if res2.StatusCode != 200 {
		t.Fatalf("me endpoint: status=%d body=%s", res2.StatusCode, body2)
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body2, &me); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	if me.ID != "user-1" {
		t.Errorf("identity id = %q, want user-1", me.ID)
	}

	// Bad credentials → invalid_grant with friendly description.
	form2 := url.Values{
		"grant_type": {"password"},
		"client_id":  {"cli"},
		"username":   {"alice"},
		"password":   {"wrong"},
	}
	req3, _ := http.NewRequest("POST", addr+"/oauth/token", strings.NewReader(form2.Encode()))
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req3.SetBasicAuth("cli", "topsecret")
	res3, _ := http.DefaultClient.Do(req3)
	body3, _ := io.ReadAll(res3.Body)
	res3.Body.Close()
	if res3.StatusCode != 400 {
		t.Errorf("bad creds: status=%d body=%s", res3.StatusCode, body3)
	}
	if !strings.Contains(string(body3), "invalid_grant") {
		t.Errorf("bad creds body should mention invalid_grant; got %s", body3)
	}
	if !strings.Contains(string(body3), "incorrect") {
		t.Errorf("bad creds body should carry the friendly description; got %s", body3)
	}
}

// TestVerifySpringPassword sweeps the supported password formats so
// the salted-sha1 / bcrypt / {bcrypt} / {noop} matrix doesn't
// silently regress.
func TestVerifySpringPassword(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	bhash := string(hash)

	cases := []struct {
		name, stored, input, scheme string
		ok                          bool
	}{
		{"noop ok", "{noop}letmein", "letmein", "noop", true},
		{"noop bad", "{noop}letmein", "wrong", "noop", false},
		{"empty stored", "", "x", "", false},
		{"bcrypt prefixed ok", "{bcrypt}" + bhash, "secret", "bcrypt", true},
		{"bcrypt raw ok", bhash, "secret", "bcrypt", true},
		{"bcrypt bad", bhash, "wrong", "bcrypt", false},
		{"unknown scheme", "{md5}deadbeef", "x", "md5", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, scheme := oauth2.VerifySpringPassword(tc.stored, tc.input)
			if ok != tc.ok || scheme != tc.scheme {
				t.Errorf("VerifySpringPassword(stored, %q) = (%v, %q); want (%v, %q)",
					tc.input, ok, scheme, tc.ok, tc.scheme)
			}
		})
	}
}

func startApp(t *testing.T, addr string, cfg oauth2.Config) string {
	t.Helper()
	go func() {
		nexus.Run(
			nexus.Config{Server: nexus.ServerConfig{Addr: "127.0.0.1" + addr}, TraceCapacity: 10},
			oauth2.Module(cfg),
			nexus.AsRest("GET", "/me", meHandler, auth.Required()),
		)
	}()
	url := "http://127.0.0.1" + addr
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get(url + "/oauth/token"); err == nil {
			return url
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("oauth2 test app did not start within 3s")
	return ""
}

type meResp struct {
	ID string `json:"id"`
}

func meHandler(ctx context.Context) (meResp, error) {
	id, _ := auth.IdentityFrom(ctx)
	if id == nil {
		return meResp{}, nil
	}
	return meResp{ID: id.ID}, nil
}