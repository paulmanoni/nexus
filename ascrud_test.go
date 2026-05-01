package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/resource"
)

type AscrudNote struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// notesDB stands in for the user's DB wrapper (the kind of thing
// `OAtsDB` would be in a real project). Implements
// NexusResourceProvider so AsCRUD's resource auto-attach picks it up,
// and carries a parallel store so the resolver has something live to
// hand back to handlers.
type notesDB struct {
	store *MemoryStore[AscrudNote]
}

func newFakeDB() *notesDB {
	getID := func(n *AscrudNote) string { return n.ID }
	setID := func(n *AscrudNote, id string) { n.ID = id }
	return &notesDB{store: NewMemoryStore[AscrudNote](getID, setID, nil)}
}

func (f *notesDB) NexusResources() []resource.Resource {
	return []resource.Resource{
		resource.NewDatabase("notes-fakedb", "Fake DB", nil, func() bool { return true }, resource.AsDefault()),
	}
}

// TestAsCRUD_RestEndpoints exercises the full Create → Read → List →
// Update → Delete cycle through the generated REST surface, asserting
// that the framework's reflective handler-binding + AsRest mount path
// + MemoryStore semantics line up. Uses httptest against the live
// gin engine to catch any divergence between AsCRUD's handler shapes
// and what AsRest's binder accepts.
func TestAsCRUD_RestEndpoints(t *testing.T) {
	mod := Module("ascrud_rest",
		Provide(func(app *App) *Service { return app.Service("notes-test") }),
		AsCRUD[AscrudNote](MemoryResolver[AscrudNote](nil, nil)),
	)
	app, err := newApp(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}, mod)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Stop()

	srv := httptest.NewServer(app.Engine())
	defer srv.Close()

	// Create
	created := postJSON(t, srv.URL+"/ascrudnotes", `{"title":"first","body":"hello"}`)
	if created["id"] == "" || created["title"] != "first" {
		t.Fatalf("create returned %#v", created)
	}
	id := created["id"]

	// Read
	read := getJSON(t, srv.URL+"/ascrudnotes/"+id)
	if read["id"] != id || read["title"] != "first" {
		t.Fatalf("read mismatch: %#v", read)
	}

	// List
	listResp := getRaw(t, srv.URL+"/ascrudnotes")
	if !strings.Contains(listResp, `"total":1`) || !strings.Contains(listResp, `"first"`) {
		t.Fatalf("list missing expected fields: %s", listResp)
	}

	// Update — only Title in patch; Body must survive because
	// mergePatch skips zero-value fields.
	patched := patchJSON(t, srv.URL+"/ascrudnotes/"+id, `{"title":"renamed"}`)
	if patched["title"] != "renamed" {
		t.Fatalf("update title: %#v", patched)
	}
	if patched["body"] != "hello" {
		t.Fatalf("update wiped body — mergePatch should preserve unset fields: %#v", patched)
	}

	// Delete
	deleteReq(t, srv.URL+"/ascrudnotes/"+id)
	resp, err := http.Get(srv.URL + "/ascrudnotes/" + id)
	if err != nil {
		t.Fatalf("get-after-delete: %v", err)
	}
	resp.Body.Close()
	// ErrCRUDNotFound must surface as 404, not the generic 500 the
	// AsRest error path used to give. Guards the sentinel-mapping
	// wired into AsRest via MapCRUDError.
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get-after-delete: status=%d, want 404", resp.StatusCode)
	}
}

// TestAsCRUD_FxInjectedResolverAttachesResources covers the design's
// headline feature: the resolver can declare fx deps after ctx, and
// any dep implementing NexusResourceProvider auto-attaches its
// resources to every endpoint AsCRUD generates. No explicit Bind
// option, no second type param — just put the dep in the resolver
// signature.
func TestAsCRUD_FxInjectedResolverAttachesResources(t *testing.T) {
	mod := Module("ascrud_resource",
		Provide(func(app *App) *Service { return app.Service("notes-resource") }),
		Provide(newFakeDB),
		AsCRUD[AscrudNote](
			func(ctx context.Context, db *notesDB) (Store[AscrudNote], error) {
				return db.store, nil
			},
			WithGraphQL(),
		),
	)
	app, err := newApp(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}, mod)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Stop()

	// (1) Endpoints must be live: do a quick CRUD round-trip so we
	// know the holder.fn was populated by the setup invoke and the
	// resolver-with-deps path actually carries the *notesDB through.
	srv := httptest.NewServer(app.Engine())
	defer srv.Close()
	created := postJSON(t, srv.URL+"/ascrudnotes", `{"title":"hello"}`)
	if created["id"] == "" {
		t.Fatalf("create did not return id: %#v", created)
	}

	// (2) The fakeDB's resource must be registered.
	resources := app.Registry().Resources()
	var found bool
	for _, r := range resources {
		if r.Name == "notes-fakedb" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("notes-fakedb resource not registered; got %d resources", len(resources))
	}

	// (3) Each generated CRUD endpoint must list "notes-fakedb"
	// among its attached resources, so the dashboard can draw the
	// resource→endpoint edges.
	eps := app.Registry().Endpoints()
	if len(eps) == 0 {
		t.Fatalf("no endpoints recorded")
	}
	for _, ep := range eps {
		hasIt := false
		for _, r := range ep.Resources {
			if r == "notes-fakedb" {
				hasIt = true
				break
			}
		}
		if !hasIt {
			t.Errorf("%s %s (%s): resources=%v, want it to include notes-fakedb",
				ep.Method, ep.Path, ep.Name, ep.Resources)
		}
	}
}

