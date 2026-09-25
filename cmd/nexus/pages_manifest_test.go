package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/client"
	"github.com/paulmanoni/nexus/registry"
)

// TestRegistryFromManifest_RoundTripsPageTag: the manifest flattens the
// Inertia page tag into EndpointInfo.Page; the offline codegen path must put
// it back on Tags (next to auth.flow) so the renderer skips the page exactly
// as the in-process path does.
func TestRegistryFromManifest_RoundTripsPageTag(t *testing.T) {
	reg := registryFromManifest(client.Manifest{Endpoints: []client.EndpointInfo{
		{Service: "users", Transport: "rest", Method: "GET", Path: "/users", Name: "usersPage", Page: "Users/Index"},
		{Service: "users", Transport: "rest", Method: "POST", Path: "/login", Name: "login", AuthFlow: "login", Page: "Login"},
		{Service: "users", Transport: "rest", Method: "GET", Path: "/api/users", Name: "listUsers"},
	}})
	got := map[string]map[string]string{}
	for _, e := range reg.Endpoints() {
		got[e.Name] = e.Tags
	}
	if got["usersPage"][registry.PageTag] != "Users/Index" {
		t.Errorf("usersPage tags = %v, want page Users/Index", got["usersPage"])
	}
	if got["login"][registry.PageTag] != "Login" || got["login"]["auth.flow"] != "login" {
		t.Errorf("login tags = %v, want both page and auth.flow", got["login"])
	}
	if got["listUsers"] != nil {
		t.Errorf("plain endpoint gained tags: %v", got["listUsers"])
	}
}

// TestClientCmd_WritesInertiaDTS: a manifest with a page makes `nexus client`
// write inertia.d.ts and type the page in client.d.ts; one without pages
// writes no inertia.d.ts.
func TestClientCmd_WritesInertiaDTS(t *testing.T) {
	run := func(manifest string) string {
		dir := t.TempDir()
		in := filepath.Join(dir, "in.json")
		if err := os.WriteFile(in, []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, "sdk")
		var stdout, stderr bytes.Buffer
		cmd := newClientCmd(&stdout, &stderr)
		cmd.SetArgs([]string{"--out", out, "--manifest", in})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v\nstderr: %s", err, stderr.String())
		}
		return out
	}

	out := run(`{"version":"client.v1","basePath":"",
		"endpoints":[{"service":"users","transport":"rest","method":"GET","path":"/users",
			"name":"usersPage","page":"Users/Index","return":{"kind":"any"}}],
		"sharedProps":{"can":{"kind":"map","keyOf":{"kind":"primitive","primitive":"string"},"of":{"kind":"primitive","primitive":"boolean"}}}}`)
	if b := readFile(t, filepath.Join(out, "inertia.d.ts")); !bytes.Contains(b, []byte("sharedPageProps: NexusSharedProps")) {
		t.Errorf("inertia.d.ts content:\n%s", b)
	}
	dts := string(readFile(t, filepath.Join(out, "client.d.ts")))
	for _, want := range []string{"'Users/Index': { [key: string]: unknown }", "can?: Record<string, boolean>"} {
		if !strings.Contains(dts, want) {
			t.Errorf("client.d.ts missing %q", want)
		}
	}
	if strings.Contains(dts, "'GET /users'") {
		t.Error("page emitted in RestEndpoints")
	}

	plain := run(`{"version":"client.v1","basePath":"","endpoints":[
		{"service":"pets","transport":"rest","method":"GET","path":"/pets","name":"listPets"}]}`)
	if _, err := os.Stat(filepath.Join(plain, "inertia.d.ts")); !os.IsNotExist(err) {
		t.Errorf("inertia.d.ts written for a manifest without pages (stat err %v)", err)
	}
}
