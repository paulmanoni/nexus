package nexus

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// genIsRow is a stand-in for entities.InterviewSession — a named
// struct with a few scalar fields. The whole point is to confirm
// graphql-go's reflective generator keeps it as a NAMED SDL Object
// type when it's wrapped in a generic envelope.
type genIsRow struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
}

// genResp mimics pkg.Response[T] — a generic envelope. The data
// field's element type is what we care about discovering in the
// SDL.
type genResp[T any] struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

// Top-level handler matching your shape: returns
// *pkg.Response[[]entities.InterviewSession].
func NewListGenIs(_ struct{}) (*genResp[[]genIsRow], error) {
	return &genResp[[]genIsRow]{
		Success: true,
		Data:    []genIsRow{{ID: 1, State: "open"}},
	}, nil
}

// TestGenericEnvelope_IntrospectionExposesInnerNamedType runs a real
// introspection query against an app whose handler returns a
// generic envelope wrapping a slice of a named type. We log:
//
//   - every Object/InputObject type the schema emits (so you can see
//     the envelope's auto-generated SDL name)
//   - the inner type's field list (the LoadField target)
//
// Read the t.Logf output to see what SDL names your real app's
// pkg.Response[...] and entities.InterviewSession map to.
func TestGenericEnvelope_IntrospectionExposesInnerNamedType(t *testing.T) {
	mod := Module("generic_envelope_introspect",
		AsQuery(NewListGenIs, Op("listGenIs")),
	)
	app, err := newApp(Config{
		Server:        ServerConfig{Addr: "127.0.0.1:0"},
		Introspection: true,
	}, mod)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Stop()
	srv := httptest.NewServer(app.Engine())
	defer srv.Close()

	// 1. List every type name the schema knows.
	allTypes := postIntrospect(t, srv.URL, `{ __schema { types { name kind } } }`)
	t.Logf("--- SCHEMA TYPES ---")
	for _, name := range listTypeNames(allTypes) {
		t.Logf("  %s", name)
	}

	// 2. Find the inner type by its Go name (genIsRow → likely
	//    "GenIsRow" or similar after sanitization).
	candidates := []string{"genIsRow", "GenIsRow", "GenIsrow", "Genisrow"}
	for _, cand := range candidates {
		t.Logf("--- fields of %q ---", cand)
		raw := postIntrospectRaw(t, srv.URL, fmt.Sprintf(
			`{ __type(name:%q) { name fields { name type { name kind ofType { name kind } } } } }`,
			cand))
		t.Logf("  %s", raw)
	}

	// 3. Dump every Object type's field list — this is the
	//    definitive answer for "what SDL name does my envelope's
	//    inner type have, and what fields are on it."
	objectTypes := pickObjectTypes(allTypes)
	sort.Strings(objectTypes)
	t.Logf("--- Object-type field shapes ---")
	for _, name := range objectTypes {
		raw := postIntrospectRaw(t, srv.URL, fmt.Sprintf(
			`{ __type(name:%q) { name fields { name } } }`, name))
		t.Logf("  %s", raw)
	}
}

// --- helpers ---

func postIntrospect(t *testing.T, url, query string) map[string]any {
	t.Helper()
	raw := postIntrospectRaw(t, url, query)
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, raw)
	}
	return env.Data
}

func postIntrospectRaw(t *testing.T, url, query string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"query": query})
	resp, err := http.Post(url+"/graphql", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

func listTypeNames(data map[string]any) []string {
	schema, _ := data["__schema"].(map[string]any)
	types, _ := schema["types"].([]any)
	var names []string
	for _, ti := range types {
		tm, _ := ti.(map[string]any)
		name, _ := tm["name"].(string)
		// Skip the built-in introspection types — noise.
		if strings.HasPrefix(name, "__") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func pickObjectTypes(data map[string]any) []string {
	schema, _ := data["__schema"].(map[string]any)
	types, _ := schema["types"].([]any)
	var names []string
	for _, ti := range types {
		tm, _ := ti.(map[string]any)
		kind, _ := tm["kind"].(string)
		name, _ := tm["name"].(string)
		if kind == "OBJECT" && !strings.HasPrefix(name, "__") {
			names = append(names, name)
		}
	}
	return names
}