// TestAsCRUD_TagsEndpointsWithModule guards the optionGroup → Module
// annotator wiring: AsCRUD bundles many child registrations behind a
// single Option, but Module() only annotates direct children. The
// wrapper has to forward setModule/setDeployment/setRestPrefix to the
// inner Options, otherwise endpoints land in the registry with an
// empty Module field and the dashboard renders them as bare service
// ops outside any module card.
func TestAsCRUD_TagsEndpointsWithModule(t *testing.T) {
	mod := Module("ascrud_tagged",
		DeployAs("notes-svc"),
		Provide(func(app *App) *Service { return app.Service("notes-tagged") }),
		AsCRUD[AscrudNote](MemoryResolver[AscrudNote](nil, nil), WithGraphQL()),
	)
	app, err := newApp(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}, mod)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Stop()

	eps := app.Registry().Endpoints()
	if len(eps) == 0 {
		t.Fatalf("no endpoints recorded")
	}
	for _, e := range eps {
		if e.Module != "ascrud_tagged" {
			t.Errorf("%s %s: Module=%q, want %q", e.Method, e.Path, e.Module, "ascrud_tagged")
		}
		if e.Deployment != "notes-svc" {
			t.Errorf("%s %s: Deployment=%q, want %q", e.Method, e.Path, e.Deployment, "notes-svc")
		}
	}
}

// TestAsCRUD_DefaultIsRestOnly verifies that without WithGraphQL the
// generator does not mount any GraphQL fields — keeping the SDL clean
// for projects that intentionally want a single transport.
func TestAsCRUD_DefaultIsRestOnly(t *testing.T) {
	mod := Module("ascrud_default",
		Provide(func(app *App) *Service { return app.Service("notes-default") }),
		AsCRUD[AscrudNote](MemoryResolver[AscrudNote](nil, nil)),
	)
	app, err := newApp(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}, mod)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Stop()

	for _, r := range app.Engine().Routes() {
		if r.Path == "/graphql" || strings.HasSuffix(r.Path, "/graphql") {
			t.Fatalf("default AsCRUD should not mount GraphQL — saw %s %s", r.Method, r.Path)
		}
	}
}

// TestAsCRUD_GraphQL drives the same five operations through the
// GraphQL transport, confirming the generated ops mount under the
// module's service and the input/output type derivation works for
// both flat scalars and the Page<T> envelope.
func TestAsCRUD_GraphQL(t *testing.T) {
	mod := Module("ascrud_gql",
		Provide(func(app *App) *Service { return app.Service("notes-gql") }),
		AsCRUD[AscrudNote](MemoryResolver[AscrudNote](nil, nil), WithGraphQL()),
	)
	app, err := newApp(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}, mod)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Stop()

	srv := httptest.NewServer(app.Engine())
	defer srv.Close()

	gqlURL := srv.URL + "/graphql"

	// Create
	cr := gqlExec(t, gqlURL, `mutation { createAscrudNote(title:"alpha", body:"b1") { id title body } }`)
	createPayload, _ := cr["createAscrudNote"].(map[string]any)
	id, _ := createPayload["id"].(string)
	if id == "" {
		t.Fatalf("createAscrudNote returned no id: %#v", cr)
	}

	// Get
	g := gqlExec(t, gqlURL, `query { getAscrudNote(id:"`+id+`") { id title } }`)
	getPayload, _ := g["getAscrudNote"].(map[string]any)
	if getPayload["title"] != "alpha" {
		t.Fatalf("getAscrudNote mismatch: %#v", g)
	}

	// List — Page envelope's {items,total} surfaces as a typed object.
	l := gqlExec(t, gqlURL, `query { listAscrudnotes { total items { id title } } }`)
	page, _ := l["listAscrudnotes"].(map[string]any)
	if page["total"].(float64) != 1 {
		t.Fatalf("listAscrudnotes total: %#v", page)
	}

	// Update — id rides on the body in the GraphQL shape.
	u := gqlExec(t, gqlURL, `mutation { updateAscrudNote(id:"`+id+`", title:"beta") { title body } }`)
	updatePayload, _ := u["updateAscrudNote"].(map[string]any)
	if updatePayload["title"] != "beta" {
		t.Fatalf("updateAscrudNote did not apply: %#v", u)
	}
	if updatePayload["body"] != "b1" {
		t.Fatalf("updateAscrudNote dropped untouched body: %#v", u)
	}

	// Delete — returns Boolean.
	d := gqlExec(t, gqlURL, `mutation { deleteAscrudNote(id:"`+id+`") }`)
	if d["deleteAscrudNote"] != true {
		t.Fatalf("deleteAscrudNote: %#v", d)
	}
}

// ─── helpers ──────────────────────────────────────────────────────

func postJSON(t *testing.T, url, body string) map[string]string {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s = %d: %s", url, resp.StatusCode, b)
	}
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func getJSON(t *testing.T, url string) map[string]string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, b)
	}
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func getRaw(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func patchJSON(t *testing.T, url, body string) map[string]string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PATCH %s = %d: %s", url, resp.StatusCode, b)
	}
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func deleteReq(t *testing.T, url string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("DELETE %s = %d", url, resp.StatusCode)
	}
}

func gqlExec(t *testing.T, url, q string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"query": q})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gql post: %v", err)
	}
	defer resp.Body.Close()
	var env struct {
		Data   map[string]any `json:"data"`
		Errors []any          `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("gql decode: %v", err)
	}
	if len(env.Errors) > 0 {
		t.Fatalf("gql errors for %q: %v", q, env.Errors)
	}
	return env.Data
}