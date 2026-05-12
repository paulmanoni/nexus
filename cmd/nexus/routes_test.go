package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	nexusmanifest "github.com/paulmanoni/nexus/manifest"
)

// writeRoutesJSON writes a minimal Manifest with the given Routes
// slice to a temp file and returns the path. Lets tests drive
// runRoutes through the same input pipeline operators use.
func writeRoutesJSON(t *testing.T, routes []nexusmanifest.Route) string {
	t.Helper()
	m := nexusmanifest.Manifest{
		SchemaVersion: "1",
		App:           nexusmanifest.AppIdentity{Name: "demo"},
		Name:          "demo",
		Health: nexusmanifest.Health{
			Liveness:  "/__nexus/health",
			Readiness: "/__nexus/ready",
		},
		Routes: routes,
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// sampleRoutes mirrors the smoke-test data: a representative mix of
// kinds and auth settings so filter tests cover the matrix.
func sampleRoutes() []nexusmanifest.Route {
	return []nexusmanifest.Route{
		{ID: "r1", Kind: "rest", Method: "GET", Path: "/users/:id", Module: "users", Auth: "required"},
		{ID: "r2", Kind: "rest", Method: "GET", Path: "/users", Module: "users"},
		{ID: "r3", Kind: "rest", Method: "POST", Path: "/checkout", Module: "checkout", Auth: "required"},
		{ID: "r4", Kind: "graphql.query", Operation: "listAdverts", Module: "adverts", Auth: "optional"},
		{ID: "r5", Kind: "graphql.mutation", Operation: "createAdvert", Module: "adverts", Auth: "required"},
		{ID: "r6", Kind: "ws", Path: "/events", Module: "chat", Auth: "required"},
	}
}

func TestRoutes_TextOutput_AllRoutes(t *testing.T) {
	path := writeRoutesJSON(t, sampleRoutes())
	stdout := new(bytes.Buffer)
	err := runRoutes(stdout, new(bytes.Buffer), routesOptions{filePath: path})
	if err != nil {
		t.Fatalf("expected nil error; got %v", err)
	}
	out := stdout.String()
	for _, expect := range []string{"6 routes", "/users", "/checkout", "listAdverts", "createAdvert", "/events"} {
		if !strings.Contains(out, expect) {
			t.Errorf("output missing %q:\n%s", expect, out)
		}
	}
}

func TestRoutes_KindFilter(t *testing.T) {
	path := writeRoutesJSON(t, sampleRoutes())
	stdout := new(bytes.Buffer)
	if err := runRoutes(stdout, new(bytes.Buffer), routesOptions{filePath: path, kindFilter: "rest"}); err != nil {
		t.Fatalf("rest filter: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "3 routes") {
		t.Errorf("expected 3 REST routes; got:\n%s", out)
	}
	if strings.Contains(out, "listAdverts") || strings.Contains(out, "/events") {
		t.Errorf("non-REST routes leaked into output:\n%s", out)
	}
}

func TestRoutes_MethodFilter_CaseInsensitive(t *testing.T) {
	path := writeRoutesJSON(t, sampleRoutes())
	stdout := new(bytes.Buffer)
	if err := runRoutes(stdout, new(bytes.Buffer), routesOptions{filePath: path, methodFilter: "get"}); err != nil {
		t.Fatalf("get filter: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "2 routes") {
		t.Errorf("expected 2 GET routes; got:\n%s", out)
	}
	if strings.Contains(out, "POST") || strings.Contains(out, "/checkout") {
		t.Errorf("non-GET leaked:\n%s", out)
	}
}

func TestRoutes_PathSubstring(t *testing.T) {
	path := writeRoutesJSON(t, sampleRoutes())
	stdout := new(bytes.Buffer)
	if err := runRoutes(stdout, new(bytes.Buffer), routesOptions{filePath: path, pathFilter: "users"}); err != nil {
		t.Fatalf("path filter: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "2 routes") {
		t.Errorf("expected 2 /users routes; got:\n%s", out)
	}
}

func TestRoutes_AuthFilter(t *testing.T) {
	path := writeRoutesJSON(t, sampleRoutes())
	stdout := new(bytes.Buffer)
	if err := runRoutes(stdout, new(bytes.Buffer), routesOptions{filePath: path, authFilter: "required"}); err != nil {
		t.Fatalf("auth filter: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "4 routes") {
		t.Errorf("expected 4 required-auth routes; got:\n%s", out)
	}
}

func TestRoutes_ModuleFilter(t *testing.T) {
	path := writeRoutesJSON(t, sampleRoutes())
	stdout := new(bytes.Buffer)
	if err := runRoutes(stdout, new(bytes.Buffer), routesOptions{filePath: path, moduleFilter: "adverts"}); err != nil {
		t.Fatalf("module filter: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "2 routes") {
		t.Errorf("expected 2 adverts routes; got:\n%s", out)
	}
}

func TestRoutes_EmptyResult_StillSuccess(t *testing.T) {
	// Filter eliminates every route; should still exit 0 and produce
	// a friendly "no routes matched" line — operators piping to wc
	// shouldn't see a non-zero exit on a deliberately narrow filter.
	path := writeRoutesJSON(t, sampleRoutes())
	stdout := new(bytes.Buffer)
	err := runRoutes(stdout, new(bytes.Buffer), routesOptions{
		filePath: path, methodFilter: "TRACE",
	})
	if err != nil {
		t.Fatalf("empty result should exit 0; got %v", err)
	}
	if !strings.Contains(stdout.String(), "no routes matched") {
		t.Errorf("expected empty-result message:\n%s", stdout.String())
	}
}

func TestRoutes_JSONOutput(t *testing.T) {
	path := writeRoutesJSON(t, sampleRoutes())
	stdout := new(bytes.Buffer)
	if err := runRoutes(stdout, new(bytes.Buffer), routesOptions{filePath: path, jsonOut: true, kindFilter: "ws"}); err != nil {
		t.Fatalf("json output: %v", err)
	}
	var got []nexusmanifest.Route
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("parse json: %v\n%s", err, stdout.String())
	}
	if len(got) != 1 || got[0].Path != "/events" {
		t.Errorf("expected 1 WS route at /events; got %+v", got)
	}
}

func TestRoutes_SortOrder_KindThenMethodThenPath(t *testing.T) {
	// Out-of-order input; expected order: rest GET first
	// (alpha-by-path within), then rest POST, then graphql.query.
	routes := []nexusmanifest.Route{
		{Kind: "graphql.query", Operation: "z"},
		{Kind: "rest", Method: "POST", Path: "/checkout"},
		{Kind: "rest", Method: "GET", Path: "/users"},
		{Kind: "rest", Method: "GET", Path: "/admin"},
	}
	path := writeRoutesJSON(t, routes)
	stdout := new(bytes.Buffer)
	if err := runRoutes(stdout, new(bytes.Buffer), routesOptions{filePath: path}); err != nil {
		t.Fatalf("%v", err)
	}
	out := stdout.String()
	idxAdmin := strings.Index(out, "/admin")
	idxUsers := strings.Index(out, "/users")
	idxCheckout := strings.Index(out, "/checkout")
	idxQuery := strings.Index(out, "z")
	if !(idxAdmin < idxUsers && idxUsers < idxCheckout && idxCheckout < idxQuery) {
		t.Errorf("expected order admin<users<checkout<z, got positions: admin=%d users=%d checkout=%d z=%d\n%s",
			idxAdmin, idxUsers, idxCheckout, idxQuery, out)
	}
}

func TestRoutes_EndpointsFallback_WhenRoutesEmpty(t *testing.T) {
	// Older manifest emits Endpoints but no Routes — verify the
	// fallback projection picks them up.
	m := nexusmanifest.Manifest{
		SchemaVersion: "1",
		App:           nexusmanifest.AppIdentity{Name: "old"},
		Name:          "old",
		Health: nexusmanifest.Health{
			Liveness: "/__nexus/health", Readiness: "/__nexus/ready",
		},
		Endpoints: []nexusmanifest.EndpointSummary{
			{Service: "users", Transport: "rest", Method: "GET", Path: "/users"},
			{Service: "chat", Transport: "ws", Path: "/events"},
		},
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	stdout := new(bytes.Buffer)
	if err := runRoutes(stdout, new(bytes.Buffer), routesOptions{filePath: path}); err != nil {
		t.Fatalf("fallback: %v", err)
	}
	out := stdout.String()
	for _, expect := range []string{"/users", "/events", "users", "chat"} {
		if !strings.Contains(out, expect) {
			t.Errorf("fallback missing %q:\n%s", expect, out)
		}
	}
}

func TestRoutes_PathAndBinary_MutuallyExclusive(t *testing.T) {
	err := runRoutes(new(bytes.Buffer), new(bytes.Buffer), routesOptions{
		filePath:   "x.json",
		binaryPath: "./bin",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "cannot combine") {
		t.Errorf("error should mention conflict: %v", err)
	}
}

func TestRoutes_YamlBinaryCombo_Rejected(t *testing.T) {
	err := runRoutes(new(bytes.Buffer), new(bytes.Buffer), routesOptions{
		inputFormat: "yaml",
		binaryPath:  "./bin",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error should mention yaml: %v", err)
	}
}

func TestRoutes_CobraCommand_WiredInRoot(t *testing.T) {
	root := newRootCmd(new(bytes.Buffer), new(bytes.Buffer))
	for _, c := range root.Commands() {
		if c.Name() == "routes" {
			return
		}
	}
	t.Fatal("routes not registered on root")
}
