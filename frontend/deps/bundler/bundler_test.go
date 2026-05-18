package bundler

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/resolver"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

func TestBuild_RequiresEntries(t *testing.T) {
	b := New()
	_, err := b.Build(Options{})
	if err == nil || !strings.Contains(err.Error(), "entry") {
		t.Errorf("err = %v, want 'entry required'", err)
	}
}

func TestBuild_LockfileWithoutStoreErrors(t *testing.T) {
	b := New()
	_, err := b.Build(Options{
		Entries:  []string{"x"},
		Lockfile: lockfile.New(),
	})
	if err == nil || !strings.Contains(err.Error(), "Store") {
		t.Errorf("err = %v, want 'Store missing' message", err)
	}
}

// TestBuild_EndToEnd_ResolverFromStore is the headline smoke test:
// a user-supplied entry file imports "vue" by bare spec, the
// resolver plugin reads the cached blob from the store, esbuild
// bundles them together, and the output references the bundled
// vue stub by name.
//
// This is the "minimum proof that the whole pipeline works"
// before any UI/CLI gets wired up.
func TestBuild_EndToEnd_ResolverFromStore(t *testing.T) {
	tmp := t.TempDir()

	// 1. Set up store with a tiny vue stub blob.
	s, err := store.New(filepath.Join(tmp, "cache"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	vueBody := []byte(`export default { name: "vue-stub" };`)
	if _, err := s.Put("https://esm.sh/vue@3.4.21", bytes.NewReader(vueBody), "",
		store.Metadata{
			URL:         "https://esm.sh/vue@3.4.21",
			ResolvedURL: "https://esm.sh/vue@3.4.21",
			ContentType: "application/javascript",
		}); err != nil {
		t.Fatalf("store.Put: %v", err)
	}

	// 2. Lockfile pins vue → that URL.
	lf := lockfile.New()
	lf.Add(lockfile.Package{
		Spec:     "vue",
		Version:  "3.4.21",
		Resolved: "https://esm.sh/vue@3.4.21",
	})

	// 3. User entry imports vue by bare spec.
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(srcDir, "app.js")
	if err := os.WriteFile(entry, []byte(
		`import Vue from "vue";
console.log("hello", Vue.name);
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// 4. Wire resolver into bundler and build.
	plugin, err := resolver.New(resolver.Options{Lockfile: lf, Store: s})
	if err != nil {
		t.Fatalf("resolver.New: %v", err)
	}
	b := New()
	b.AddPlugin(plugin)

	outDir := filepath.Join(tmp, "out")
	res, err := b.Build(Options{
		Entries:  []string{entry},
		OutDir:   outDir,
		Lockfile: lf,
		Store:    s,
		Minify:   false,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("build errors: %v", res.Errors)
	}

	// 5. Verify the bundle contains the vue stub inlined.
	out, err := os.ReadFile(filepath.Join(outDir, "app.js"))
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	bundle := string(out)
	if !strings.Contains(bundle, "vue-stub") {
		t.Errorf("bundle missing vue-stub literal; got:\n%s", bundle)
	}
	if !strings.Contains(bundle, "hello") {
		t.Errorf("bundle missing user code; got:\n%s", bundle)
	}
}

func TestBuild_DefaultsApplied(t *testing.T) {
	tmp := t.TempDir()
	entry := filepath.Join(tmp, "noop.js")
	if err := os.WriteFile(entry, []byte(`export const x = 1;`), 0o644); err != nil {
		t.Fatal(err)
	}
	b := New()
	res, err := b.Build(Options{
		Entries: []string{entry},
		OutDir:  filepath.Join(tmp, "out"),
		Minify:  false,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors: %v", res.Errors)
	}
	// File should exist at the requested OutDir/<entry-basename>.
	if _, err := os.Stat(filepath.Join(tmp, "out", "noop.js")); err != nil {
		t.Errorf("expected output file: %v", err)
	}
}
