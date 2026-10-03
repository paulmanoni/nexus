package nexus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/registry"
)

// TestDescribe_CrossTransport proves nexus.Describe sets the registry
// Description on every transport — REST, GraphQL, and WS — from a single
// option value, the way HideFromDashboard and WithIcon already do. This is the
// contract that lets one helper supersede the transport-specific Desc/Description.
func TestDescribe_CrossTransport(t *testing.T) {
	type args struct {
		ID string `path:"id" graphql:"id"`
	}
	newGet := func(p Params[args]) (string, error) { return "ok", nil }
	newSearch := func(p Params[args]) (string, error) { return "ok", nil }
	newSend := func(sess *WSSession, p Params[args]) error { return nil }

	app, err := newApp(config.Runtime{Server: config.Server{Addr: "127.0.0.1:0"}},
		AsRest("GET", "/items/:id", newGet, Describe("Fetch one item")),
		AsQuery(newSearch, Describe("Search items")),
		AsWS("/events", "item.send", newSend, Describe("Send an item event")),
	)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Stop()

	// One endpoint per transport was registered above; index the description
	// each carries by transport and assert Describe reached all three.
	want := map[registry.Transport]string{
		registry.REST:      "Fetch one item",
		registry.GraphQL:   "Search items",
		registry.WebSocket: "Send an item event",
	}
	got := map[registry.Transport]string{}
	for _, e := range app.Registry().Endpoints() {
		if _, tracked := want[e.Transport]; tracked {
			got[e.Transport] = e.Description
		}
	}
	for tr, exp := range want {
		if got[tr] != exp {
			t.Errorf("%s endpoint: Description = %q, want %q", tr, got[tr], exp)
		}
	}
}

// nexus.Envelope: handlers return plain (T, error); the app-supplied wrap
// owns the wire shape, including turning errors into 200/data payloads.

type envResp[T any] struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

func envWrap[T any](v T, err error) (*envResp[T], error) {
	if err != nil {
		return &envResp[T]{Status: false, Message: err.Error()}, nil
	}
	return &envResp[T]{Status: true, Message: "ok", Data: v}, nil
}

type envSvc struct{}

func (s *envSvc) FindUser(ctx context.Context, a probeArgs) (*probeOut, error) {
	if a.Name == "missing" {
		return nil, Err(NotFound, "no such user")
	}
	return &probeOut{Greeting: "found " + a.Name}, nil
}

