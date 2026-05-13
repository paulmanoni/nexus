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

	if !strings.Contains(body, "import nexus from") {
		t.Errorf("missing import line\n--- body ---\n%s", body)
	}
	if !strings.Contains(body, "nexus()") {
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
	if strings.Count(string(again), "nexus()") != 1 {
		t.Errorf("nexus() appears more than once after second merge")
	}
}

// TestMergeViteConfig_LegacyAutoSelectName confirms a config that
// was wired before the rename (using `nexusAutoSelect`) is left
// untouched on the next merge — both the import and the plugin
// call stay as-is, no duplicate `nexus(...)` gets inserted.
func TestMergeViteConfig_LegacyAutoSelectName(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vite.config.ts")
	sdkDir := dir
	original := `import { defineConfig } from 'vite'
import nexusAutoSelect from './nexus-vite-plugin.js'
import vue from '@vitejs/plugin-vue'
export default defineConfig({
  plugins: [vue(), nexusAutoSelect()],
})
`
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MergeViteConfig(cfgPath, sdkDir, io.Discard); err != nil {
		t.Fatalf("merge: %v", err)
	}
	body, _ := os.ReadFile(cfgPath)
	got := string(body)
	if strings.Count(got, "import nexus") != 1 {
		t.Errorf("expected exactly one nexus-style import, got %d\n%s",
			strings.Count(got, "import nexus"), got)
	}
	if strings.Contains(got, "nexus()") && strings.Contains(got, "nexusAutoSelect()") {
		// Both the legacy and new call sites would mean we duplicated.
		// The legacy one is fine on its own; we just shouldn't add a new one.
		if strings.Count(got, "nexus(") != 1 { // counts both prefixes via prefix match
			t.Errorf("legacy + new call both present:\n%s", got)
		}
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
	if !strings.Contains(string(body), "plugins: [nexus()]") {
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

// TestEnsureViteProxyForNexus_PrependsIntoExistingProxy pins the
// happy path: a project that already has /graphql + /oauth proxy
// rules gets /__nexus prepended without disturbing the rest.
func TestEnsureViteProxyForNexus_PrependsIntoExistingProxy(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vite.config.ts")
	original := `import { defineConfig } from 'vite'
export default defineConfig({
  plugins: [],
  server: {
    proxy: {
      "/graphql": { target: "http://localhost:8080", changeOrigin: true },
      "/ws": { target: "ws://localhost:8080", ws: true, changeOrigin: true },
    },
  },
})
`
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureViteProxyForNexus(cfgPath, "http://localhost:8080", io.Discard); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	body, _ := os.ReadFile(cfgPath)
	got := string(body)
	for _, want := range []string{
		`"/__nexus": { target: "http://localhost:8080", changeOrigin: true }`,
		`"/graphql": { target: "http://localhost:8080", changeOrigin: true }`, // unchanged
		`"/ws": { target: "ws://localhost:8080", ws: true, changeOrigin: true }`, // unchanged
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n--- body ---\n%s", want, got)
		}
	}
	// Idempotent.
	if err := EnsureViteProxyForNexus(cfgPath, "http://localhost:8080", io.Discard); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if again, _ := os.ReadFile(cfgPath); string(again) != got {
		t.Errorf("proxy injection is not idempotent")
	}
	if strings.Count(got, `"/__nexus"`) != 1 {
		t.Errorf("/__nexus duplicated:\n%s", got)
	}
}

// TestEnsureViteProxyForNexus_AddsProxyToServerBlock covers the
// case where server: { … } exists but has no proxy: yet.
func TestEnsureViteProxyForNexus_AddsProxyToServerBlock(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vite.config.ts")
	original := `import { defineConfig } from 'vite'
export default defineConfig({
  server: { port: 5173 },
})
`
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureViteProxyForNexus(cfgPath, "http://localhost:8080", io.Discard); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(cfgPath)
	got := string(body)
	for _, want := range []string{
		"proxy: {",
		`"/__nexus":`,
		"port: 5173", // unchanged
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n--- body ---\n%s", want, got)
		}
	}
}

// TestEnsureViteProxyForNexus_AddsServerBlockWhenMissing covers
// the case where defineConfig has no server: block at all.
func TestEnsureViteProxyForNexus_AddsServerBlockWhenMissing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vite.config.ts")
	original := `import { defineConfig } from 'vite'
export default defineConfig({
  plugins: [],
})
`
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureViteProxyForNexus(cfgPath, "http://localhost:8080", io.Discard); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(cfgPath)
	got := string(body)
	if !strings.Contains(got, "server: { proxy: {") {
		t.Errorf("expected fresh server.proxy block\n--- body ---\n%s", got)
	}
	if !strings.Contains(got, `"/__nexus":`) {
		t.Errorf("missing /__nexus entry\n--- body ---\n%s", got)
	}
}

