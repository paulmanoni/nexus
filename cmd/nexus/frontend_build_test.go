package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mockEsm spins up the same kind of httptest registry the deps
// CLI tests use, plus the routes needed by the frontend-build
// pipeline. Reused-by-import would create a test-package cycle;
// duplicate is cheap.
func mockEsmForFrontend(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/htm", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/htm@3.1.1", http.StatusFound)
	})
	mux.HandleFunc("/htm@3.1.1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(
			`export default { bind(fn) { return (strings, ...values) => fn("tag", null, strings, ...values); } };`,
		))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestFrontendBuild_EndToEnd_AddThenBundle is the headline proof:
// nexus add pulls a fake "htm" from the mock registry, frontendBuild
// scans islands.src + bundles the user's TS file against the cached
// blob, writes a self-contained ESM bundle to islands/.
func TestFrontendBuild_EndToEnd_AddThenBundle(t *testing.T) {
	srv := mockEsmForFrontend(t)
	cwd, cleanup := inDepsTestSandbox(t, srv.URL)
	defer cleanup()

	// Step 1: nexus add htm (populates lockfile + cache).
	if err := runAdd(context.Background(), discard(), discard(), []string{"htm"}); err != nil {
		t.Fatalf("runAdd: %v", err)
	}

	// Step 2: stage a frontend entry under islands.src.
	if err := os.MkdirAll(filepath.Join(cwd, "islands.src"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(cwd, "islands.src", "Counter.ts")
	if err := os.WriteFile(entry, []byte(
		`import htm from "htm";
const h = htm.bind((tag: string) => tag);
console.log(h);
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Step 3: run the frontend build step directly. (`nexus build`
	// orchestrates this + go build + embed-gen; we exercise just
	// the frontend layer here.)
	var stdout, stderr bytes.Buffer
	if err := frontendBuild(cwd, &stdout, &stderr); err != nil {
		t.Fatalf("frontendBuild: %v\nstderr: %s", err, stderr.String())
	}

	// Step 4: verify a bundle landed in islands/.
	out, err := os.ReadFile(filepath.Join(cwd, "islands", "Counter.js"))
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	bundle := string(out)
	if !strings.Contains(bundle, "bind") {
		t.Errorf("bundle missing htm bind impl; got:\n%s", bundle)
	}
}

func TestFrontendBuild_NoIslandsSrcIsNoOp(t *testing.T) {
	cwd, cleanup := inDepsTestSandbox(t, "http://127.0.0.1:0")
	defer cleanup()

	var stdout, stderr bytes.Buffer
	if err := frontendBuild(cwd, &stdout, &stderr); err != nil {
		t.Errorf("frontendBuild on a project without islands.src should be a no-op, got: %v", err)
	}
	// No bundle written.
	if _, err := os.Stat(filepath.Join(cwd, "islands")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("islands dir should not have been created")
	}
}

func TestFrontendBuild_EmptyIslandsSrcIsNoOp(t *testing.T) {
	cwd, cleanup := inDepsTestSandbox(t, "http://127.0.0.1:0")
	defer cleanup()
	if err := os.MkdirAll(filepath.Join(cwd, "islands.src"), 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := frontendBuild(cwd, &stdout, &stderr); err != nil {
		t.Errorf("empty islands.src should be no-op, got: %v", err)
	}
}

// TestFrontendBuild_DotVueProducesClearError checks the rejection
// path .vue takes when the binary was built WITHOUT the Vue SFC
// compiler (default pure-Go build — vueCompilerHook is nil).
// When -tags vue is passed, the hook is wired, .vue compiles
// normally, and this test wouldn't be meaningful — so it's gated
// off the vue tag.
func TestFrontendBuild_DotVueProducesClearError(t *testing.T) {
	if vueCompilerHook != nil {
		t.Skip("vue tag enabled — .vue compiles instead of rejecting")
	}
	cwd, cleanup := inDepsTestSandbox(t, "http://127.0.0.1:0")
	defer cleanup()
	if err := os.MkdirAll(filepath.Join(cwd, "islands.src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "islands.src", "Foo.vue"), []byte(`<template>x</template>`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := frontendBuild(cwd, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected .vue rejection")
	}
	if !strings.Contains(err.Error(), "built without Vue SFC support") {
		t.Errorf("err missing build-tag guidance: %v", err)
	}
}

func TestFrontendBuild_MissingLockfileReportsClearly(t *testing.T) {
	cwd, cleanup := inDepsTestSandbox(t, "http://127.0.0.1:0")
	defer cleanup()
	if err := os.MkdirAll(filepath.Join(cwd, "islands.src"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Two bare imports in user code → error should suggest BOTH
	// nexus add commands by name, not a generic hint.
	if err := os.WriteFile(filepath.Join(cwd, "islands.src", "Foo.ts"), []byte(
		`import { ref } from "vue";
import { VueFlow } from "@vue-flow/core";
`), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := frontendBuild(cwd, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected lockfile-missing error")
	}
	msg := err.Error()
	for _, want := range []string{
		"nexus.lock missing",
		"nexus add vue",
		"nexus add @vue-flow/core",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("err missing %q\n--- full msg ---\n%s", want, msg)
		}
	}
}