func TestEnvelopeGraphQL(t *testing.T) {
	app, stop, err := InProcess(config.Runtime{},
		Supply(&envSvc{}),
		AsQuery((*envSvc).FindUser, Envelope(envWrap[*probeOut])),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	got := postGraphQL(t, app, `{ findUser(name: "ada") { status message data { greeting } } }`)
	if !strings.Contains(got, `"status":true`) || !strings.Contains(got, "found ada") {
		t.Fatalf("success = %s", got)
	}

	got = postGraphQL(t, app, `{ findUser(name: "missing") { status message data { greeting } } }`)
	if !strings.Contains(got, `"status":false`) || !strings.Contains(got, "no such user") {
		t.Fatalf("error-in-envelope = %s", got)
	}
	if strings.Contains(got, `"errors"`) {
		t.Fatalf("enveloped error must not reach the GraphQL errors array: %s", got)
	}
}

func TestEnvelopeRest(t *testing.T) {
	app, stop, err := InProcess(config.Runtime{},
		Supply(&envSvc{}),
		AsRest("GET", "/find", (*envSvc).FindUser, Envelope(envWrap[*probeOut])),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/find?name=bo", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "found bo") {
		t.Fatalf("success = %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/find?name=missing", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":false`) || !strings.Contains(w.Body.String(), "no such user") {
		t.Fatalf("enveloped error must be a 200 payload: %d %s", w.Code, w.Body.String())
	}
}

// A wrap whose input type doesn't match the handler's return must fail at
// boot, naming both types.
func TestEnvelopeTypeMismatchFailsBoot(t *testing.T) {
	_, stop, err := InProcess(config.Runtime{},
		Supply(&envSvc{}),
		AsQuery((*envSvc).FindUser, Envelope(envWrap[[]string])),
	)
	if stop != nil {
		defer func() { _ = stop(context.Background()) }()
	}
	if err == nil || !strings.Contains(err.Error(), "Envelope wrap takes") {
		t.Fatalf("expected boot failure naming the mismatch, got %v", err)
	}
}

// TestTag_CrossTransport proves nexus.Tag stamps the same key/value on every
// transport's registry entry — the channel extensions (inertia.Page) use to
// mark endpoints without an option of their own in this package.
func TestTag_CrossTransport(t *testing.T) {
	type args struct {
		ID string `path:"id" graphql:"id"`
	}
	newGet := func(p Params[args]) (string, error) { return "ok", nil }
	newSearch := func(p Params[args]) (string, error) { return "ok", nil }
	newSend := func(sess *WSSession, p Params[args]) error { return nil }

	app, err := newApp(config.Runtime{Server: config.Server{Addr: "127.0.0.1:0"}},
		AsRest("GET", "/items/:id", newGet, Tag("x.kind", "rest"), Tag("", "ignored")),
		AsQuery(newSearch, Tag("x.kind", "gql")),
		AsWS("/events", "item.send", newSend, Tag("x.kind", "ws")),
	)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Stop()

	want := map[registry.Transport]string{
		registry.REST:      "rest",
		registry.GraphQL:   "gql",
		registry.WebSocket: "ws",
	}
	for _, e := range app.Registry().Endpoints() {
		exp, tracked := want[e.Transport]
		if !tracked {
			continue
		}
		if got := e.Tags["x.kind"]; got != exp {
			t.Errorf("%s endpoint: Tags[x.kind] = %q, want %q", e.Transport, got, exp)
		}
		if _, ok := e.Tags[""]; ok {
			t.Errorf("%s endpoint: empty-key Tag must be ignored, got tags %v", e.Transport, e.Tags)
		}
		delete(want, e.Transport)
	}
	if len(want) != 0 {
		t.Errorf("no endpoint registered for transports %v", want)
	}
}

// TestRegisterSharedProp walks a shared prop's type into the registry and the
// shared named-type pool, so a named struct reaches the manifest's refs the
// same way an endpoint schema's would.
func TestRegisterSharedProp(t *testing.T) {
	type Viewer struct {
		Name string `json:"name"`
	}
	app := New(config.Runtime{})
	app.RegisterSharedProp("can", reflect.TypeOf(map[string]bool{}))
	app.RegisterSharedProp("viewer", reflect.TypeOf(&Viewer{}))
	app.RegisterSharedProp("", reflect.TypeOf(""))
	app.RegisterSharedProp("nil", nil)

	got := app.Registry().SharedProps()
	if len(got) != 2 {
		t.Fatalf("SharedProps = %v, want exactly can + viewer", got)
	}
	if c := got["can"]; c.Kind != "map" || c.Of == nil || c.Of.Primitive != "boolean" {
		t.Errorf("can = %+v, want map of boolean", c)
	}
	if v := got["viewer"]; v.Kind != "ref" || v.Ref != "Viewer" || !v.Optional {
		t.Errorf("viewer = %+v, want optional ref Viewer", v)
	}
	if _, ok := app.SchemaRefs()["Viewer"]; !ok {
		t.Errorf("Viewer missing from the schema refs pool: %v", app.SchemaRefs())
	}
}

// Keys an option in this package owns are refused: auth.public / auth.flow
// would exempt a route from the default auth gate behind the back of the
// options that document it.
func TestTag_RefusesFrameworkKeys(t *testing.T) {
	for _, key := range []string{PublicTag, AuthFlowTag, registry.AuthRequiresTag, registry.HiddenTag, registry.IconTag, registry.EnvelopeTag, registry.ProxyTag} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("Tag(%q) did not panic", key)
				}
			}()
			Tag(key, "x")
		}()
	}
	Tag(registry.PageTag, "Users/Index") // an extension's own key is fine
}

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
	app, err := newApp(config.Runtime{
		Server:        config.Server{Addr: "127.0.0.1:0"},
		Introspection: true,
	}, mod)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Stop()
	srv := httptest.NewServer(app.Router())
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
