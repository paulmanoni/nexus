package client

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/paulmanoni/nexus/httpx/stdrouter"
	"github.com/paulmanoni/nexus/registry"
)

// pagesRegistry registers one ordinary REST route, a typed page, an untyped
// page, and two typed shared props — the registry shape inertia.Page +
// ShareScoped/ShareTyped produce.
func pagesRegistry() *registry.Registry {
	reg := registry.New()
	reg.RegisterEndpoint(registry.Endpoint{
		Name: "listPets", Transport: registry.REST, Method: "GET", Path: "/pets", Service: "pets",
		ReturnSchema: &registry.TypeRef{Kind: "array", Of: &registry.TypeRef{Kind: "ref", Ref: "Pet"}},
	})
	reg.RegisterEndpoint(registry.Endpoint{
		Name: "petsPage", Transport: registry.REST, Method: "GET", Path: "/pets/page", Service: "pets",
		Tags:         map[string]string{registry.PageTag: "Pets/Index"},
		ReturnSchema: &registry.TypeRef{Kind: "ref", Ref: "PetsPageProps"},
	})
	reg.RegisterEndpoint(registry.Endpoint{
		Name: "about", Transport: registry.REST, Method: "GET", Path: "/about", Service: "pets",
		Tags:         map[string]string{registry.PageTag: "About"},
		ReturnSchema: &registry.TypeRef{Kind: "any"},
	})
	reg.SetSharedProp("can", registry.TypeRef{Kind: "map",
		KeyOf: &registry.TypeRef{Kind: "primitive", Primitive: "string"},
		Of:    &registry.TypeRef{Kind: "primitive", Primitive: "boolean"}})
	reg.SetSharedProp("viewer", registry.TypeRef{Kind: "ref", Ref: "Viewer", Optional: true})
	return reg
}

// TestBuildManifest_PagesAndSharedProps pins the projection: the page tag
// becomes EndpointInfo.Page (only on pages) and the registry's typed shared
// props become Manifest.SharedProps.
func TestBuildManifest_PagesAndSharedProps(t *testing.T) {
	m := buildManifest(pagesRegistry(), nil, nil, "")

	pages := map[string]string{}
	for _, e := range m.Endpoints {
		pages[e.Path] = e.Page
	}
	want := map[string]string{"/pets": "", "/pets/page": "Pets/Index", "/about": "About"}
	for path, w := range want {
		if pages[path] != w {
			t.Errorf("%s: Page = %q, want %q", path, pages[path], w)
		}
	}
	if len(m.SharedProps) != 2 {
		t.Fatalf("SharedProps = %v, want can + viewer", m.SharedProps)
	}
	if c := m.SharedProps["can"]; c == nil || c.Kind != "map" {
		t.Errorf("can = %+v, want a map", c)
	}
	if v := m.SharedProps["viewer"]; v == nil || v.Ref != "Viewer" || !v.Optional {
		t.Errorf("viewer = %+v, want optional ref Viewer", v)
	}

	if empty := buildManifest(registry.New(), nil, nil, ""); empty.SharedProps != nil {
		t.Errorf("no shared props must leave SharedProps nil (omitted), got %v", empty.SharedProps)
	}
}

