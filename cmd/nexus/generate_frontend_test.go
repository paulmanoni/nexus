package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/client"
	"github.com/paulmanoni/nexus/registry"
)

// fixtureManifestJSON writes a minimal client.Manifest to disk and
// returns its path. The shape exercises the three transports the
// renderer differentiates so the smoke test catches a regression in
// any of them.
func fixtureManifestJSON(t *testing.T) string {
	t.Helper()
	m := client.Manifest{
		Version:  client.SchemaVersion,
		BasePath: "",
		Endpoints: []client.EndpointInfo{
			{
				Service:   "users",
				Name:      "listUsers",
				Transport: "graphql",
				Method:    "query",
				Return:    &registry.TypeRef{Kind: "ref", Ref: "User"},
			},
			{
				Service:   "users",
				Name:      "createUser",
				Transport: "graphql",
				Method:    "mutation",
				Args: &registry.TypeRef{Kind: "object", Object: &registry.NamedType{Fields: []registry.FieldSchema{
					{Name: "Email", JSONName: "email", Type: registry.TypeRef{Kind: "primitive", Primitive: "string"}},
				}}},
				Return: &registry.TypeRef{Kind: "ref", Ref: "User"},
			},
			{
				Service:   "health",
				Name:      "getHealth",
				Transport: "rest",
				Method:    "GET",
				Path:      "/health",
				Return:    &registry.TypeRef{Kind: "primitive", Primitive: "string"},
			},
		},
		Refs: map[string]registry.NamedType{
			"User": {Fields: []registry.FieldSchema{
				{Name: "ID", JSONName: "id", Type: registry.TypeRef{Kind: "primitive", Primitive: "string"}},
				{Name: "Email", JSONName: "email", Type: registry.TypeRef{Kind: "primitive", Primitive: "string"}},
			}},
		},
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func TestGenerateFrontend_WritesAllFiles(t *testing.T) {
	manifest := fixtureManifestJSON(t)
	outDir := t.TempDir()

	var stdout, stderr bytes.Buffer
	err := runGenerateFrontend(frontendOptions{
		Manifest:  manifest,
		Out:       outDir,
		Framework: "vue",
		Root:      "web",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runGenerateFrontend: %v (stderr: %s)", err, stderr.String())
	}

	for _, name := range []string{"_client.ts", "types.ts", "index.ts", "vue.ts"} {
		body, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if len(body) == 0 {
			t.Fatalf("%s is empty", name)
		}
	}

	idx, _ := os.ReadFile(filepath.Join(outDir, "index.ts"))
	if !strings.Contains(string(idx), "export const listUsers") {
		t.Fatalf("index.ts missing listUsers export:\n%s", idx)
	}
	if !strings.Contains(string(idx), "export const createUser") {
		t.Fatalf("index.ts missing createUser export")
	}
	if !strings.Contains(string(idx), "export const getHealth") {
		t.Fatalf("index.ts missing getHealth REST export")
	}

	types, _ := os.ReadFile(filepath.Join(outDir, "types.ts"))
	if !strings.Contains(string(types), "export interface User") {
		t.Fatalf("types.ts missing User interface:\n%s", types)
	}

	vue, _ := os.ReadFile(filepath.Join(outDir, "vue.ts"))
	if !strings.Contains(string(vue), "useListUsers") {
		t.Fatalf("vue.ts missing useListUsers composable")
	}
	if !strings.Contains(string(vue), "useCreateUser") {
		t.Fatalf("vue.ts missing useCreateUser composable")
	}
}

// TestGenerateFrontend_NoneFrameworkSkipsVue ensures --framework=none
// emits only the transport-neutral trio, not a stale vue.ts that
// would have referenced 'vue' imports nobody installed.
func TestGenerateFrontend_NoneFrameworkSkipsVue(t *testing.T) {
	manifest := fixtureManifestJSON(t)
	outDir := t.TempDir()

	err := runGenerateFrontend(frontendOptions{
		Manifest:  manifest,
		Out:       outDir,
		Framework: "none",
		Root:      "web",
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "vue.ts")); !os.IsNotExist(err) {
		t.Fatalf("vue.ts should not exist for framework=none — err=%v", err)
	}
}

// TestGenerateFrontend_CheckMatches succeeds on a freshly-generated
// tree — the CI guard's happy path.
func TestGenerateFrontend_CheckMatches(t *testing.T) {
	manifest := fixtureManifestJSON(t)
	outDir := t.TempDir()

	// First pass writes the tree.
	if err := runGenerateFrontend(frontendOptions{
		Manifest:  manifest,
		Out:       outDir,
		Framework: "vue",
		Root:      "web",
	}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	// Second pass with --check should be a clean exit.
	var stdout, stderr bytes.Buffer
	err := runGenerateFrontend(frontendOptions{
		Manifest:  manifest,
		Out:       outDir,
		Framework: "vue",
		Root:      "web",
		Check:     true,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("--check on unmodified tree: %v (stderr: %s)", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ok") {
		t.Fatalf("--check ok message missing: %q", stdout.String())
	}
}

// TestGenerateFrontend_CheckDetectsDrift is the CI guard's whole
// point — a manual edit of the generated tree should fail --check.
func TestGenerateFrontend_CheckDetectsDrift(t *testing.T) {
	manifest := fixtureManifestJSON(t)
	outDir := t.TempDir()

	if err := runGenerateFrontend(frontendOptions{
		Manifest:  manifest,
		Out:       outDir,
		Framework: "vue",
		Root:      "web",
	}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	// Tamper.
	idxPath := filepath.Join(outDir, "index.ts")
	if err := os.WriteFile(idxPath, []byte("// manually edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	err := runGenerateFrontend(frontendOptions{
		Manifest:  manifest,
		Out:       outDir,
		Framework: "vue",
		Root:      "web",
		Check:     true,
	}, io.Discard, &stderr)
	if err == nil {
		t.Fatal("--check on tampered tree should have failed")
	}
	if !strings.Contains(stderr.String(), "index.ts") {
		t.Fatalf("drift report missing index.ts: %q", stderr.String())
	}
}

// TestGenerateFrontend_RequiresSource is the foot-gun guard: neither
// --manifest nor --url makes the command meaningless.
func TestGenerateFrontend_RequiresSource(t *testing.T) {
	err := runGenerateFrontend(frontendOptions{Out: t.TempDir()}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("missing-source err = %v, want mention of --manifest/--url", err)
	}
}

// TestGenerateFrontend_RejectsBothSources blocks the ambiguous "both
// flags set" case — silently preferring one would surprise a user
// pointing at a stale file while a fresh server is running.
func TestGenerateFrontend_RejectsBothSources(t *testing.T) {
	err := runGenerateFrontend(frontendOptions{
		Manifest: fixtureManifestJSON(t),
		URL:      "http://localhost:8080",
		Out:      t.TempDir(),
	}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("both-source err = %v, want 'mutually exclusive'", err)
	}
}

// TestGenerateFrontend_UnknownFrameworkRejected catches typos at
// flag-parse time rather than producing a confusing partial output.
func TestGenerateFrontend_UnknownFrameworkRejected(t *testing.T) {
	err := runGenerateFrontend(frontendOptions{
		Manifest:  fixtureManifestJSON(t),
		Out:       t.TempDir(),
		Framework: "ember",
		Root:      "web",
	}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not recognised") {
		t.Fatalf("unknown framework err = %v", err)
	}
}
