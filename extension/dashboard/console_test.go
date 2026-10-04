package dashboard

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2/extension/metrics"
	"github.com/paulmanoni/nexus/v2/httpx/stdrouter"
	"github.com/paulmanoni/nexus/v2/registry"
	"github.com/paulmanoni/nexus/v2/trace"
)

// consoleFixture mounts the dashboard over a small registry: one service
// with a REST and a GraphQL endpoint (the REST one's args a ref into the
// schema pool), a failing worker, recorded traffic, and one trace.
func consoleFixture(t *testing.T) *stdrouter.Router {
	t.Helper()
	reg := registry.New()
	reg.RegisterService(registry.Service{Name: "pets", Description: "Pet catalogue"})
	reg.RegisterEndpoint(registry.Endpoint{Service: "pets", Name: "GET /pets/:id", Transport: registry.REST, Method: "GET", Path: "/pets/:id"})
	reg.RegisterEndpoint(registry.Endpoint{Service: "pets", Name: "searchPets", Transport: registry.GraphQL, Method: "query", Path: "/graphql",
		Args: []registry.GraphQLArg{{Name: "query", Type: "String!"}}})
	reg.SetEndpointSchema("pets", "GET /pets/:id", &registry.TypeRef{Kind: "ref", Ref: "GetArgs"}, nil)
	reg.RegisterWorker(registry.Worker{Name: "indexer", Status: "failed", LastError: "segment corrupted"})

	ms := metrics.NewMemoryStore()
	ms.Record("pets.GET /pets/:id", "127.0.0.1", nil)
	ms.Record("pets.GET /pets/:id", "127.0.0.1", errString("pet 13 is cursed"))

	bus := trace.NewBus(64)
	publishTrace(bus, "tid-console")

	e := stdrouter.New()
	Mount(e, reg, bus, nil, nil, ms, nil, nil, Config{
		Name: "Petshop",
		SchemaRefs: func() map[string]registry.NamedType {
			return map[string]registry.NamedType{"GetArgs": {Fields: []registry.FieldSchema{
				{Name: "ID", JSONName: "id", Path: "id", Type: registry.TypeRef{Kind: "primitive", Primitive: "string"}},
			}}}
		},
	})
	return e
}

type errString string

func (e errString) Error() string { return string(e) }

func get(e *stdrouter.Router, path string, partial bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	if partial {
		r.Header.Set(partialHeader, "1")
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	return w
}

func TestConsolePagesRender(t *testing.T) {
	e := consoleFixture(t)
	for path, want := range map[string][]string{
		Prefix + "/":                      {"Architecture · Petshop", `data-static`},
		Prefix + "/ui/endpoints":          {"/pets/:id", "searchPets", "QUERY"},
		Prefix + "/ui/services":           {"Pet catalogue"},
		Prefix + "/ui/resources":          {"No resources"},
		Prefix + "/ui/jobs":               {"indexer", "segment corrupted", "failed"},
		Prefix + "/ui/runtime":            {"Global middleware"},
		Prefix + "/ui/traces":             {"GET /x", "/__nexus/ui/traces/tid-console"},
		Prefix + "/ui/traces/tid-console": {"db.query", "tid-console"},
		Prefix + "/ui/endpoints/pets/GET%20%2Fpets%2F:id": {
			"pet 13 is cursed", // recent errors come from the store's error ring
			">id<",             // the input table resolved the GetArgs ref
			"data-tester",
		},
	} {
		w := get(e, path, false)
		if w.Code != 200 {
			t.Errorf("%s: status %d", path, w.Code)
			continue
		}
		body := w.Body.String()
		if !strings.HasPrefix(body, "<!doctype html>") {
			t.Errorf("%s: not a full document", path)
		}
		for _, s := range want {
			if !strings.Contains(body, s) {
				t.Errorf("%s: missing %q", path, s)
			}
		}
	}
}

func TestConsolePartialIsLiveRegionsOnly(t *testing.T) {
	e := consoleFixture(t)
	w := get(e, Prefix+"/ui/endpoints", true)
	body := w.Body.String()
	if strings.Contains(body, "<html") || strings.Contains(body, "data-theme-toggle") {
		t.Fatalf("partial carried the document shell: %.200s", body)
	}
	for _, id := range []string{`id="nx-status"`, `id="nx-main"`} {
		if !strings.Contains(body, id) {
			t.Errorf("partial missing %s", id)
		}
	}
}

func TestConsoleUnknownIs404(t *testing.T) {
	e := consoleFixture(t)
	for _, path := range []string{
		Prefix + "/ui/endpoints/pets/nope",
		Prefix + "/ui/traces/unknown",
		Prefix + "/ui/auth", // auth not wired
	} {
		if w := get(e, path, false); w.Code != 404 {
			t.Errorf("%s: want 404, got %d", path, w.Code)
		}
	}
}

func TestConsoleAssets(t *testing.T) {
	e := consoleFixture(t)
	for path, ct := range map[string]string{
		Prefix + "/ui/assets/console.css": "text/css",
		Prefix + "/ui/assets/console.js":  "text/javascript",
	} {
		w := get(e, path, false)
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), ct) || w.Body.Len() == 0 {
			t.Errorf("%s: %d %q (%d bytes)", path, w.Code, w.Header().Get("Content-Type"), w.Body.Len())
		}
	}
	if w := get(e, Prefix+"/ui/assets/missing.js", false); w.Code != 404 {
		t.Errorf("missing asset: want 404, got %d", w.Code)
	}
}

