package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

// fakeNexusApp stands in for a running nexus app at install time —
// serves /__nexus/client/manifest.json plus the file artifacts the
// install path expects. Tests construct one, point the install
// flow at httptest.Server.URL, and assert on the resulting
// lockfile + node_modules tree.
func fakeNexusApp(t *testing.T, version string) *httptest.Server {
	t.Helper()
	files := map[string]string{
		"manifest.json": `{"version":"` + version + `","endpoints":[]}`,
		"client.js":     "// nexus-client core\n",
		"client.d.ts":   "export class NexusClient {}\n",
		"vue.js":        "// vue adapter\n",
		"vue.d.ts":      "export function useNexus(): unknown\n",
		"react.js":      "// react adapter\n",
		"react.d.ts":    "export function useNexus(): unknown\n",
	}
	mux := http.NewServeMux()
	for name, body := range files {
		ct := "application/javascript; charset=utf-8"
		switch {
		case strings.HasSuffix(name, ".json"):
			ct = "application/json; charset=utf-8"
		case strings.HasSuffix(name, ".d.ts"):
			ct = "application/typescript; charset=utf-8"
		}
		mux.HandleFunc("/__nexus/client/"+name, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.Write([]byte(body))
		})
	}
	return httptest.NewServer(mux)
}

func TestIsNexusClientSpec(t *testing.T) {
	tests := []struct {
		spec string
		want bool
	}{
		{"nexus-client", true},
		{"nexus-client@1.2.3", true},
		{"nexus-client/vue", true},
		{"nexus-client/vue@1.2.3", true},
		{"nexus-client/react", true},
		// Lookalikes that must stay on the esm.sh path.
		{"nexus-client-other", false},
		{"@scope/nexus-client", false},
		{"nexus-client/unknown-adapter", false},
		{"vue", false},
		{"", false},
	}
	for _, tt := range tests {
		got := isNexusClientSpec(tt.spec)
		if got != tt.want {
			t.Errorf("isNexusClientSpec(%q) = %v, want %v", tt.spec, got, tt.want)
		}
	}
}

func TestFilesForSpec_CoreOnly(t *testing.T) {
	f := filesForSpec("nexus-client")
	if len(f.core) != 2 || f.core[0] != "client.js" || f.core[1] != "client.d.ts" {
		t.Errorf("core: %+v", f.core)
	}
	if len(f.adapter) != 0 {
		t.Errorf("expected no adapter files, got %v", f.adapter)
	}
}

func TestFilesForSpec_VueAdapter(t *testing.T) {
	f := filesForSpec("nexus-client/vue@1.0.0")
	if len(f.adapter) != 2 || f.adapter[0] != "vue.js" {
		t.Errorf("vue adapter: %+v", f.adapter)
	}
}

func TestFilesForSpec_ReactAdapter(t *testing.T) {
	f := filesForSpec("nexus-client/react")
	if len(f.adapter) != 2 || f.adapter[0] != "react.js" {
		t.Errorf("react adapter: %+v", f.adapter)
	}
}

func TestResolveNexusClientOrigin_Default(t *testing.T) {
	t.Setenv(nexusClientOriginEnv, "")
	got := resolveNexusClientOrigin("")
	if got != "http://localhost:8080" {
		t.Errorf("default origin: got %q", got)
	}
}

func TestResolveNexusClientOrigin_EnvOverride(t *testing.T) {
	t.Setenv(nexusClientOriginEnv, "https://staging.example.com:9000")
	got := resolveNexusClientOrigin("")
	if got != "https://staging.example.com:9000" {
		t.Errorf("env: got %q", got)
	}
}

func TestResolveNexusClientOrigin_ExplicitWins(t *testing.T) {
	t.Setenv(nexusClientOriginEnv, "https://env.example.com")
	got := resolveNexusClientOrigin("https://flag.example.com/")
	if got != "https://flag.example.com" {
		t.Errorf("explicit should beat env: got %q", got)
	}
}

func TestResolveNexusClientOrigin_BareHostGetsHttpScheme(t *testing.T) {
	got := resolveNexusClientOrigin("localhost:9999")
	if got != "http://localhost:9999" {
		t.Errorf("schemeless input: got %q", got)
	}
}

