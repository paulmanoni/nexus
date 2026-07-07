package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/auth"
)

func newLogoutApp(t *testing.T, opts ...auth.LogoutOption) (*nexus.App, func(context.Context) error) {
	t.Helper()
	app, stop, err := nexus.InProcess(nexus.Config{},
		auth.Module(auth.Config{
			Authentication: auth.Authentication{Schemes: []auth.Scheme{{Extract: auth.Bearer()}}},
			Backend:        auth.StaticBackend(loginBackend{}),
		}),
		auth.LogoutEndpoint(opts...),
	)
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	return app, stop
}

func postBearer(t *testing.T, app *nexus.App, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	return rec
}

func TestLogoutEndpoint_RevokesToken(t *testing.T) {
	var revoked string
	app, stop := newLogoutApp(t, auth.WithRevoker(func(_ context.Context, tok string) error {
		revoked = tok
		return nil
	}))
	defer stop(context.Background())

	rec := postBearer(t, app, "/auth/logout", "tok-abc")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if revoked != "tok-abc" {
		t.Errorf("revoker got %q, want tok-abc", revoked)
	}
}

func TestLogoutEndpoint_Idempotent_NoToken(t *testing.T) {
	called := false
	app, stop := newLogoutApp(t, auth.WithRevoker(func(_ context.Context, _ string) error {
		called = true
		return nil
	}))
	defer stop(context.Background())

	// No Authorization header — still 200, revoker not called.
	rec := postBearer(t, app, "/auth/logout", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if called {
		t.Error("revoker should not fire without a token")
	}
}

func TestLogoutEndpoint_CustomPath(t *testing.T) {
	app, stop := newLogoutApp(t, auth.LogoutAt("/signout"))
	defer stop(context.Background())

	if rec := postBearer(t, app, "/signout", "x"); rec.Code != http.StatusOK {
		t.Fatalf("custom path status = %d, want 200", rec.Code)
	}
}
