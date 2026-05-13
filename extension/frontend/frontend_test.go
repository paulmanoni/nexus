package frontend

import (
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/registry"
)

// minimalFS is enough to satisfy nexus.ServeFrontend's "index.html
// exists" boot check when the Plugin's runtime side is exercised.
// Most tests stub the FS but never actually boot Fx, so the
// in-memory fstest map is fine.
func minimalFS() fs.FS {
	return fstest.MapFS{
		"web/dist/index.html": &fstest.MapFile{Data: []byte("<!doctype html><html></html>")},
	}
}

func TestConfig_validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"empty Root", Config{FS: minimalFS()}, "Root is required"},
		{"unknown framework", Config{Root: "web", FS: minimalFS(), Framework: "ember"}, "not recognised"},
		{"Vue ok", Config{Root: "web", FS: minimalFS(), Framework: Vue}, ""},
		{"None ok", Config{Root: "web", FS: minimalFS(), Framework: None}, ""},
		{"no FS ok for codegen-only", Config{Root: "web"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestConfig_validateRuntime(t *testing.T) {
	if err := (Config{Root: "web"}).validateRuntime(); err == nil {
		t.Fatal("validateRuntime accepted nil FS; should require it")
	}
	if err := (Config{Root: "web", FS: minimalFS()}).validateRuntime(); err != nil {
		t.Fatalf("validateRuntime rejected valid runtime config: %v", err)
	}
}

// TestClientConfigFromFrontend_PassesThroughSDKKnobs covers the
// IntelliSense pathway after phase 3: apps that want the legacy
// auto-dump (writes client.js + *.d.ts to disk so IDE URL → file
// resolution works) declare SDKOutDir / SDKTSConfig / SDKViteConfig
// on frontend.Config. mountClientSDK projects those into the
// client.Config it hands to client.Mount; if the mapping silently
// drops a field the user's IDE goes blind without any obvious cause.
func TestClientConfigFromFrontend_PassesThroughSDKKnobs(t *testing.T) {
	got := clientConfigFromFrontend(Config{
		ManifestPublic: true,
		SDKOutDir:      "./web/sdk",
		SDKTSConfig:    "./web/tsconfig.json",
		SDKViteConfig:  "./web/vite.config.ts",
	})
	if !got.Enabled {
		t.Error("Enabled must be true — frontend.Plugin always mounts the SDK")
	}
	if !got.Public {
		t.Error("Public lost: ManifestPublic did not project to client.Config.Public")
	}
	if got.OutDir != "./web/sdk" {
		t.Errorf("OutDir = %q, want ./web/sdk", got.OutDir)
	}
	if got.TSConfig != "./web/tsconfig.json" {
		t.Errorf("TSConfig = %q, want ./web/tsconfig.json", got.TSConfig)
	}
	if got.ViteConfig != "./web/vite.config.ts" {
		t.Errorf("ViteConfig = %q, want ./web/vite.config.ts", got.ViteConfig)
	}
}

// TestClientConfigFromFrontend_ZeroValueStaysZero ensures unset SDK
// knobs don't leak phantom paths into client.Config — that would
// trigger the auto-dump against a directory the user never asked
// to write to. The auto-detection inside client.Mount runs only on
// genuinely empty fields, so we must not pre-fill them here.
func TestClientConfigFromFrontend_ZeroValueStaysZero(t *testing.T) {
	got := clientConfigFromFrontend(Config{Root: "web"})
	if got.OutDir != "" {
		t.Errorf("OutDir = %q, want empty (no SDKOutDir set)", got.OutDir)
	}
	if got.TSConfig != "" {
		t.Errorf("TSConfig = %q, want empty", got.TSConfig)
	}
	if got.ViteConfig != "" {
		t.Errorf("ViteConfig = %q, want empty", got.ViteConfig)
	}
}

// TestConfig_defaults_RuntimeSDKAutoFills covers the auto-default
// pathway: RuntimeSDK:true is the opt-in signal that the user wants
// the IntelliSense bridge wired up. defaults() fills SDKOutDir and
// SDKTSConfig from Root so most apps never type those paths.
func TestConfig_defaults_RuntimeSDKAutoFills(t *testing.T) {
	c := Config{Root: "web", RuntimeSDK: true}.defaults()
	if c.SDKOutDir != "./web/sdk" {
		t.Errorf("SDKOutDir = %q, want ./web/sdk", c.SDKOutDir)
	}
	if c.SDKTSConfig != "./web/tsconfig.json" {
		t.Errorf("SDKTSConfig = %q, want ./web/tsconfig.json", c.SDKTSConfig)
	}
	if c.SDKViteConfig != "" {
		t.Errorf("SDKViteConfig should NOT auto-default — got %q", c.SDKViteConfig)
	}
}

// TestConfig_defaults_RuntimeSDKFalseLeavesEmpty is the inverse: an
// app on the typed codegen path (RuntimeSDK: false) shouldn't have
// the SDK auto-dump fire. The fields stay empty so client.Mount
// skips its dump entirely.
func TestConfig_defaults_RuntimeSDKFalseLeavesEmpty(t *testing.T) {
	c := Config{Root: "web", RuntimeSDK: false}.defaults()
	if c.SDKOutDir != "" {
		t.Errorf("SDKOutDir = %q, want empty (RuntimeSDK off)", c.SDKOutDir)
	}
	if c.SDKTSConfig != "" {
		t.Errorf("SDKTSConfig = %q, want empty", c.SDKTSConfig)
	}
}

// TestConfig_defaults_RespectsExplicitSDKPaths verifies the
// override pathway: a user with a non-standard layout sets the
// fields explicitly, and defaults() must not stomp them.
func TestConfig_defaults_RespectsExplicitSDKPaths(t *testing.T) {
	c := Config{
		Root:        "web",
		RuntimeSDK:  true,
		SDKOutDir:   "./other/path",
		SDKTSConfig: "./other/tsconfig.json",
	}.defaults()
	if c.SDKOutDir != "./other/path" {
		t.Errorf("SDKOutDir = %q, want ./other/path (explicit value lost)", c.SDKOutDir)
	}
	if c.SDKTSConfig != "./other/tsconfig.json" {
		t.Errorf("SDKTSConfig = %q, want explicit override preserved", c.SDKTSConfig)
	}
}

func TestConfig_defaults(t *testing.T) {
	c := Config{Root: "web", FS: minimalFS()}.defaults()
	if c.Output != "dist" {
		t.Errorf("Output default: %q, want dist", c.Output)
	}
	if c.Generate != "src/__nexus" {
		t.Errorf("Generate default: %q, want src/__nexus", c.Generate)
	}
	if c.FSRoot != "web/dist" {
		t.Errorf("FSRoot default: %q, want web/dist", c.FSRoot)
	}
}

// TestRender_TransportNeutralFiles ensures the three core files are
// always emitted, even on an empty registry. The output is the
// contract every framework consumer relies on — if these go missing,
// the user's `import { client } from '@/__nexus'` breaks at type-check.
func TestRender_TransportNeutralFiles(t *testing.T) {
	reg := registry.New()
	ctx := extension.GenerateContext{Registry: reg}
	files, err := Render(Config{Framework: None}, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := fileNames(files)
	want := []string{"_client.ts", "types.ts", "index.ts"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
}

// TestRender_VueAddsVueFile checks the per-framework adapter slot
// activates only when Framework == Vue.
func TestRender_VueAddsVueFile(t *testing.T) {
	reg := registry.New()
	ctx := extension.GenerateContext{Registry: reg}
	files, err := Render(Config{Framework: Vue}, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := fileNames(files)
	want := []string{"_client.ts", "types.ts", "index.ts", "vue.ts"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
}

// TestRender_GraphqlEndpointGeneratesIndexExport boots a fake registry
// with one GraphQL endpoint and asserts the generated index.ts holds a
// matching `export const listUsers`. Per-op typed exports are the
// headline feature of the compile-time-bonded design; a regression
// here would silently fall back to the old string-keyed surface.
func TestRender_GraphqlEndpointGeneratesIndexExport(t *testing.T) {
	reg := registry.New()
	reg.RegisterEndpoint(registry.Endpoint{
		Service:      "users",
		Name:         "listUsers",
		Transport:    registry.GraphQL,
		Method:       "query",
		ReturnSchema: &registry.TypeRef{Kind: "ref", Ref: "User"},
	})
	ctx := extension.GenerateContext{
		Registry: reg,
		Refs: map[string]registry.NamedType{
			"User": {Fields: []registry.FieldSchema{
				{Name: "ID", JSONName: "id", Type: registry.TypeRef{Kind: "primitive", Primitive: "string"}},
			}},
		},
	}
	files, err := Render(Config{Framework: Vue}, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	idx := findFile(t, files, "index.ts")
	if !strings.Contains(idx, "export const listUsers") {
		t.Fatalf("index.ts missing listUsers export:\n%s", idx)
	}
	if !strings.Contains(idx, "T.User") {
		t.Fatalf("index.ts return type not namespaced — got:\n%s", idx)
	}
	types := findFile(t, files, "types.ts")
	if !strings.Contains(types, "export interface User") {
		t.Fatalf("types.ts missing User interface:\n%s", types)
	}
	vue := findFile(t, files, "vue.ts")
	if !strings.Contains(vue, "export function useListUsers") {
		t.Fatalf("vue.ts missing useListUsers composable:\n%s", vue)
	}
}

// TestRender_MutationGeneratesMutateComposable exercises the
// query-vs-mutation branch in the Vue renderer — a mutation should
// produce { mutate, ... }, not { refresh, ... }.
func TestRender_MutationGeneratesMutateComposable(t *testing.T) {
	reg := registry.New()
	reg.RegisterEndpoint(registry.Endpoint{
		Service:      "users",
		Name:         "createUser",
		Transport:    registry.GraphQL,
		Method:       "mutation",
		ArgsSchema:   &registry.TypeRef{Kind: "object", Object: &registry.NamedType{Fields: []registry.FieldSchema{{Name: "Email", JSONName: "email", Type: registry.TypeRef{Kind: "primitive", Primitive: "string"}}}}},
		ReturnSchema: &registry.TypeRef{Kind: "ref", Ref: "User"},
	})
	files, err := Render(Config{Framework: Vue}, extension.GenerateContext{
		Registry: reg,
		Refs:     map[string]registry.NamedType{"User": {Fields: nil}},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	vue := findFile(t, files, "vue.ts")
	if !strings.Contains(vue, "async function mutate") {
		t.Fatalf("mutation composable missing mutate() — got:\n%s", vue)
	}
	if strings.Contains(vue, "useCreateUser(vars?: MaybeRefOrGetter") {
		t.Fatalf("mutation composable shouldn't accept reactive vars in its constructor — got:\n%s", vue)
	}
}

// TestRender_DeterministicOutput is the byte-equality guarantee the
// `nexus generate --check` CI mode depends on. Render the same context
// twice; the two outputs must match exactly.
func TestRender_DeterministicOutput(t *testing.T) {
	reg := registry.New()
	for _, name := range []string{"listUsers", "listPosts", "listPets"} {
		reg.RegisterEndpoint(registry.Endpoint{
			Service:   "x",
			Name:      name,
			Transport: registry.GraphQL,
			Method:    "query",
		})
	}
	ctx := extension.GenerateContext{
		Registry: reg,
		Refs: map[string]registry.NamedType{
			"Zebra": {Fields: nil},
			"Alpha": {Fields: nil},
			"Mango": {Fields: nil},
		},
	}
	a, err := Render(Config{Framework: None}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(Config{Framework: None}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(filesByPath(a), filesByPath(b)) {
		t.Fatal("render output is not deterministic — registry iteration order leaked")
	}
}

// TestRender_MergesContributorFiles is the phase-2 contract test:
// any ClientContributor in ctx.Contributors gets its files appended
// to the driver's own output, attributed by the contributor and not
// silently overwriting the driver's emissions.
func TestRender_MergesContributorFiles(t *testing.T) {
	contrib := extension.ContributorFunc(func(ctx extension.GenerateContext) ([]extension.File, error) {
		return []extension.File{
			{Path: "auth/vue.ts", Body: []byte("// auth stub")},
		}, nil
	})
	files, err := Render(Config{Framework: Vue}, extension.GenerateContext{
		Registry:     registry.New(),
		Contributors: []extension.ClientContributor{contrib},
	})
	if err != nil {
		t.Fatal(err)
	}
	paths := fileNames(files)
	wantPresent := []string{"_client.ts", "types.ts", "index.ts", "vue.ts", "auth/vue.ts"}
	for _, w := range wantPresent {
		found := false
		for _, p := range paths {
			if p == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing %q in render output: %v", w, paths)
		}
	}
}

// TestRender_ContributorErrorAborts ensures a contributor failure
// surfaces as a render error rather than producing a half-tree.
// Partial writes would leave the consumer's source tree in a state
// the writer's no-op detection can't recover from.
func TestRender_ContributorErrorAborts(t *testing.T) {
	bad := extension.ContributorFunc(func(extension.GenerateContext) ([]extension.File, error) {
		return nil, errors.New("contributor broke")
	})
	_, err := Render(Config{Framework: None}, extension.GenerateContext{
		Registry:     registry.New(),
		Contributors: []extension.ClientContributor{bad},
	})
	if err == nil {
		t.Fatal("expected error from broken contributor")
	}
	if !strings.Contains(err.Error(), "contributor broke") {
		t.Fatalf("error %q does not wrap contributor cause", err)
	}
}

// TestRender_NilRegistryRejected catches the obvious operator mistake
// of calling render() without populating the context.
func TestRender_NilRegistryRejected(t *testing.T) {
	_, err := Render(Config{}, extension.GenerateContext{})
	if err == nil || !strings.Contains(err.Error(), "Registry is nil") {
		t.Fatalf("render with nil registry: %v, want 'Registry is nil'", err)
	}
}

// -- helpers --------------------------------------------------------

func fileNames(files []extension.File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

func findFile(t *testing.T, files []extension.File, name string) string {
	t.Helper()
	for _, f := range files {
		if f.Path == name {
			return string(f.Body)
		}
	}
	t.Fatalf("file %q not in output (%v)", name, fileNames(files))
	return ""
}

func filesByPath(files []extension.File) map[string]string {
	out := make(map[string]string, len(files))
	for _, f := range files {
		out[f.Path] = string(f.Body)
	}
	return out
}
