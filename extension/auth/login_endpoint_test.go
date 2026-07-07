package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/auth"
	"github.com/paulmanoni/nexus/httpx"
)

// loginBackend implements the login + resolve capabilities for the endpoint
// test.
type loginBackend struct{}

func (loginBackend) Resolve(_ context.Context, token string) (*auth.Identity, error) {
	if token == "" {
		return nil, nil
	}
	return &auth.Identity{ID: token}, nil
}
func (loginBackend) Login(_ context.Context, cred auth.Credentials) (*auth.Identity, error) {
	pw, ok := cred.(auth.Password)
	if !ok || pw.Username != "alice" || pw.Password != "s3cret" {
		return nil, auth.ErrInvalidCredentials
	}
	return &auth.Identity{ID: "user-1", Roles: []string{"reader"}}, nil
}

func postJSON(t *testing.T, app *nexus.App, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func newLoginApp(t *testing.T, opts ...auth.LoginOption) (*nexus.App, func(context.Context) error) {
	t.Helper()
	app, stop, err := nexus.InProcess(nexus.Config{},
		auth.Module(auth.Config{
			Authentication: auth.Authentication{Schemes: []auth.Scheme{{Extract: auth.Bearer()}}},
			Backend:        auth.StaticBackend(loginBackend{}),
		}),
		auth.LoginEndpoint(opts...),
	)
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	return app, stop
}

func TestLoginEndpoint_Success(t *testing.T) {
	app, stop := newLoginApp(t)
	defer stop(context.Background())

	rec, body := postJSON(t, app, "/auth/login", `{"username":"alice","password":"s3cret"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	idMap, _ := body["identity"].(map[string]any)
	if idMap == nil || idMap["ID"] != "user-1" {
		t.Fatalf("response identity = %v, want id user-1", body)
	}
}

func TestLoginEndpoint_InvalidCredentials(t *testing.T) {
	app, stop := newLoginApp(t)
	defer stop(context.Background())

	rec, body := postJSON(t, app, "/auth/login", `{"username":"alice","password":"wrong"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if body["error"] == nil {
		t.Errorf("401 body should carry an error, got %v", body)
	}
	if body["identity"] != nil {
		t.Error("failed login must not leak an identity")
	}
}

func TestLoginEndpoint_WithIssuer(t *testing.T) {
	app, stop := newLoginApp(t, auth.LoginAt("/signin"), auth.WithIssuer(
		func(_ context.Context, id *auth.Identity) (any, error) {
			return map[string]any{"token": "tok-for-" + id.ID}, nil
		}))
	defer stop(context.Background())

	rec, body := postJSON(t, app, "/signin", `{"username":"alice","password":"s3cret"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if body["token"] != "tok-for-user-1" {
		t.Errorf("issuer body = %v, want token tok-for-user-1", body)
	}
}

// tokenSvc is a stand-in for an app service (e.g. an OAuth2 server) that the
// issuer needs via DI — the exact shape LoginHandler exists for.
type tokenSvc struct{ prefix string }

func (s *tokenSvc) issue(id string) string { return s.prefix + id }

// TestLoginHandler_DIIssuer proves the exported LoginHandler can be wired in
// an app-owned AsRestHandler factory whose issuer closes over a DI-injected
// service — the pattern for token servers that a static WithIssuer can't see.
func TestLoginHandler_DIIssuer(t *testing.T) {
	app, stop, err := nexus.InProcess(nexus.Config{},
		nexus.Provide(func() *tokenSvc { return &tokenSvc{prefix: "tok:"} }),
		auth.Module(auth.Config{
			Authentication: auth.Authentication{Schemes: []auth.Scheme{{Extract: auth.Bearer()}}},
			Backend:        auth.StaticBackend(loginBackend{}),
		}),
		nexus.AsRestHandler("POST", "/auth/login",
			func(m *auth.Manager, svc *tokenSvc) httpx.HandlerFunc {
				return auth.LoginHandler(m, func(_ context.Context, id *auth.Identity) (any, error) {
					return map[string]any{"access_token": svc.issue(id.ID)}, nil
				})
			}, nexus.Public()),
	)
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	defer stop(context.Background())

	rec, body := postJSON(t, app, "/auth/login", `{"username":"alice","password":"s3cret"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if body["access_token"] != "tok:user-1" {
		t.Errorf("DI issuer body = %v, want access_token tok:user-1", body)
	}
}

func TestLoginEndpoint_BadBody(t *testing.T) {
	app, stop := newLoginApp(t)
	defer stop(context.Background())

	rec, _ := postJSON(t, app, "/auth/login", `not-json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
