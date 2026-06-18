package gql

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/graphql-go/graphql"
	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/httpx/stdrouter"

	"github.com/paulmanoni/nexus/registry"
)

// TestCachedHandler_HitProducesSameResult drives two identical queries
// through Mount and asserts (a) both succeed with the same body and
// (b) the cache shows one miss + one hit. This is the integration
// contract: a cache hit must produce a byte-identical envelope to a
// miss, so callers can't tell whether parse+validate ran.
func TestCachedHandler_HitProducesSameResult(t *testing.T) {

	schema := buildEchoSchema(t)
	cache := NewDocumentCache(16)

	e := stdrouter.New()
	Mount(e, registry.New(), nil, "test", "/graphql", &schema,
		func(o *Options) { o.DocumentCache = cache },
	)

	body := []byte(`{"query":"{ echo(q:\"hi\") }"}`)

	first := doPost(t, e, "/graphql", body)
	second := doPost(t, e, "/graphql", body)

	if !bytes.Equal(first, second) {
		t.Fatalf("hit envelope diverged from miss\nfirst:  %s\nsecond: %s", first, second)
	}

	stats := cache.Stats()
	if stats.Hits != 1 {
		t.Errorf("hits = %d, want 1", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Errorf("misses = %d, want 1", stats.Misses)
	}
	if stats.Size != 1 {
		t.Errorf("size = %d, want 1", stats.Size)
	}
}

// TestCachedHandler_InvalidQueryCachedAsErrors covers the negative
// path: a syntactically valid but semantically invalid query (e.g.
// references a non-existent field) should be cached as "invalid"
// so the validation walker doesn't re-run on every repeat.
func TestCachedHandler_InvalidQueryCachedAsErrors(t *testing.T) {

	schema := buildEchoSchema(t)
	cache := NewDocumentCache(16)

	e := stdrouter.New()
	Mount(e, registry.New(), nil, "test", "/graphql", &schema,
		func(o *Options) { o.DocumentCache = cache },
	)

	body := []byte(`{"query":"{ nope }"}`)
	first := doPost(t, e, "/graphql", body)
	second := doPost(t, e, "/graphql", body)

	if !bytes.Equal(first, second) {
		t.Fatalf("invalid-query responses diverged\nfirst:  %s\nsecond: %s", first, second)
	}

	// Both responses should carry errors.
	var env struct {
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(first, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Errors) == 0 {
		t.Fatalf("expected validation errors, got: %s", first)
	}

	stats := cache.Stats()
	if stats.Hits != 1 || stats.Misses != 1 {
		t.Fatalf("expected 1 miss + 1 hit on invalid query, got %+v", stats)
	}
}

// --- helpers ---

func buildEchoSchema(t *testing.T) graphql.Schema {
	t.Helper()
	q := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"echo": &graphql.Field{
				Type: graphql.String,
				Args: graphql.FieldConfigArgument{
					"q": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (any, error) {
					return p.Args["q"], nil
				},
			},
		},
	})
	s, err := graphql.NewSchema(graphql.SchemaConfig{Query: q})
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	return s
}

func doPost(t *testing.T, e httpx.Router, path string, body []byte) []byte {
	t.Helper()
	req, _ := http.NewRequest("POST", path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}
