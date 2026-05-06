package nexus

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

// TestClientManifest_DTSCarriesRefsAndEndpoints pins that the live
// HTTP path through GET /__nexus/client/client.d.ts produces a
// .d.ts file that reflects the running app's registry: the named
// type pet shows up as an interface, REST endpoints land in
// RestEndpoints with proper TS types. Generator unit tests (in
// client/generator_test.go) cover individual renderer arms; this
// test pins the wiring at the HTTP boundary.
func TestClientManifest_DTSCarriesRefsAndEndpoints(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		Module("pets",
			AsRest("GET", "/pets", newListPets()),
			AsRest("POST", "/pets", newCreatePet()),
		).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	client.Mount(app.Engine(), app.Registry(), nil, app.SchemaRefs, "", client.Config{Enabled: true})

	ts := httptest.NewServer(app)
	defer ts.Close()

	r, err := http.Get(ts.URL + "/__nexus/client/client.d.ts")
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("GET client.d.ts: err=%v status=%d", err, statusOf(r))
	}
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	dts := string(body)

	for _, sym := range []string{
		"export interface pet {",
		"id: string",
		"name: string",
		"age?: number",
		"export interface RestEndpoints {",
		"'GET /pets':",
		"'POST /pets':",
		"return: pet[]",
		"args: pet",
		"declare module '/__nexus/client/client.js'",
	} {
		if !strings.Contains(dts, sym) {
			t.Errorf("DTS missing %q\n--- DTS ---\n%s\n--- end ---", sym, dts)
		}
	}
}

// TestClientManifest_VueComposablesExports asserts the served
// vue.js is the real composables module, not the step-3 placeholder.
// Same name-presence approach as the client.js sibling test.
func TestClientManifest_VueComposablesExports(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		Module("pets", AsRest("GET", "/pets", newListPets())).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	client.Mount(app.Engine(), app.Registry(), nil, app.SchemaRefs, "", client.Config{Enabled: true})
	ts := httptest.NewServer(app)
	defer ts.Close()

	r, err := http.Get(ts.URL + "/__nexus/client/vue.js")
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("GET vue.js: err=%v status=%d", err, statusOf(r))
	}
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	bodyStr := string(body)
	for _, sym := range []string{
		"export function useNexus",
		"export function useQuery",
		"export function useMutation",
		"export function useGqlQuery",
		"export function useGqlMutation",
		"export function useCrud",
		"export function useWS",
		"export function useAuth",
		"import { ref, watch, unref, computed, onUnmounted } from 'vue'",
		"import { NexusClient } from './client.js'",
	} {
		if !strings.Contains(bodyStr, sym) {
			t.Errorf("vue.js missing %q — composables regressed to placeholder?", sym)
		}
	}
}

// TestClientManifest_RuntimeJSExports asserts the served client.js
// is the real runtime, not the step-3 placeholder. Pins the public
// surface (NexusClient + AuthNamespace + WSHandle + token stores)
// without parsing JavaScript: the SDK is hand-authored and small
// enough that a name-presence check catches any catastrophic
// regression (file truncated, wrong file embedded) without
// requiring a node toolchain in CI.
func TestClientManifest_RuntimeJSExports(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		Module("pets", AsRest("GET", "/pets", newListPets())).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	client.Mount(app.Engine(), app.Registry(), nil, app.SchemaRefs, "", client.Config{Enabled: true})

	ts := httptest.NewServer(app)
	defer ts.Close()

	r, err := http.Get(ts.URL + "/__nexus/client/client.js")
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("GET client.js: err=%v status=%d", err, statusOf(r))
	}
	defer r.Body.Close()
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("Content-Type: got %q, want application/javascript prefix", ct)
	}
	body, _ := io.ReadAll(r.Body)
	bodyStr := string(body)
	for _, sym := range []string{
		"export class NexusClient",
		"export class NexusError",
		"export function localStorageTokenStore",
		"export function memoryTokenStore",
		"class AuthNamespace",
		"class CrudHandle",
		"class WSHandle",
		"async login",
		"async logout",
		"buildGqlDocument",
	} {
		if !strings.Contains(bodyStr, sym) {
			t.Errorf("client.js missing %q — runtime regressed to placeholder?", sym)
		}
	}
}

// TestClientManifest_AutoMountViaConfig pins the Config.Client.Enabled
// auto-mount path: declaring Client on nexus.Config is sufficient to
// make /__nexus/client/manifest.json resolve. No manual client.Mount
// call needed.
func TestClientManifest_AutoMountViaConfig(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{
			Server: ServerConfig{Addr: "127.0.0.1:0"},
			Client: client.Config{Enabled: true},
		}),
		Module("pets", AsRest("GET", "/pets", newListPets())).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	if app.ClientHandler() == nil {
		t.Fatal("ClientHandler() returned nil — Config.Client.Enabled didn't trigger Mount")
	}

	ts := httptest.NewServer(app)
	defer ts.Close()
	r, err := http.Get(ts.URL + "/__nexus/client/manifest.json")
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("manifest GET: err=%v status=%d", err, statusOf(r))
	}
	r.Body.Close()
}

