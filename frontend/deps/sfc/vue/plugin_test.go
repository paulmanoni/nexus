package vue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
)

// TestPlugin_EndToEndBundleVueFile is the smoke proof that the
// SFC plugin integrates with esbuild correctly: a tiny .vue file
// goes in, an ES module bundle comes out — without ANY Node side.
func TestPlugin_EndToEndBundleVueFile(t *testing.T) {
	tmp := t.TempDir()
	entry := filepath.Join(tmp, "Counter.vue")
	if err := os.WriteFile(entry, []byte(`<template>
  <p>Hello {{ name }}</p>
</template>
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Build the compiler with the fake adapter (production code
	// would use the real @vue/compiler-sfc bundle).
	adapter := loadFakeAdapter(t)
	c, err := NewCompiler(adapter, "fake")
	if err != nil {
		t.Fatal(err)
	}
	plugin, err := Plugin(c)
	if err != nil {
		t.Fatal(err)
	}

	outFile := filepath.Join(tmp, "out.js")
	res := api.Build(api.BuildOptions{
		EntryPoints: []string{entry},
		Outfile:     outFile,
		Bundle:      true,
		Write:       true,
		Format:      api.FormatESModule,
		Plugins:     []api.Plugin{plugin},
		LogLevel:    api.LogLevelSilent,
	})
	if len(res.Errors) != 0 {
		t.Fatalf("build errors: %+v", res.Errors)
	}
	out, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	bundle := string(out)
	// The fake adapter ships the template body verbatim inside
	// a JSON-encoded string; assert it survived bundling.
	if !strings.Contains(bundle, "Hello {{ name }}") {
		t.Errorf("bundle missing compiled template; got:\n%s", bundle)
	}
	if !strings.Contains(bundle, "Counter.vue") {
		t.Errorf("bundle missing source filename trace; got:\n%s", bundle)
	}
}

func TestPlugin_NilCompilerErrors(t *testing.T) {
	if _, err := Plugin(nil); err == nil {
		t.Error("Plugin(nil) should error")
	}
}

func TestPlugin_CompileErrorsSurfaceAsBuildFailures(t *testing.T) {
	tmp := t.TempDir()
	entry := filepath.Join(tmp, "Broken.vue")
	// The fake adapter triggers an error when the source starts
	// with "BOOM" — easy way to drive the error path without
	// inventing a real compile failure.
	if err := os.WriteFile(entry, []byte("BOOM bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := NewCompiler(loadFakeAdapter(t), "fake")
	if err != nil {
		t.Fatal(err)
	}
	plugin, err := Plugin(c)
	if err != nil {
		t.Fatal(err)
	}
	res := api.Build(api.BuildOptions{
		EntryPoints: []string{entry},
		Outfile:     filepath.Join(tmp, "out.js"),
		Bundle:      true,
		Write:       false,
		Format:      api.FormatESModule,
		Plugins:     []api.Plugin{plugin},
		LogLevel:    api.LogLevelSilent,
	})
	if len(res.Errors) == 0 {
		t.Fatal("expected build errors from BOOM-tagged compile")
	}
	if !strings.Contains(res.Errors[0].Text, "synthetic test error") {
		t.Errorf("error text = %q", res.Errors[0].Text)
	}
}
