package gql

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/graphql-go/graphql"

	"github.com/paulmanoni/nexus/registry"
)

func init() { gin.SetMode(gin.TestMode) }

// schemaWithMiddleware builds a minimal one-field schema whose
// resolver runs `mw` (an inline middleware-shaped function) before
// returning a constant. Lets each test plug a custom middleware
// without copy-pasting schema boilerplate.
func schemaWithMiddleware(mw func(p graphql.ResolveParams) (any, error)) *graphql.Schema {
	rootQuery := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"hello": &graphql.Field{
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (any, error) {
					if v, err := mw(p); err != nil || v != nil {
						return v, err
					}
					return "world", nil
				},
			},
		},
	})
	s, _ := graphql.NewSchema(graphql.SchemaConfig{Query: rootQuery})
	return &s
}

func postQuery(t *testing.T, e *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/graphql", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(w, req)
	return w
}

// TestSetStatusCode_OverridesDefault verifies a middleware can flip
// the response status from 200 to 401 by calling SetStatusCode in
// the request context. The GraphQL body still contains the error
// in the standard `errors` slice — we don't touch the body.
func TestSetStatusCode_OverridesDefault(t *testing.T) {
	schema := schemaWithMiddleware(func(p graphql.ResolveParams) (any, error) {
		SetStatusCode(p.Context, http.StatusUnauthorized)
		return nil, errors.New("unauthorized")
	})
	e := gin.New()
	e.POST("/graphql", simpleHandler(schema))

	w := postQuery(t, e, `{"query":"{ hello }"}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: want 401, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"errors"`) {
		t.Errorf("body should still carry errors slice; got %s", w.Body.String())
	}
}

// TestSetStatusCode_DefaultsTo200 confirms the framework returns
// 200 OK when no middleware overrides — preserving GraphQL-spec
// behavior for resolvers that just return data.
func TestSetStatusCode_DefaultsTo200(t *testing.T) {
	schema := schemaWithMiddleware(func(p graphql.ResolveParams) (any, error) { return nil, nil })
	e := gin.New()
	e.POST("/graphql", simpleHandler(schema))

	w := postQuery(t, e, `{"query":"{ hello }"}`)
	if w.Code != http.StatusOK {
		t.Errorf("status: want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"world"`) {
		t.Errorf("body should contain resolver result; got %s", w.Body.String())
	}
}

// TestSetStatusCode_LastWriteWins documents the "innermost wins"
// semantic: outer middleware sets 401, inner sets 429, response is
// 429. Matches how layered middleware composes — the deeper
// middleware to fire is the more specific decision.
func TestSetStatusCode_LastWriteWins(t *testing.T) {
	schema := schemaWithMiddleware(func(p graphql.ResolveParams) (any, error) {
		SetStatusCode(p.Context, http.StatusUnauthorized)
		SetStatusCode(p.Context, http.StatusTooManyRequests)
		return nil, errors.New("rate limited")
	})
	e := gin.New()
	e.POST("/graphql", simpleHandler(schema))

	w := postQuery(t, e, `{"query":"{ hello }"}`)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status: want 429, got %d", w.Code)
	}
}

// TestSetStatusCode_NoOpWithoutHolder confirms the function silently
// no-ops when ctx didn't pass through the gql adapter — covers
// resolver tests that run a bare graphql.Do call.
func TestSetStatusCode_NoOpWithoutHolder(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("SetStatusCode should not panic without holder; got %v", r)
		}
	}()
	SetStatusCode(context.Background(), http.StatusForbidden)
}

// TestSetStatusCode_ThroughMount_E2E is the integration test: spin up
// a real httptest server via Mount() (the public entry every nexus
// app uses), fire a GraphQL POST, and verify the auth-style middleware
// flips the status from 200 → 401. Covers the full Gin → simpleHandler
// → graphql.Do → SetStatusCode → c.JSON chain.
func TestSetStatusCode_ThroughMount_E2E(t *testing.T) {
	// Schema with a resolver that calls SetStatusCode then returns
	// a GraphQL error — same pattern an auth middleware would use.
	rootQuery := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"secret": &graphql.Field{
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (any, error) {
					SetStatusCode(p.Context, http.StatusUnauthorized)
					return nil, errors.New("unauthorized")
				},
			},
			"public": &graphql.Field{
				Type:    graphql.String,
				Resolve: func(p graphql.ResolveParams) (any, error) { return "ok", nil },
			},
		},
	})
	schema, _ := graphql.NewSchema(graphql.SchemaConfig{Query: rootQuery})

	e := gin.New()
	Mount(e, registry.New(), nil, "graphtest", "/graphql", &schema)
	srv := httptest.NewServer(e)
	defer srv.Close()

	// 1) Override path: secret resolver flips to 401.
	resp, err := http.Post(srv.URL+"/graphql", "application/json",
		bytes.NewBufferString(`{"query":"{ secret }"}`))
	if err != nil {
		t.Fatalf("POST secret: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("secret: want 401, got %d", resp.StatusCode)
	}

	// 2) No-override path: public resolver returns 200.
	resp2, err := http.Post(srv.URL+"/graphql", "application/json",
		bytes.NewBufferString(`{"query":"{ public }"}`))
	if err != nil {
		t.Fatalf("POST public: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("public: want 200, got %d", resp2.StatusCode)
	}
}

// TestSetStatusCode_GoGraphHandler verifies the buffered-replay path
// works — when Playground/DEBUG/UserDetailsFn forces the goGraph
// path, the status override still lands correctly via
// statusCaptureWriter's flush.
func TestSetStatusCode_GoGraphHandler(t *testing.T) {
	rootQuery := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"x": &graphql.Field{
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (any, error) {
					SetStatusCode(p.Context, http.StatusTooManyRequests)
					return nil, errors.New("rate limited")
				},
			},
		},
	})
	schema, _ := graphql.NewSchema(graphql.SchemaConfig{Query: rootQuery})

	e := gin.New()
	// Pretty=true forces the goGraphHandler branch in Mount.
	Mount(e, registry.New(), nil, "graphtest-debug", "/graphql", &schema, WithPretty(true), WithDEBUG(true))
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/graphql", "application/json",
		bytes.NewBufferString(`{"query":"{ x }"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status: want 429, got %d", resp.StatusCode)
	}
}