// TestClientManifest_AutoMountViaOption covers the option-chain
// wiring (ClientUse) — same effect as Config.Client.Enabled but
// composes with IfDeployment / Module wrappers.
func TestClientManifest_AutoMountViaOption(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		Module("pets", AsRest("GET", "/pets", newListPets())).nexusOption(),
		ClientUse(client.Config{}).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	if app.ClientHandler() == nil {
		t.Fatal("ClientHandler() nil after ClientUse")
	}
	ts := httptest.NewServer(app)
	defer ts.Close()
	r, err := http.Get(ts.URL + "/__nexus/client/manifest.json")
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("manifest GET: err=%v status=%d", err, statusOf(r))
	}
	r.Body.Close()
}

// TestClientManifest_DoubleMountIsNoOp pins ClientUse's idempotence:
// when BOTH Config.Client.Enabled AND ClientUse are present, the
// second wiring is a no-op (the first mount wins). Without this
// guard a paranoid user mixing both styles would crash with a
// duplicate-route gin panic.
func TestClientManifest_DoubleMountIsNoOp(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{
			Server: ServerConfig{Addr: "127.0.0.1:0"},
			Client: client.Config{Enabled: true},
		}),
		Module("pets", AsRest("GET", "/pets", newListPets())).nexusOption(),
		ClientUse(client.Config{}).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()
	// If we got here the route table didn't double-register.
	if app.ClientHandler() == nil {
		t.Fatal("ClientHandler() nil after dual wiring")
	}
}

// TestClientManifest_OutDirAutoDump confirms Config.Client.OutDir
// triggers the in-process disk dump on Mount — frontend tooling
// (TS compiler, IDE) picks up types and imports without a manual
// nexus client --out step. Idempotent contract verified by the
// CLI's TestClientCmd_IdempotentSecondRun; this test only pins
// that the auto-dump fires at all and lands the expected files.
func TestClientManifest_OutDirAutoDump(t *testing.T) {
	dir := t.TempDir()
	out := dir + "/sdk"
	tsconfig := dir + "/tsconfig.json"

	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{
			Server: ServerConfig{Addr: "127.0.0.1:0"},
			Client: client.Config{
				Enabled:  true,
				OutDir:   out,
				TSConfig: tsconfig,
			},
		}),
		Module("pets", AsRest("GET", "/pets", newListPets())).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	// All four SDK files should exist immediately after Mount —
	// no HTTP request needed. .d.ts must reflect the registered
	// pet endpoint (manifest was eagerly built).
	for _, name := range []string{"client.js", "vue.js", "manifest.json", "client.d.ts"} {
		path := out + "/" + name
		if info, err := os.Stat(path); err != nil {
			t.Errorf("auto-dump missing %s: %v", path, err)
		} else if info.Size() == 0 {
			t.Errorf("auto-dumped %s is empty", path)
		}
	}

	// tsconfig.json should have the path mappings + baseUrl.
	tsBody, err := os.ReadFile(tsconfig)
	if err != nil {
		t.Fatalf("tsconfig not written: %v", err)
	}
	for _, want := range []string{
		`"baseUrl": "."`,
		`"/__nexus/client/client.js"`,
		`"/__nexus/client/vue.js"`,
		`"sdk/client.js"`,
	} {
		if !strings.Contains(string(tsBody), want) {
			t.Errorf("tsconfig missing %q", want)
		}
	}

	// Manifest content reflects the running app.
	dts, _ := os.ReadFile(out + "/client.d.ts")
	if !strings.Contains(string(dts), "'GET /pets'") {
		t.Errorf("client.d.ts didn't pick up the registered endpoint:\n%s", dts)
	}
}

// TestClientManifest_SetAuthInfo proves the auth-bridge seam: an
// explicit SetClientAuthInfo call (the kind auth.Module's option
// chain will issue) populates Manifest.Auth on the next request.
func TestClientManifest_SetAuthInfo(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{
			Server: ServerConfig{Addr: "127.0.0.1:0"},
			Client: client.Config{Enabled: true},
		}),
		Module("pets", AsRest("GET", "/pets", newListPets())).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	app.SetClientAuthInfo(func() client.ExtractorInfo {
		return client.ExtractorInfo{Strategy: "bearer", HeaderName: "Authorization"}
	})

	m := app.ClientHandler().Manifest()
	if m.Auth == nil {
		t.Fatal("Manifest.Auth nil after SetClientAuthInfo")
	}
	if m.Auth.Strategy != "bearer" {
		t.Errorf("Auth.Strategy: got %q, want %q", m.Auth.Strategy, "bearer")
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