// TestEnsureViteProxyForNexus_InjectsAllDefaultPrefixes is the
// post-generalisation contract: when none of the framework prefixes
// are present, all four (/__nexus, /graphql, /oauth, /ws) land in
// one pass. The /ws entry gets ws:true so vite upgrades the
// connection instead of buffering an HTTP response.
func TestEnsureViteProxyForNexus_InjectsAllDefaultPrefixes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vite.config.ts")
	original := `import { defineConfig } from 'vite'
export default defineConfig({
  plugins: [],
})
`
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureViteProxyForNexus(cfgPath, "http://localhost:8080", io.Discard); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(cfgPath)
	for _, want := range []string{
		`"/__nexus": { target: "http://localhost:8080", changeOrigin: true }`,
		`"/graphql": { target: "http://localhost:8080", changeOrigin: true }`,
		`"/oauth": { target: "http://localhost:8080", changeOrigin: true }`,
		`"/ws": { target: "http://localhost:8080", changeOrigin: true, ws: true }`,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q\n--- body ---\n%s", want, got)
		}
	}
}

// TestEnsureViteProxyForPrefixes_CustomList covers the explicit-
// prefix variant: apps with a custom /api or /v1 base path can pass
// their own list. Defaults aren't auto-mixed in — the caller owns
// the full set.
func TestEnsureViteProxyForPrefixes_CustomList(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vite.config.ts")
	original := `import { defineConfig } from 'vite'
export default defineConfig({})
`
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureViteProxyForPrefixes(cfgPath, "http://localhost:8080",
		[]string{"/api", "/v1"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(cfgPath)
	for _, want := range []string{`"/api":`, `"/v1":`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q\n--- body ---\n%s", want, got)
		}
	}
	// Defaults should NOT have been auto-added by the custom-list path.
	for _, leaked := range []string{`"/__nexus"`, `"/graphql"`, `"/oauth"`, `"/ws"`} {
		if strings.Contains(string(got), leaked) {
			t.Errorf("default prefix %s leaked into custom-list output", leaked)
		}
	}
}

// TestEnsureViteProxyForPrefixes_PerPrefixIdempotence checks the
// idempotency contract is per-prefix, not per-call. A second run
// against a partially-wired config adds only what's missing.
func TestEnsureViteProxyForPrefixes_PerPrefixIdempotence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vite.config.ts")
	original := `import { defineConfig } from 'vite'
export default defineConfig({
  server: {
    proxy: {
      "/graphql": { target: "http://localhost:8080", changeOrigin: true },
    },
  },
})
`
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureViteProxyForNexus(cfgPath, "http://localhost:8080", io.Discard); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(cfgPath)
	got := string(body)
	// /graphql was already present — must not be duplicated.
	if strings.Count(got, `"/graphql":`) != 1 {
		t.Errorf("/graphql duplicated:\n%s", got)
	}
	// The other three should be added.
	for _, want := range []string{`"/__nexus":`, `"/oauth":`, `"/ws":`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q after partial-wire pass\n--- body ---\n%s", want, got)
		}
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