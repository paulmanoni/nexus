//go:build cgo
// +build cgo

package vue

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func loadFakeAdapter(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "fake-adapter.js"))
	if err != nil {
		t.Fatalf("read fake adapter: %v", err)
	}
	return b
}

func TestNewCompiler_EmptyBundleErrors(t *testing.T) {
	if _, err := NewCompiler(nil, ""); err == nil {
		t.Error("expected error on nil bundle")
	}
	if _, err := NewCompiler([]byte{}, ""); err == nil {
		t.Error("expected error on empty bundle")
	}
}

func TestNewCompiler_BundleWithoutAdapterErrors(t *testing.T) {
	bundle := []byte(`var x = 1;`)
	_, err := NewCompiler(bundle, "test")
	if err == nil {
		t.Fatal("expected error on bundle missing the adapter global")
	}
	if !strings.Contains(err.Error(), "__nexus_compileSFC") {
		t.Errorf("err = %v, want mention of __nexus_compileSFC", err)
	}
}

func TestCompile_SimpleSFCRoundTrip(t *testing.T) {
	c, err := NewCompiler(loadFakeAdapter(t), "fake-1.0.0")
	if err != nil {
		t.Fatalf("NewCompiler: %v", err)
	}
	defer c.Close()
	src := `<template>
        <h1>Hello, {{ name }}</h1>
    </template>`
	res, err := c.Compile(src, "Counter.vue")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !strings.Contains(res.Code, "export default") {
		t.Errorf("Code missing export; got: %s", res.Code)
	}
	if !strings.Contains(res.Code, "Hello, {{ name }}") {
		t.Errorf("Code missing template body; got: %s", res.Code)
	}
	if !strings.Contains(res.Code, "Counter.vue") {
		t.Errorf("filename not threaded through; got: %s", res.Code)
	}
	if len(res.Errors) != 0 {
		t.Errorf("unexpected errors: %v", res.Errors)
	}
}

func TestCompile_ErrorsArePropagated(t *testing.T) {
	c, err := NewCompiler(loadFakeAdapter(t), "fake-1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := c.Compile("BOOM whatever", "Broken.vue")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("Errors = %d, want 1", len(res.Errors))
	}
	if res.Errors[0].Message != "synthetic test error" {
		t.Errorf("Errors[0].Message = %q", res.Errors[0].Message)
	}
	if res.Errors[0].Line != 1 || res.Errors[0].Column != 1 {
		t.Errorf("Errors[0] line/col = (%d,%d)", res.Errors[0].Line, res.Errors[0].Column)
	}
}

func TestCompile_SerializesConcurrentCalls(t *testing.T) {
	// QuickJS contexts are NOT thread-safe; Compile takes a
	// mutex to serialize. This test fires 16 goroutines at one
	// Compiler — they should all succeed without panics or
	// data races.
	c, err := NewCompiler(loadFakeAdapter(t), "fake-1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const N = 16
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for range N {
		go func() {
			defer wg.Done()
			if _, err := c.Compile(`<template><p>x</p></template>`, "Conc.vue"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent compile failed: %v", err)
	}
}

func TestCompile_AsyncGeneratorPrototypeChainWorks(t *testing.T) {
	// THE headline test: this expression is exactly the one that
	// killed the Goja-based path. QuickJS handles async generators
	// natively, so the prototype chain resolves to a real object
	// instead of stumbling into Object.getPrototypeOf(undefined).
	// If this passes, the upstream blocker that gated Vue SFC
	// compile under Goja is gone.
	bundle := []byte(`
		var ne = Object.getPrototypeOf(
		    Object.getPrototypeOf(async function*(){}).prototype
		);
		globalThis.__nexus_compileSFC = function() {
			return { code: typeof ne, errors: [] };
		};
	`)
	c, err := NewCompiler(bundle, "async-gen-probe")
	if err != nil {
		t.Fatalf("the @babel/parser-style async-gen introspection still fails: %v", err)
	}
	defer c.Close()
	res, err := c.Compile("", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != "object" {
		t.Errorf("introspection returned typeof = %q, want 'object'", res.Code)
	}
}

func TestCompiler_VersionExposed(t *testing.T) {
	c, err := NewCompiler(loadFakeAdapter(t), "vue-compiler@3.4.21")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Version() != "vue-compiler@3.4.21" {
		t.Errorf("Version = %q", c.Version())
	}
}