func TestGraphQLTemplate(t *testing.T) {
	st := &consoleState{Refs: map[string]registry.NamedType{
		"Pet": {Fields: []registry.FieldSchema{
			{JSONName: "id", GraphQLName: "id", Type: registry.TypeRef{Kind: "primitive", Primitive: "string"}},
			{JSONName: "owner", GraphQLName: "owner", Type: registry.TypeRef{Kind: "ref", Ref: "User"}},
		}},
	}}
	got := st.graphqlTemplate(registry.Endpoint{Name: "searchPets", Method: "query",
		Args:         []registry.GraphQLArg{{Name: "query", Type: "String!"}},
		ReturnSchema: &registry.TypeRef{Kind: "array", Of: &registry.TypeRef{Kind: "ref", Ref: "Pet"}}})
	want := "query SearchPets($query: String!) {\n  searchPets(query: $query) { id }\n}\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

// bigFixture registers n endpoints across services, as a large app does.
func bigFixture(t *testing.T, n int) *stdrouter.Router {
	t.Helper()
	reg := registry.New()
	for i := 0; i < n; i++ {
		svc := "svc-" + itoa(i%40)
		reg.RegisterService(registry.Service{Name: svc})
		reg.RegisterEndpoint(registry.Endpoint{Service: svc, Name: "GET /op" + itoa(i), Transport: registry.REST, Method: "GET", Path: "/op" + itoa(i)})
	}
	e := stdrouter.New()
	Mount(e, reg, nil, nil, nil, metrics.NewMemoryStore(), nil, nil, Config{Name: "Big"})
	return e
}

func TestConsoleListsPageLargeApps(t *testing.T) {
	e := bigFixture(t, 1000)
	body := get(e, Prefix+"/ui/endpoints", false).Body.String()
	if n := strings.Count(body, `data-href="/__nexus/ui/endpoints/`); n != pageSize {
		t.Fatalf("rendered %d endpoint rows, want one page (%d)", n, pageSize)
	}
	if !strings.Contains(body, "1–100 of 1000") {
		t.Error("missing pager summary")
	}
	// The rail groups by service: 40 groups, not 1000 rows.
	if n := strings.Count(body, "?g=svc-"); n != 40 {
		t.Errorf("rail has %d groups, want 40", n)
	}
	// Search and group scoping happen on the server.
	scoped := get(e, Prefix+"/ui/endpoints?g=svc-3&q=op4", false).Body.String()
	for _, want := range []string{">/op43<", ">/op403<", ">/op443<"} {
		if !strings.Contains(scoped, want) {
			t.Errorf("scoped search missing %s", want)
		}
	}
	if strings.Contains(scoped, ">/op123<") {
		t.Error("search let a non-matching row through")
	}
	if strings.Contains(scoped, `"/__nexus/ui/endpoints/svc-4/`) {
		t.Error("scoped search leaked another group")
	}
	last := get(e, Prefix+"/ui/endpoints?p=10", false).Body.String()
	if !strings.Contains(last, "901–1000 of 1000") {
		t.Error("last page summary wrong")
	}
}

func TestConsolePartialAnswers304WhenUnchanged(t *testing.T) {
	e := bigFixture(t, 10)
	first := get(e, Prefix+"/ui/services", true)
	tag := first.Header().Get("ETag")
	if first.Code != 200 || tag == "" {
		t.Fatalf("first partial: %d, etag %q", first.Code, tag)
	}
	r := httptest.NewRequest("GET", Prefix+"/ui/services", nil)
	r.Header.Set(partialHeader, "1")
	r.Header.Set("If-None-Match", tag)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	if w.Code != 304 || w.Body.Len() != 0 {
		t.Fatalf("unchanged partial: %d with %d bytes, want an empty 304", w.Code, w.Body.Len())
	}
}

func TestListQueryHref(t *testing.T) {
	q := listQuery{Path: "/x", Q: "pets", Sort: "reqs", Desc: true, Page: 3, Args: map[string]string{"t": "rest"}}
	if got := q.href("p", "4"); got != "/x?p=4&q=pets&sort=-reqs&t=rest" {
		t.Errorf("page link: %s", got)
	}
	// Changing the view goes back to page 1.
	if got := q.href("t", ""); got != "/x?q=pets&sort=-reqs" {
		t.Errorf("scope link: %s", got)
	}
	// Re-clicking the sorted column reverses it; a new numeric column sorts descending.
	if got := q.sortHref("reqs", true); got != "/x?q=pets&sort=reqs&t=rest" {
		t.Errorf("toggle sort: %s", got)
	}
	if got := q.sortHref("errs", true); got != "/x?q=pets&sort=-errs&t=rest" {
		t.Errorf("new sort: %s", got)
	}
}

func TestAuthRejectionsFromTraceBuffer(t *testing.T) {
	bus := trace.NewBus(64)
	bus.Publish(trace.Event{Kind: rejectKind, Status: 401, TraceID: "t-anon", Service: "pets", Endpoint: "POST /pets",
		Error: "missing token", Meta: map[string]any{"reason": "unauthenticated"}})
	bus.Publish(trace.Event{Kind: trace.KindRequestEnd, TraceID: "t-ok", Status: 200})
	bus.Publish(trace.Event{Kind: rejectKind, Status: 403, TraceID: "t-bob", Service: "pets", Endpoint: "adoptPet",
		Error: "forbidden", Meta: map[string]any{"reason": "permission", "identity": "bob"}})

	rows := recentRejects(bus)
	if len(rows) != 2 || rows[0].Identity != "bob" || rows[1].Reason != "unauthenticated" {
		t.Fatalf("rows (newest first, rejects only): %+v", rows)
	}

	var buf strings.Builder
	setup := &authSetup{Default: "signed-in", Cache: "5m0s", LocksKnown: true,
		Schemes: []struct{ Name, Type, Reads string }{{"web", "session", "session cookie"}},
		Areas: []struct {
			Name, Prefix, Login, Home string
			Kinds                     []string
		}{{Name: "admin", Prefix: "/admin", Kinds: []string{"staff"}, Home: "/admin"}},
		Locks: []struct {
			Key, What string
			Until     time.Time
		}{{Key: "a:ana", What: "account ana", Until: time.Now().Add(time.Minute)}}}
	st := &consoleState{Auth: &authSummary{Setup: setup}, Endpoints: []registry.Endpoint{
		{Name: "GET /admin/users", Transport: registry.REST, Method: "GET", Path: "/admin/users", Tags: map[string]string{registry.AuthRequiresTag: "users.view"}},
		{Name: "GET /health", Transport: registry.REST, Method: "GET", Path: "/health", Tags: map[string]string{"auth.public": "true"}},
		{Name: "listPets", Transport: registry.GraphQL, Path: "/graphql", Tags: map[string]string{registry.AuthKindTag: "staff"}},
	}}
	if err := authPage(st, rows, true).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{
		`data-events="auth.reject"`, // re-renders as rejections arrive
		"Recent rejections", ">bob<", "anonymous", ">permission<", "missing token",
		`data-href="/__nexus/ui/traces/t-bob"`, // each row opens its trace
		"Schemes — tried in this order", ">session cookie<",
		"area admin (staff) · requires users.view", // the area's kinds, then the endpoint's own gate
		"signed in · kind staff",
		">public<",                             // Public endpoints flagged
		"account ana", "a:ana", "/auth/unlock", // a lock, with its unlock button
		"/auth/revoke-user",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("auth page missing %q", want)
		}
	}
}
