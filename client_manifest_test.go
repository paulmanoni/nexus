package nexus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/paulmanoni/nexus/client"
	"github.com/paulmanoni/nexus/registry"
)

// pet is a named struct type — the schema walker should put it in
// the manifest's Refs pool exactly once even though both List and
// Create reference it in their args / return shapes.
type pet struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Age  int    `json:"age,omitempty"`
}

type listPetsArgs struct {
	Limit int `query:"limit"`
}

func newListPets() func(ctx context.Context, args listPetsArgs) ([]pet, error) {
	return func(ctx context.Context, args listPetsArgs) ([]pet, error) { return nil, nil }
}

func newCreatePet() func(ctx context.Context, p pet) (pet, error) {
	return func(ctx context.Context, p pet) (pet, error) { return p, nil }
}

type loginArgs struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResp struct {
	Token string `json:"token"`
}

func newLogin() func(ctx context.Context, args loginArgs) (loginResp, error) {
	return func(ctx context.Context, args loginArgs) (loginResp, error) {
		return loginResp{Token: "fake"}, nil
	}
}

// TestClientManifest_RESTEndpointsCarrySchema is the contract test
// for the SDK manifest path: the schema walker populates ArgsSchema
// and ReturnSchema on every REST endpoint, named struct types
// collect into the shared Refs pool, and the AuthRoute marker
// surfaces as EndpointInfo.AuthFlow.
//
// Auth.Module integration (the manifest's Auth section) is covered
// in a sibling test inside the auth package, where nexus → auth
// is the legal direction (this package can't import auth without
// creating a cycle).
func TestClientManifest_RESTEndpointsCarrySchema(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		Module("pets",
			AsRest("GET", "/pets", newListPets()),
			AsRest("POST", "/pets", newCreatePet()),
			AsRest("POST", "/login", newLogin(), AuthRoute("login")),
		).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	h := client.Mount(app.Engine(), app.Registry(), nil, app.SchemaRefs, "", client.Config{Enabled: true})
	m := h.Manifest()

	type epKey struct{ method, path string }
	user := map[epKey]client.EndpointInfo{}
	for _, e := range m.Endpoints {
		if strings.HasPrefix(e.Path, "/__nexus/") {
			continue
		}
		user[epKey{e.Method, e.Path}] = e
	}
	if got, want := len(user), 3; got != want {
		t.Fatalf("user endpoints: got %d, want %d (had %+v)", got, want, user)
	}

	// List: return is []pet → array of ref pet
	list := user[epKey{"GET", "/pets"}]
	if list.Return == nil || list.Return.Kind != "array" {
		t.Errorf("list return: want array, got %+v", list.Return)
	}
	if list.Return == nil || list.Return.Of == nil || list.Return.Of.Kind != "ref" || list.Return.Of.Ref != "pet" {
		t.Errorf("list return element: want ref pet, got %+v", list.Return)
	}

	// Create: args is ref pet
	create := user[epKey{"POST", "/pets"}]
	if create.Args == nil || create.Args.Kind != "ref" || create.Args.Ref != "pet" {
		t.Errorf("create args: want ref pet, got %+v", create.Args)
	}

	// Login: AuthFlow tag transferred from option to manifest
	login := user[epKey{"POST", "/login"}]
	if login.AuthFlow != "login" {
		t.Errorf("login.AuthFlow: got %q, want %q", login.AuthFlow, "login")
	}

	// pet appears in Refs once with three fields (json-tagged).
	ref, ok := m.Refs["pet"]
	if !ok {
		t.Fatalf("Refs missing pet; have %v", refKeys(m.Refs))
	}
	if got, want := len(ref.Fields), 3; got != want {
		t.Errorf("pet fields: got %d, want %d", got, want)
	}
}

// TestClientManifest_HTTPRouteServesJSON pins the route shape:
// GET <path>/manifest.json returns 200 + application/json + a
// well-formed Manifest struct, plus the static client.js / .d.ts /
// vue.js routes respond.
func TestClientManifest_HTTPRouteServesJSON(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		Module("pets",
			AsRest("GET", "/pets", newListPets()),
		).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	client.Mount(app.Engine(), app.Registry(), nil, app.SchemaRefs, "",
		client.Config{Enabled: true, Path: "/__nexus/client"})

	ts := httptest.NewServer(app)
	defer ts.Close()

	r1, err := http.Get(ts.URL + "/__nexus/client/manifest.json")
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	defer r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", r1.StatusCode)
	}
	if ct := r1.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json prefix", ct)
	}
	var m client.Manifest
	if err := json.NewDecoder(r1.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Version == "" {
		t.Error("Manifest.Version empty")
	}
	if len(m.Endpoints) == 0 {
		t.Error("Manifest.Endpoints empty")
	}

	// Cache: second request returns the same Version + same endpoint
	// count — sync.Once guarantees identical cached payloads.
	r2, _ := http.Get(ts.URL + "/__nexus/client/manifest.json")
	defer r2.Body.Close()
	var m2 client.Manifest
	_ = json.NewDecoder(r2.Body).Decode(&m2)
	if m.Version != m2.Version || len(m.Endpoints) != len(m2.Endpoints) {
		t.Errorf("cached manifest changed across requests: %v vs %v", m, m2)
	}

	// client.js / client.d.ts / vue.js routes also respond.
	for _, path := range []string{"/__nexus/client/client.js", "/__nexus/client/client.d.ts", "/__nexus/client/vue.js"} {
		rr, err := http.Get(ts.URL + path)
		if err != nil || rr.StatusCode != http.StatusOK {
			t.Errorf("GET %s: err=%v status=%d", path, err, statusOf(rr))
			if rr != nil {
				rr.Body.Close()
			}
			continue
		}
		rr.Body.Close()
	}
}

func refKeys(m map[string]registry.NamedType) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func statusOf(r *http.Response) int {
	if r == nil {
		return 0
	}
	return r.StatusCode
}