// TestGenerateClientDTS_InertiaTypes covers the page/share emission rules
// against a manifest built from the registry.
func TestGenerateClientDTS_InertiaTypes(t *testing.T) {
	m := buildManifest(pagesRegistry(), nil, nil, "")
	// A second route for Pets/Index with different props → union; a third
	// with the SAME props as the first must not duplicate it.
	m.Endpoints = append(m.Endpoints,
		EndpointInfo{Transport: "rest", Method: "POST", Path: "/pets/page", Page: "Pets/Index",
			Return: &registry.TypeRef{Kind: "ref", Ref: "PetSearchProps"}},
		EndpointInfo{Transport: "rest", Method: "PUT", Path: "/pets/page", Page: "Pets/Index",
			Return: &registry.TypeRef{Kind: "ref", Ref: "PetsPageProps", Optional: true}},
	)
	out := GenerateClientDTS(m)

	restBlock := between(out, "export interface RestEndpoints {", "}\n")
	if !strings.Contains(restBlock, "'GET /pets'") {
		t.Errorf("ordinary REST route missing from RestEndpoints:\n%s", restBlock)
	}
	for _, page := range []string{"/pets/page", "/about"} {
		if strings.Contains(restBlock, page) {
			t.Errorf("page route %s must not be emitted in RestEndpoints:\n%s", page, restBlock)
		}
	}
	for _, want := range []string{
		"  'Pets/Index': PetSearchProps | PetsPageProps\n",
		"  'About': { [key: string]: unknown }\n",
		"  can: Record<string, boolean>\n",
		"  viewer?: Viewer\n",
		"  [key: string]: unknown\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("client.d.ts missing %q", want)
		}
	}
	if strings.Contains(out, "PageProps<") {
		t.Error("no generic page-props alias may be emitted — Vue's compiler cannot resolve it")
	}

	// A manifest with neither pages nor shared props emits neither interface.
	plain := GenerateClientDTS(buildManifest(registry.New(), nil, nil, ""))
	if strings.Contains(plain, "NexusPageProps") || strings.Contains(plain, "NexusSharedProps") {
		t.Error("Inertia interfaces emitted for a manifest without pages or shared props")
	}
}

// TestGenerateInertiaDTS covers both sides: empty without pages/shares,
// the augmentation otherwise — including shares-only and pages-only apps.
func TestGenerateInertiaDTS(t *testing.T) {
	if got := GenerateInertiaDTS(Manifest{Version: SchemaVersion}); got != "" {
		t.Fatalf("no pages/shares: want empty, got:\n%s", got)
	}
	cases := map[string]Manifest{
		"pages only":  {Endpoints: []EndpointInfo{{Transport: "rest", Page: "Home"}}},
		"shares only": {SharedProps: map[string]*registry.TypeRef{"can": {Kind: "any"}}},
	}
	for name, m := range cases {
		out := GenerateInertiaDTS(m)
		for _, want := range []string{
			"import type { NexusSharedProps } from './client'",
			"declare module '@inertiajs/core'",
			"sharedPageProps: NexusSharedProps",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: inertia.d.ts missing %q:\n%s", name, want, out)
			}
		}
		if dts := GenerateClientDTS(m); !strings.Contains(dts, "export interface NexusSharedProps") {
			t.Errorf("%s: client.d.ts must export the NexusSharedProps inertia.d.ts imports", name)
		}
	}
}

// TestMount_InertiaDTSRoute serves inertia.d.ts when the registry has pages
// and 404s when it has none.
func TestMount_InertiaDTSRoute(t *testing.T) {
	get := func(reg *registry.Registry) (int, string) {
		e := stdrouter.New()
		Mount(e, reg, nil, nil, "", Config{})
		srv := httptest.NewServer(e)
		defer srv.Close()
		res, err := http.Get(srv.URL + "/__nexus/client/inertia.d.ts")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if code, body := get(pagesRegistry()); code != http.StatusOK || !strings.Contains(body, "sharedPageProps") {
		t.Errorf("with pages: status %d body %q", code, body)
	}
	if code, _ := get(registry.New()); code != http.StatusNotFound {
		t.Errorf("without pages: status %d, want 404", code)
	}
}

// TestDump_InertiaDTS writes inertia.d.ts beside client.d.ts when there are
// pages, and removes a generated copy — never a hand-written file — once
// there are none.
func TestDump_InertiaDTS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inertia.d.ts")

	withPages := &Handler{reg: pagesRegistry(), once: &sync.Once{}}
	if err := withPages.Dump(dir, "", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), "sharedPageProps") {
		t.Fatalf("inertia.d.ts not written: %v %q", err, b)
	}

	without := &Handler{reg: registry.New(), once: &sync.Once{}}
	if err := without.Dump(dir, "", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale generated inertia.d.ts must be removed, stat err = %v", err)
	}

	if err := os.WriteFile(path, []byte("// mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := without.Dump(dir, "", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a hand-written inertia.d.ts must be left alone: %v", err)
	}
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i:]
	j := strings.Index(rest, end)
	if j < 0 {
		return rest
	}
	return rest[:j]
}
