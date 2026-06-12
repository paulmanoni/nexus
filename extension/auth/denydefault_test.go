package auth_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/auth"
)

func okHandler(ctx context.Context) (map[string]string, error) {
	return map[string]string{"ok": "yes"}, nil
}

type gqlPing struct {
	Message string `graphql:"message"`
}

// NewGqlPing mounts as the GraphQL query field "gqlPing".
func NewGqlPing(ctx context.Context) (*gqlPing, error) {
	return &gqlPing{Message: "pong"}, nil
}

func postGraphQL(t *testing.T, url, bearer, query string) string {
	t.Helper()
	body := []byte(`{"query":` + jsonString(query) + `}`)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// TestDenyByDefault_GatesGraphQLFields proves deny-by-default reaches
// GraphQL fields too (not just REST): an ungated query field rejects an
// anonymous request and resolves once an identity is present.
func TestDenyByDefault_GatesGraphQLFields(t *testing.T) {
	resolver := func(ctx context.Context, tok string) (*auth.Identity, error) {
		return &auth.Identity{ID: tok}, nil
	}
	addr := "127.0.0.1:8780"
	_ = bootWithManager(t, addr, auth.Config{
		Authentication: auth.Authentication{Schemes: []auth.Scheme{{Resolve: resolver}}},
		Authorization:  auth.Authorization{Default: auth.Authenticated()},
	},
		nexus.AsQuery(NewGqlPing),
	)

	url := "http://" + addr + "/graphql"
	q := `{ gqlPing { message } }`

	// Anonymous → the field is gated; the errors array carries the
	// unauthenticated sentinel and the data is null.
	anon := postGraphQL(t, url, "", q)
	if !strings.Contains(anon, "unauthenticated") {
		t.Fatalf("anonymous GraphQL query should be rejected, got: %s", anon)
	}
	if strings.Contains(anon, "pong") {
		t.Fatalf("anonymous GraphQL query should not resolve, got: %s", anon)
	}

	// Authenticated → resolves.
	authed := postGraphQL(t, url, "tok", q)
	if !strings.Contains(authed, "pong") {
		t.Fatalf("authenticated GraphQL query should resolve, got: %s", authed)
	}
}

// TestDenyByDefault_GatesUngatedEndpoints pins the deny-by-default policy:
// with Authorization.Default = Authenticated(), an endpoint that declares no
// explicit gate still requires an identity, while nexus.Public() and
// AuthRoute("login") endpoints stay open.
func TestDenyByDefault_GatesUngatedEndpoints(t *testing.T) {
	resolver := func(ctx context.Context, tok string) (*auth.Identity, error) {
		return &auth.Identity{ID: tok}, nil
	}
	addr := "127.0.0.1:8782"
	_ = bootWithManager(t, addr, auth.Config{
		Authentication: auth.Authentication{Schemes: []auth.Scheme{{Resolve: resolver}}},
		Authorization:  auth.Authorization{Default: auth.Authenticated()},
	},
		// No explicit gate — deny-by-default must require an identity.
		nexus.AsRest("GET", "/private", okHandler),
		// Explicit opt-out — open to anyone.
		nexus.AsRest("GET", "/open", okHandler, nexus.Public()),
		// Login flow — auto-exempt (you can't require auth to log in).
		nexus.AsRest("GET", "/login", okHandler, nexus.AuthRoute("login")),
	)

	// Ungated endpoint: 401 without a token, 200 with one.
	if _, status := httpGet(t, "http://"+addr+"/private", ""); status != 401 {
		t.Fatalf("ungated, no token: status=%d, want 401", status)
	}
	if _, status := httpGet(t, "http://"+addr+"/private", "tok"); status != 200 {
		t.Fatalf("ungated, valid token: status=%d, want 200", status)
	}

	// Public endpoint: open without a token.
	if _, status := httpGet(t, "http://"+addr+"/open", ""); status != 200 {
		t.Fatalf("public, no token: status=%d, want 200", status)
	}

	// Login flow: auto-exempt, open without a token.
	if _, status := httpGet(t, "http://"+addr+"/login", ""); status != 200 {
		t.Fatalf("login flow, no token: status=%d, want 200", status)
	}
}

// TestPermitByDefault_LeavesEndpointsOpen guards that the zero Decision
// (Permit) keeps the opt-in model — an ungated endpoint stays reachable.
func TestPermitByDefault_LeavesEndpointsOpen(t *testing.T) {
	resolver := func(ctx context.Context, tok string) (*auth.Identity, error) {
		return &auth.Identity{ID: tok}, nil
	}
	addr := "127.0.0.1:8781"
	_ = bootWithManager(t, addr, auth.Config{
		Authentication: auth.Authentication{Schemes: []auth.Scheme{{Resolve: resolver}}},
		// No Default set → Permit → ungated endpoints stay open.
	},
		nexus.AsRest("GET", "/open", okHandler),
	)

	if _, status := httpGet(t, "http://"+addr+"/open", ""); status != 200 {
		t.Fatalf("permit-by-default, ungated, no token: status=%d, want 200", status)
	}
}
