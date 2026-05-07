package client

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeViteConfig_AddsImportAndPluginEntry(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vite.config.ts")
	sdkDir := filepath.Join(dir, "src", "sdk")
	if err := os.MkdirAll(sdkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	original := `import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
})
`
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MergeViteConfig(cfgPath, sdkDir, io.Discard); err != nil {
		t.Fatalf("merge: %v", err)
	}
	out, _ := os.ReadFile(cfgPath)
	body := string(out)

	if !strings.Contains(body, "import nexusAutoSelect from") {
		t.Errorf("missing import line\n--- body ---\n%s", body)
	}
	if !strings.Contains(body, "nexusAutoSelect()") {
		t.Errorf("plugins entry missing\n--- body ---\n%s", body)
	}
	if !strings.Contains(body, "vue()") {
		t.Errorf("existing vue() plugin clobbered\n--- body ---\n%s", body)
	}
	// Idempotence: second run is a no-op.
	if err := MergeViteConfig(cfgPath, sdkDir, io.Discard); err != nil {
		t.Fatalf("second merge: %v", err)
	}
	again, _ := os.ReadFile(cfgPath)
	if string(again) != body {
		t.Errorf("merge is not idempotent")
	}
	// Make sure we didn't double up.
	if strings.Count(string(again), "nexusAutoSelect()") != 1 {
		t.Errorf("nexusAutoSelect() appears more than once after second merge")
	}
}

func TestMergeViteConfig_EmptyPluginsArray(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vite.config.ts")
	if err := os.WriteFile(cfgPath, []byte(
		`import { defineConfig } from 'vite'
export default defineConfig({
  plugins: [],
})
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MergeViteConfig(cfgPath, dir, io.Discard); err != nil {
		t.Fatalf("merge: %v", err)
	}
	body, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(body), "plugins: [nexusAutoSelect()]") {
		t.Errorf("entry not inserted into empty array\n--- body ---\n%s", body)
	}
}

// TestEnsureViteWatchExclude_StandaloneInjection verifies the helper
// the dev CLI calls before spawning vite — it should patch the
// config independently of the plugin-wiring path so vite reads the
// fix on first boot.
func TestEnsureViteWatchExclude_StandaloneInjection(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vite.config.ts")
	original := `import { defineConfig } from 'vite'
export default defineConfig({
  plugins: [],
  build: { outDir: "dist" },
})
`
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureViteWatchExclude(cfgPath, io.Discard); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	body, _ := os.ReadFile(cfgPath)
	got := string(body)
	if !strings.Contains(got, "watch: { exclude:") {
		t.Errorf("expected watch.exclude\n--- body ---\n%s", got)
	}
	if !strings.Contains(got, "'**/auto-imports.d.ts'") {
		t.Errorf("expected auto-imports glob\n--- body ---\n%s", got)
	}
	// Standalone helper should NOT inject the plugin import — that's
	// MergeViteConfig's job and runs later from Go's auto-dump.
	if strings.Contains(got, "nexusAutoSelect") {
		t.Errorf("EnsureViteWatchExclude leaked plugin wiring\n--- body ---\n%s", got)
	}
	// Idempotent.
	if err := EnsureViteWatchExclude(cfgPath, io.Discard); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	again, _ := os.ReadFile(cfgPath)
	if string(again) != got {
		t.Errorf("EnsureViteWatchExclude is not idempotent")
	}
}

// TestEnsureViteWatchExclude_MissingFile silently no-ops so a
// project without a vite config (or with one in a non-conventional
// location) doesn't fail the dev loop.
func TestEnsureViteWatchExclude_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureViteWatchExclude(filepath.Join(dir, "absent.config.ts"), io.Discard); err != nil {
		t.Fatalf("expected nil for missing config, got %v", err)
	}
}

// TestMergeViteConfig_InjectsWatchExcludeIntoExistingBuild verifies
// the auto-fix for the unplugin-auto-import / @nuxt/ui rebuild loop:
// when the user's config already has a build: { … } block, we
// prepend watch: { exclude: [...] } without disturbing the rest.
func TestMergeViteConfig_InjectsWatchExcludeIntoExistingBuild(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vite.config.ts")
	original := `import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
})
`
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MergeViteConfig(cfgPath, dir, io.Discard); err != nil {
		t.Fatalf("merge: %v", err)
	}
	body, _ := os.ReadFile(cfgPath)
	got := string(body)
	for _, want := range []string{
		"watch: { exclude:",
		"'**/auto-imports.d.ts'",
		"'**/components.d.ts'",
		`outDir: "dist"`,    // unchanged
		`emptyOutDir: true`, // unchanged
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in output\n--- body ---\n%s", want, got)
		}
	}
	// Idempotency: second run must not duplicate the entry.
	if err := MergeViteConfig(cfgPath, dir, io.Discard); err != nil {
		t.Fatalf("second merge: %v", err)
	}
	again, _ := os.ReadFile(cfgPath)
	if string(again) != got {
		t.Errorf("watch-exclude injection is not idempotent")
	}
	if strings.Count(string(again), "auto-imports.d.ts") != 1 {
		t.Errorf("auto-imports.d.ts appears more than once after second merge")
	}
}

// TestMergeViteConfig_AddsBuildBlockWhenMissing verifies the second
// branch: a config without any build: block gets a freshly-formed
// build: { watch: { exclude: [...] } } added inside defineConfig.
func TestMergeViteConfig_AddsBuildBlockWhenMissing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vite.config.ts")
	original := `import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
})
`
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MergeViteConfig(cfgPath, dir, io.Discard); err != nil {
		t.Fatalf("merge: %v", err)
	}
	body, _ := os.ReadFile(cfgPath)
	got := string(body)
	if !strings.Contains(got, "build: { watch: { exclude:") {
		t.Errorf("expected fresh build block with watch.exclude\n--- body ---\n%s", got)
	}
	if !strings.Contains(got, "'**/auto-imports.d.ts'") {
		t.Errorf("expected auto-imports glob\n--- body ---\n%s", got)
	}
}

func TestMergeViteConfig_NoPluginsArrayLeavesFileAlone(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vite.config.ts")
	original := `import { defineConfig } from 'vite'
export default defineConfig({
  build: { sourcemap: true },
})
`
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	// Should not error, but also not insert into a plugins array
	// (the file has none); the import is still added so the user
	// only has to add the plugins entry.
	if err := MergeViteConfig(cfgPath, dir, io.Discard); err != nil {
		t.Fatalf("merge: %v", err)
	}
	body, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(body), "nexusAutoSelect()") {
		t.Errorf("should not have invented a plugins array\n--- body ---\n%s", body)
	}
}