package client

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/registry"
)

func str() registry.TypeRef { return registry.TypeRef{Kind: "primitive", Primitive: "string"} }
func num() registry.TypeRef { return registry.TypeRef{Kind: "primitive", Primitive: "integer"} }

// pagesFixture has a component with two real routes and a trailing-slash
// twin, a list page with query arguments (one via a ref, one inline), a
// POST-only page, a page with both GET and POST, and a non-page endpoint.
func pagesFixture(basePath string) Manifest {
	page := func(method, path, comp string, args *registry.TypeRef) EndpointInfo {
		return EndpointInfo{Transport: "rest", Method: method, Path: path, Page: comp, Args: args}
	}
	idArgs := &registry.TypeRef{Kind: "object", Object: &registry.NamedType{Fields: []registry.FieldSchema{
		{Name: "ID", Type: num(), Path: "id"},
		{Name: "Tab", Type: str(), Query: "tab"},
	}}}
	return Manifest{
		Version:  SchemaVersion,
		BasePath: basePath,
		Refs: map[string]registry.NamedType{
			"listArgs": {Fields: []registry.FieldSchema{
				{Name: "Page", Type: num(), Query: "page"},
				{Name: "Status", Type: registry.TypeRef{Kind: "array", Of: &registry.TypeRef{Kind: "primitive", Primitive: "string"}}, Query: "status"},
				{Name: "Body", Type: str(), JSONName: "body"}, // json-only: not on the URL
			}},
		},
		Endpoints: []EndpointInfo{
			page("GET", "/permits/edit/:id", "Permits/Form", idArgs),
			page("GET", "/permits/register", "Permits/Form", nil),
			page("GET", "/permits/register/", "Permits/Form", nil),
			page("GET", "/users", "Users/Index", &registry.TypeRef{Kind: "ref", Ref: "listArgs"}),
			page("GET", "/users/", "Users/Index", &registry.TypeRef{Kind: "ref", Ref: "listArgs"}),
			page("POST", "/users/save", "Users/Save", nil),
			page("GET", "/auth/login", "Auth/Login", nil),
			page("POST", "/auth/logout", "Auth/Login", nil),
			page("GET", "/files/*path", "Files/Show", nil),
			{Transport: "rest", Method: "GET", Path: "/api/users"},
		},
	}
}

func TestPageRoutes_GroupsAndOrders(t *testing.T) {
	got := pageRoutes(pagesFixture(""))
	paths := func(c string) []string {
		var out []string
		for _, r := range got[c] {
			out = append(out, r.Path)
		}
		return out
	}
	cases := map[string][]string{
		// Most path params first; the twin collapses to the slash-less form.
		"Permits/Form": {"/permits/edit/:id", "/permits/register"},
		"Users/Index":  {"/users"},
		// POST-only pages keep their POST route.
		"Users/Save": {"/users/save"},
		// A GET route wins over a POST route for navigation.
		"Auth/Login": {"/auth/login"},
		"Files/Show": {"/files/*path"},
	}
	for c, want := range cases {
		if p := paths(c); !reflect.DeepEqual(p, want) {
			t.Errorf("%s routes = %v, want %v", c, p, want)
		}
	}
	if len(got) != len(cases) {
		t.Errorf("components = %d, want %d (non-page endpoints must be skipped)", len(got), len(cases))
	}
	users := got["Users/Index"][0].Query
	if len(users) != 2 || users[0].Key != "page" || users[0].TS != "number | string" ||
		users[1].Key != "status" || users[1].TS != "ReadonlyArray<string>" {
		t.Errorf("Users/Index query = %+v, want page + status (json-only body excluded)", users)
	}
	form := got["Permits/Form"][0]
	if !reflect.DeepEqual(form.Params, []string{"id"}) || len(form.Query) != 1 || form.Query[0].Key != "tab" {
		t.Errorf("Permits/Form edit route = %+v", form)
	}
}

func TestGeneratePagesDTS(t *testing.T) {
	d := GeneratePagesDTS(pagesFixture(""))
	for _, want := range []string{
		"export interface NexusPageRoutes {",
		"  'Permits/Form':\n    | { id: string | number; tab?: string }\n    | { [key: string]: never }\n",
		"  'Users/Index': { page?: number | string; status?: ReadonlyArray<string> }\n",
		"  'Files/Show': { path: string | number }\n",
		"  'Permits/Form': '/permits/edit/:id' | '/permits/register'\n",
		"export declare function pageUrl<C extends PageComponent>(",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("pages.d.ts missing %q\n---\n%s", want, d)
		}
	}
	if GeneratePagesDTS(Manifest{}) != "" || GeneratePagesJS(Manifest{}) != "" {
		t.Error("no pages must generate nothing")
	}
}

// TestPageUrlRuntime runs the generated pages.js under node.
func TestPageUrlRuntime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pages.mjs"), []byte(GeneratePagesJS(pagesFixture("/app"))), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `
import { pageUrl } from './pages.mjs'
const out = {}
const run = (k, f) => { try { out[k] = f() } catch (e) { out[k] = 'ERR ' + e.message } }
run('edit', () => pageUrl('Permits/Form', { id: 42, tab: 'docs' }))
run('register', () => pageUrl('Permits/Form'))
run('masked', () => pageUrl('Permits/Form', { id: 'a/b c' }))
run('forced', () => pageUrl('Permits/Form', { id: 1 }, { route: '/permits/register' }))
run('list', () => pageUrl('Users/Index', { page: 2, status: ['open', 'held'], empty: null }))
run('extra', () => pageUrl('Users/Index', {}, { query: { sort: '-name' } }))
run('splat', () => pageUrl('Files/Show', { path: 'a b/c.pdf' }))
run('unknown', () => pageUrl('Nope'))
run('badRoute', () => pageUrl('Permits/Form', {}, { route: '/nope' }))
run('missing', () => pageUrl('Files/Show', {}))
console.log(JSON.stringify(out))
`
	if err := os.WriteFile(filepath.Join(dir, "run.mjs"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "run.mjs")
	cmd.Dir = dir
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	want := map[string]string{
		"edit":     "/app/permits/edit/42?tab=docs",
		"register": "/app/permits/register",
		"masked":   "/app/permits/edit/a%2Fb%20c",
		"forced":   "/app/permits/register?id=1",
		"list":     "/app/users?page=2&status=open&status=held",
		"extra":    "/app/users?sort=-name",
		"splat":    "/app/files/a%20b/c.pdf",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
	for _, k := range []string{"unknown", "badRoute", "missing"} {
		if !strings.HasPrefix(got[k], "ERR pageUrl:") {
			t.Errorf("%s = %q, want a pageUrl error", k, got[k])
		}
	}
}

func TestWritePagesFiles_WritesAndRemoves(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	if err := WritePagesFiles(dir, pagesFixture(""), &out); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"pages.js", "pages.d.ts"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s not written: %v", f, err)
		}
	}
	if err := WritePagesFiles(dir, Manifest{}, &out); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"pages.js", "pages.d.ts"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s should be removed once the app has no pages", f)
		}
	}
	// A hand-written file of the same name is not ours to remove.
	own := filepath.Join(dir, "pages.js")
	if err := os.WriteFile(own, []byte("export const mine = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WritePagesFiles(dir, Manifest{}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(own); err != nil {
		t.Error("a hand-written pages.js must be left alone")
	}
}
