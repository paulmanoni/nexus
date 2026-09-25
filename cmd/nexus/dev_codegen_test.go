package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/client"
	"github.com/paulmanoni/nexus/registry"
)

// fakeNexusServer stands up the three routes the dev codegen probes:
// /__nexus/plugins, /__nexus/client/manifest.json, and (optionally)
// /__nexus/client/contributions.json. The handler is built so tests
// can flip whether the frontend plugin is "registered" by toggling
// the include flag, and so they can vary the manifest/contributions
// bodies to exercise different branches without spinning a full app.
func fakeNexusServer(t *testing.T, includeFrontend bool, contributions string) *httptest.Server {
	t.Helper()
	m := client.Manifest{
		Version: client.SchemaVersion,
		Endpoints: []client.EndpointInfo{
			{
				Service:   "users",
				Name:      "listUsers",
				Transport: "graphql",
				Method:    "query",
				Return:    &registry.TypeRef{Kind: "ref", Ref: "User"},
			},
		},
		Refs: map[string]registry.NamedType{
			"User": {Fields: []registry.FieldSchema{{Name: "ID", JSONName: "id", Type: registry.TypeRef{Kind: "primitive", Primitive: "string"}}}},
		},
	}
	mb, _ := json.Marshal(m)

	mux := http.NewServeMux()
	mux.HandleFunc("/__nexus/plugins", func(w http.ResponseWriter, _ *http.Request) {
		plugins := []map[string]any{{"name": "auth"}}
		if includeFrontend {
			plugins = append(plugins, map[string]any{"name": "frontend"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"plugins": plugins})
	})
	mux.HandleFunc("/__nexus/client/manifest.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(mb)
	})
	mux.HandleFunc("/__nexus/client/contributions.json", func(w http.ResponseWriter, _ *http.Request) {
		if contributions == "" {
			http.NotFound(w, nil)
			return
		}
		w.Write([]byte(contributions))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestDevRunCodegen_RendersWhenFrontendPluginPresent covers the
// happy path: a running app reports frontend.Plugin via /plugins,
// the dev codegen renders the typed surface and writes into
// <frontendDir>/src/__nexus/. The default Vue framework selection
// is what nexus dev passes today.
func TestDevRunCodegen_RendersWhenFrontendPluginPresent(t *testing.T) {
	srv := fakeNexusServer(t, true, "")
	dir := t.TempDir()

	var out, errBuf bytes.Buffer
	err := devRunCodegen(context.Background(), srv.URL, dir, "vue", &out, &errBuf)
	if err != nil {
		t.Fatalf("devRunCodegen: %v (stderr: %s)", err, errBuf.String())
	}

	outDir := filepath.Join(dir, "src", "__nexus")
	for _, name := range []string{"_client.ts", "types.ts", "index.ts", "vue.ts"} {
		body, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Errorf("missing %s in output: %v", name, err)
			continue
		}
		if len(body) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
	if !strings.Contains(out.String(), "frontend codegen") {
		t.Errorf("expected summary line; got %q", out.String())
	}
}

// TestDevRunCodegen_SilentSkipWhenFrontendAbsent is the
// non-frontend.Plugin app case. The plugins endpoint exists but
// doesn't list "frontend". The codegen must skip silently — no
// files, no error log, no stdout noise.
func TestDevRunCodegen_SilentSkipWhenFrontendAbsent(t *testing.T) {
	srv := fakeNexusServer(t, false, "")
	dir := t.TempDir()

	var out, errBuf bytes.Buffer
	err := devRunCodegen(context.Background(), srv.URL, dir, "vue", &out, &errBuf)
	if err != nil {
		t.Fatalf("devRunCodegen: %v", err)
	}
	// Output dir must not have been created — the skip happens
	// before MkdirAll.
	if _, err := os.Stat(filepath.Join(dir, "src", "__nexus")); !os.IsNotExist(err) {
		t.Errorf("output dir created despite skip; stat err = %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("silent skip should produce no stdout; got %q", out.String())
	}
	if errBuf.Len() != 0 {
		t.Errorf("silent skip should produce no stderr; got %q", errBuf.String())
	}
}

// TestDevRunCodegen_MergesContributions wires the contributions
// route end-to-end: the fake server returns one plugin's TS, the
// dev codegen merges it into the output tree.
func TestDevRunCodegen_MergesContributions(t *testing.T) {
	body := `{
  "version": "client.v1",
  "framework": "vue",
  "plugins": [
    {
      "name": "auth",
      "files": [{"path": "auth/vue.ts", "body": "// stub useAuth\n"}]
    }
  ]
}`
	srv := fakeNexusServer(t, true, body)
	dir := t.TempDir()

	err := devRunCodegen(context.Background(), srv.URL, dir, "vue", &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "src", "__nexus", "auth", "vue.ts"))
	if err != nil {
		t.Fatalf("expected auth/vue.ts in output: %v", err)
	}
	if !strings.Contains(string(got), "stub useAuth") {
		t.Fatalf("contributor body lost in merge:\n%s", got)
	}
}

// TestDevRunCodegen_NoopOnSecondRunPreservesMtime is the dev-loop
// invariant: identical manifest → identical bytes → byte-equal
// writer skips → mtime unchanged. Without this, every Go restart
// would bump mtimes on every generated file and chum the IDE's
// file watcher into a reindex loop.
func TestDevRunCodegen_NoopOnSecondRunPreservesMtime(t *testing.T) {
	srv := fakeNexusServer(t, true, "")
	dir := t.TempDir()

	if err := devRunCodegen(context.Background(), srv.URL, dir, "vue", &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(dir, "src", "__nexus", "_client.ts")
	info1, err := os.Stat(clientPath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := devRunCodegen(context.Background(), srv.URL, dir, "vue", &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	info2, err := os.Stat(clientPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatalf("mtime changed on identical re-render: %v → %v", info1.ModTime(), info2.ModTime())
	}
}

// TestDevCodegenWatch_EmptyFrontendDirReturnsImmediately is the
// pre-check that keeps the probe loop from waiting 30s in the
// "user didn't pass --frontend" case.
func TestDevCodegenWatch_EmptyFrontendDirReturnsImmediately(t *testing.T) {
	done := make(chan struct{})
	go func() {
		devCodegenWatch(context.Background(), ":8080", "", "vue", &bytes.Buffer{}, &bytes.Buffer{})
		close(done)
	}()
	select {
	case <-done:
		// good
	case <-time.After(1 * time.Second):
		t.Fatal("devCodegenWatch blocked on empty frontendDir; should return immediately")
	}
}