// TestAddNexusClient_Core drives the install path end-to-end
// against an httptest server: the bytes land in the deps store
// (content-addressed) AND in ./node_modules/nexus-client/. The
// returned lockfile.Package entries pin the resolved URL so a
// subsequent `nexus install` from a fresh clone fetches the same
// bytes from the same app.
func TestAddNexusClient_Core(t *testing.T) {
	server := fakeNexusApp(t, "0.82.0")
	defer server.Close()

	// Isolated cwd + deps store so each test run starts clean
	// (without this the materialized node_modules from one test
	// would survive into the next).
	cwd := t.TempDir()
	must(t, os.Chdir(cwd))
	t.Cleanup(func() { _ = os.Chdir("/") })

	storeRoot := t.TempDir()
	st, err := store.New(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	dc := &depsContext{store: st, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}

	pkgs, version, err := addNexusClient(context.Background(), dc, "nexus-client", server.URL, io.Discard)
	if err != nil {
		t.Fatalf("addNexusClient: %v", err)
	}

	if version != "0.82.0" {
		t.Errorf("version: got %q, want 0.82.0", version)
	}
	if len(pkgs) != 2 {
		t.Fatalf("expected 2 packages (client.js + client.d.ts), got %d", len(pkgs))
	}
	// Resolved URL must point back at the app — the lockfile's job
	// is to pin THIS bytes-from-THIS app for `nexus install` to
	// replay later.
	for _, p := range pkgs {
		if !strings.HasPrefix(p.Resolved, server.URL+"/__nexus/client/") {
			t.Errorf("Resolved %q should point at the app", p.Resolved)
		}
		if !strings.HasPrefix(p.Integrity, "sha256-") {
			t.Errorf("Integrity should be sha256-prefixed, got %q", p.Integrity)
		}
	}

	// node_modules materialization.
	for _, expected := range []string{"client.js", "client.d.ts", "package.json"} {
		p := filepath.Join(cwd, "node_modules", "nexus-client", expected)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}

	// package.json stub must have an exports map covering . / ./vue
	// / ./react so bundlers + IDEs resolve all three subpaths.
	pjBody, err := os.ReadFile(filepath.Join(cwd, "node_modules", "nexus-client", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pj struct {
		Name    string         `json:"name"`
		Version string         `json:"version"`
		Exports map[string]any `json:"exports"`
		Types   string         `json:"types"`
	}
	if err := json.Unmarshal(pjBody, &pj); err != nil {
		t.Fatal(err)
	}
	if pj.Name != "nexus-client" || pj.Version != "0.82.0" {
		t.Errorf("stub package.json: %+v", pj)
	}
	if pj.Types != "./client.d.ts" {
		t.Errorf("stub.types: got %q, want ./client.d.ts", pj.Types)
	}
	for _, key := range []string{".", "./vue", "./react"} {
		if _, ok := pj.Exports[key]; !ok {
			t.Errorf("stub.exports missing key %q", key)
		}
	}
}

// TestAddNexusClient_VueAdapterPullsCorePlusVueFiles ensures the
// adapter subpath fetches both layers (core + adapter) so the
// developer doesn't need to add `nexus-client` separately.
func TestAddNexusClient_VueAdapterPullsCorePlusVueFiles(t *testing.T) {
	server := fakeNexusApp(t, "0.82.0")
	defer server.Close()

	cwd := t.TempDir()
	must(t, os.Chdir(cwd))
	t.Cleanup(func() { _ = os.Chdir("/") })

	storeRoot := t.TempDir()
	st, _ := store.New(storeRoot)
	dc := &depsContext{store: st, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}

	pkgs, _, err := addNexusClient(context.Background(), dc, "nexus-client/vue", server.URL, io.Discard)
	if err != nil {
		t.Fatalf("addNexusClient: %v", err)
	}
	if len(pkgs) != 4 {
		t.Fatalf("expected 4 packages (core + vue adapter), got %d", len(pkgs))
	}

	wantSpecs := map[string]bool{
		"nexus-client/client.js":   false,
		"nexus-client/client.d.ts": false,
		"nexus-client/vue.js":      false,
		"nexus-client/vue.d.ts":    false,
	}
	for _, p := range pkgs {
		wantSpecs[p.Spec] = true
	}
	for spec, found := range wantSpecs {
		if !found {
			t.Errorf("missing spec entry %q", spec)
		}
	}

	for _, expected := range []string{"client.js", "client.d.ts", "vue.js", "vue.d.ts"} {
		p := filepath.Join(cwd, "node_modules", "nexus-client", expected)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}

// TestAddNexusClient_UnreachableOriginFailsClean asserts that a
// typo / down server surfaces a clean error BEFORE any bytes land
// in the cache. The manifest probe is what catches this — if it
// trips, we never start the per-file fetch loop.
func TestAddNexusClient_UnreachableOriginFailsClean(t *testing.T) {
	cwd := t.TempDir()
	must(t, os.Chdir(cwd))
	t.Cleanup(func() { _ = os.Chdir("/") })

	storeRoot := t.TempDir()
	st, _ := store.New(storeRoot)
	dc := &depsContext{store: st, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}

	_, _, err := addNexusClient(context.Background(), dc, "nexus-client",
		"http://127.0.0.1:1", io.Discard) // closed port — guaranteed-unreachable origin
	if err == nil {
		t.Fatal("expected error on unreachable origin")
	}
	// node_modules must NOT have been created — pre-flight failure
	// guards against partial install state.
	if _, err := os.Stat(filepath.Join(cwd, "node_modules", "nexus-client")); err == nil {
		t.Error("partial install: node_modules/nexus-client/ was created despite unreachable origin")
	}
}

// TestParseSpecForPJ_NexusClientCollapses verifies that the
// package.json normalizer collapses adapter subpaths to the bare
// name so `nexus add nexus-client/vue` writes one
// `"nexus-client": "<version>"` entry (not "nexus-client/vue").
func TestParseSpecForPJ_NexusClientCollapses(t *testing.T) {
	cases := []struct {
		in, name, version string
	}{
		{"nexus-client", "nexus-client", ""},
		{"nexus-client@1.0.0", "nexus-client", "1.0.0"},
		{"nexus-client/vue", "nexus-client", ""},
		{"nexus-client/react@2.0.0", "nexus-client", "2.0.0"},
		// Non-nexus-client specs pass through unchanged.
		{"vue", "vue", ""},
		{"@vue/runtime-dom@3.4.0", "@vue/runtime-dom", "3.4.0"},
	}
	for _, c := range cases {
		name, version := parseSpecForPJ(c.in)
		if name != c.name || version != c.version {
			t.Errorf("parseSpecForPJ(%q) = (%q, %q), want (%q, %q)",
				c.in, name, version, c.name, c.version)
		}
	}
}

// TestAddNexusClient_LockfileEntryShape spot-checks that the
// generated lockfile.Package entries have the shape `nexus install`
// expects on a fresh clone: Spec / Version / Resolved / Integrity
// all populated, ContentType present, Deps empty (each file is
// its own atom; no transitives).
func TestAddNexusClient_LockfileEntryShape(t *testing.T) {
	server := fakeNexusApp(t, "0.82.0")
	defer server.Close()

	cwd := t.TempDir()
	must(t, os.Chdir(cwd))
	t.Cleanup(func() { _ = os.Chdir("/") })

	storeRoot := t.TempDir()
	st, _ := store.New(storeRoot)
	dc := &depsContext{store: st, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}

	pkgs, _, _ := addNexusClient(context.Background(), dc, "nexus-client/react", server.URL, io.Discard)
	for _, p := range pkgs {
		if p.Spec == "" || p.Version == "" || p.Resolved == "" || p.Integrity == "" {
			t.Errorf("incomplete pkg: %+v", p)
		}
		if len(p.Deps) != 0 {
			t.Errorf("nexus-client packages should have no Deps, got %v", p.Deps)
		}
	}

	// Also exercise the lockfile.File.Add path so we know the
	// shape doesn't trip any internal validation.
	lf := lockfile.New()
	for _, p := range pkgs {
		lf.Add(p)
	}
	if len(lf.Packages) != 4 {
		t.Errorf("lockfile got %d packages, want 4", len(lf.Packages))
	}
}

// TestAddNexusClient_OfflineFallbackUsesEmbeddedBundle: when the
// default origin (localhost:8080) is unreachable AND no explicit
// override was set, the install pulls bytes from the CLI binary's
// own go:embed copy. The lockfile entries get a synthetic
// `embedded://` Resolved URL so `nexus install` doesn't try to
// re-fetch over HTTP.
func TestAddNexusClient_OfflineFallbackUsesEmbeddedBundle(t *testing.T) {
	t.Setenv(nexusClientOriginEnv, "") // no explicit override

	cwd := t.TempDir()
	must(t, os.Chdir(cwd))
	t.Cleanup(func() { _ = os.Chdir("/") })

	storeRoot := t.TempDir()
	st, _ := store.New(storeRoot)
	dc := &depsContext{store: st, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}

	// originOverride = "" means "use the default localhost:8080".
	// Nothing's running there in the test env so the install must
	// fall back to embedded bytes.
	logBuf := &bytes.Buffer{}
	pkgs, version, err := addNexusClient(context.Background(), dc, "nexus-client/react", "", logBuf)
	if err != nil {
		t.Fatalf("offline fallback should not error, got %v", err)
	}
	if len(pkgs) != 4 {
		t.Fatalf("expected 4 packages (core + react adapter), got %d", len(pkgs))
	}
	if version == "" {
		t.Error("offline install should still pin a version (the CLI's own)")
	}

	// Every Resolved URL must use the synthetic embedded:// scheme
	// — that's what tells `nexus install` to skip the HTTP fetch.
	for _, p := range pkgs {
		if !strings.HasPrefix(p.Resolved, "embedded://nexus-client/") {
			t.Errorf("offline Resolved %q must use embedded:// scheme", p.Resolved)
		}
		if !strings.HasPrefix(p.Integrity, "sha256-") {
			t.Errorf("Integrity missing sha256- prefix: %q", p.Integrity)
		}
	}

	// node_modules materialization must still happen — that's the
	// whole point of the offline path.
	for _, expected := range []string{"client.js", "client.d.ts", "react.js", "react.d.ts", "package.json"} {
		p := filepath.Join(cwd, "node_modules", "nexus-client", expected)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}

	// User feedback message should mention the fallback so the
	// operator understands why typed endpoints aren't there yet.
	if !strings.Contains(logBuf.String(), "embedded in CLI") {
		t.Errorf("expected 'embedded in CLI' notice in stdout, got: %q", logBuf.String())
	}
}

// TestAddNexusClient_ExplicitOriginFailureStillErrors: when the
// operator EXPLICITLY pointed at an unreachable URL (--from or
// NEXUS_DEV_URL), we must NOT fall back to embedded — that would
// silently override their intent. Fail loud instead.
func TestAddNexusClient_ExplicitOriginFailureStillErrors(t *testing.T) {
	cwd := t.TempDir()
	must(t, os.Chdir(cwd))
	t.Cleanup(func() { _ = os.Chdir("/") })

	storeRoot := t.TempDir()
	st, _ := store.New(storeRoot)
	dc := &depsContext{store: st, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}

	_, _, err := addNexusClient(context.Background(), dc, "nexus-client",
		"http://127.0.0.1:1", io.Discard) // explicit override → no fallback
	if err == nil {
		t.Fatal("explicit unreachable origin should error, not fall back")
	}
	if _, statErr := os.Stat(filepath.Join(cwd, "node_modules", "nexus-client")); statErr == nil {
		t.Error("partial install: node_modules/nexus-client/ should not exist when explicit origin fails")
	}
}

// TestAddNexusClient_OfflineFallbackHonoredViaEnv: NEXUS_DEV_URL
// counts as explicit-intent — if the operator set it, an
// unreachable origin must NOT silently fall back. Catches the
// "I set NEXUS_DEV_URL=staging and forgot to start it" footgun.
func TestAddNexusClient_OfflineFallbackHonoredViaEnv(t *testing.T) {
	t.Setenv(nexusClientOriginEnv, "http://127.0.0.1:1")

	cwd := t.TempDir()
	must(t, os.Chdir(cwd))
	t.Cleanup(func() { _ = os.Chdir("/") })

	storeRoot := t.TempDir()
	st, _ := store.New(storeRoot)
	dc := &depsContext{store: st, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}

	_, _, err := addNexusClient(context.Background(), dc, "nexus-client", "", io.Discard)
	if err == nil {
		t.Fatal("env-set unreachable origin should error, not fall back to embedded")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
