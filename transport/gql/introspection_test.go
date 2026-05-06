package gql

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/graphql-go/graphql"
)

// schemaWithDummyField is a minimal schema for the gate tests —
// one resolvable field is enough to confirm legitimate queries
// pass while introspection ones get blocked.
func schemaWithDummyField() *graphql.Schema {
	root := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"hello": &graphql.Field{
				Type:    graphql.String,
				Resolve: func(p graphql.ResolveParams) (any, error) { return "world", nil },
			},
		},
	})
	s, _ := graphql.NewSchema(graphql.SchemaConfig{Query: root})
	return &s
}

// TestHasIntrospectionToken pins the substring detector. The "__"
// prefix is reserved by the GraphQL spec for introspection — user
// types/fields cannot start with it — so a literal substring match
// on the two known names is correct AND has no false positives on
// well-formed user queries.
func TestHasIntrospectionToken(t *testing.T) {
	cases := []struct {
		name string
		q    string
		hit  bool
	}{
		{"plain query", `{ hello }`, false},
		{"named query", `query Greet { hello }`, false},
		{"variables", `query G($x: String) { hello(name: $x) }`, false},
		{"schema query", `{ __schema { types { name } } }`, true},
		{"type query", `{ __type(name: "User") { fields { name } } }`, true},
		{"schema in fragment", `query { ...F } fragment F on Query { __schema { types { name } } }`, true},
		{"empty", "", false},
		// User can't have a field named __anything (spec) but the
		// detector errs on the side of blocking — fine for a
		// security gate.
		{"reserved prefix anywhere", `query { foo __schema_like }`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasIntrospectionToken(c.q); got != c.hit {
				t.Errorf("got %v, want %v", got, c.hit)
			}
		})
	}
}

// installGate wires the gate + simpleHandler the same way Mount
// does — without the registry coupling, so tests stay focused on
// the gate's behavior.
func installGate(allow func(c *gin.Context) bool, schema *graphql.Schema) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	handlers := []gin.HandlerFunc{}
	if allow != nil {
		handlers = append(handlers, productionGate(allow, schema))
	}
	handlers = append(handlers, simpleHandler(schema))
	e.POST("/graphql", handlers...)
	e.GET("/graphql", handlers...)
	return e
}

// TestIntrospectionGate_BlocksPostBody fires a real POST through
// the gate + simpleHandler with AllowIntrospection returning false.
// __schema POST gets 404; legitimate POST passes through.
func TestIntrospectionGate_BlocksPostBody(t *testing.T) {
	e := installGate(func(c *gin.Context) bool { return false }, schemaWithDummyField())

	// Introspection POST → 404 (not 401/403; matches the dashboard
	// gate's "indistinguishable from no route" pattern).
	w := httptest.NewRecorder()
	body := `{"query":"{ __schema { types { name } } }"}`
	req := httptest.NewRequest("POST", "/graphql", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("introspection POST: got %d, want 404", w.Code)
	}

	// Legitimate POST → 200 with resolved value. Confirms the body-
	// restore in introspectionGate works (downstream handler reads
	// the body, must see the original bytes).
	w2 := httptest.NewRecorder()
	body2 := `{"query":"{ hello }"}`
	req2 := httptest.NewRequest("POST", "/graphql", bytes.NewBufferString(body2))
	req2.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("legitimate POST: got %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "world") {
		t.Errorf("resolver didn't run; body=%s", w2.Body.String())
	}
}

// TestIntrospectionGate_BlocksGetQuery confirms the GET form (query
// in URL params, used by Playground bookmarks and curl) is gated
// the same way — different code path, same outcome.
func TestIntrospectionGate_BlocksGetQuery(t *testing.T) {
	e := installGate(func(c *gin.Context) bool { return false }, schemaWithDummyField())

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/graphql?query=%7B+__schema+%7B+types+%7B+name+%7D+%7D+%7D", nil)
	e.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("introspection GET: got %d, want 404", w.Code)
	}
}

// TestIntrospectionGate_AllowsWhenAllowReturnsTrue confirms the
// bypass path: a request from an allowlisted peer (allow returns
// true) reaches the resolver even on an introspection query.
// Keeps the dev/internal experience intact when CIDRs are set.
func TestIntrospectionGate_AllowsWhenAllowReturnsTrue(t *testing.T) {
	e := installGate(func(c *gin.Context) bool { return true }, schemaWithDummyField())

	w := httptest.NewRecorder()
	body := `{"query":"{ __schema { types { name } } }"}`
	req := httptest.NewRequest("POST", "/graphql", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("allowed introspection: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "__schema") && !strings.Contains(w.Body.String(), "types") {
		t.Errorf("resolver didn't run schema introspection; body=%s", w.Body.String())
	}
}

// TestProductionGate_BlocksRuleViolations confirms the gate
// applies the FULL go-graph security suite when the peer is not
// on the allowlist — depth, aliases, complexity, plus
// introspection. Same rule set go-graph runs under DEBUG: false.
//
// Aliases case is the most isolated to reproduce: > 4 aliases on a
// single field violates MaxAliasesLimiter regardless of depth /
// complexity arithmetic.
func TestProductionGate_BlocksRuleViolations(t *testing.T) {
	e := installGate(func(c *gin.Context) bool { return false }, schemaWithDummyField())

	// 5 aliases on a single field — exceeds maxAliases=4.
	body := `{"query":"{ a:hello b:hello c:hello d:hello e:hello }"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/graphql", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("alias-bomb query: got %d, want 404 (security suite must run); body=%s", w.Code, w.Body.String())
	}
}

// TestProductionGate_AllowsLegitimateQueries confirms the suite
// doesn't false-positive on simple, well-formed queries that fit
// well under every limit. Critical regression test: a too-strict
// gate would break the prod flow for normal users.
func TestProductionGate_AllowsLegitimateQueries(t *testing.T) {
	e := installGate(func(c *gin.Context) bool { return false }, schemaWithDummyField())

	body := `{"query":"{ hello }"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/graphql", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("legitimate query was blocked: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestIntrospectionGate_NilOptionIsNoop pins back-compat: callers
// who don't set WithAllowIntrospection see no behavioral change —
// __schema queries resolve normally, same as before v0.30.1.
func TestIntrospectionGate_NilOptionIsNoop(t *testing.T) {
	e := installGate(nil, schemaWithDummyField())

	w := httptest.NewRecorder()
	body := `{"query":"{ __schema { types { name } } }"}`
	req := httptest.NewRequest("POST", "/graphql", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("default mount: got %d, want 200 (no gate without the option)", w.Code)
	}
}
