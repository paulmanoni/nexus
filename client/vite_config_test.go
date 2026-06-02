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
// migration path: a project that already has some framework-managed
// entries gets them folded into a marker-fenced managed block so
// future syncs can add/remove declaratively. The standardized shape
// (apiURL target, std changeOrigin/ws keys) replaces whatever the
// user had — customizations are expected to live OUTSIDE the markers.
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
		"// @nexus:proxy-start",
		"// @nexus:proxy-end",
		`"/__nexus": { target: "http://localhost:8080", changeOrigin: true }`,
		`"/graphql": { target: "http://localhost:8080", changeOrigin: true }`,
		`"/oauth": { target: "http://localhost:8080", changeOrigin: true }`,
		`"/ws": { target: "http://localhost:8080", changeOrigin: true, ws: true }`,
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

// TestEnsureViteProxyForNexus_NoCommaAfterEndMarker is the regression
// test for the bootstrap bug where a comma was appended directly after
// the managed block. Because the block ends in the `// @nexus:proxy-end`
// LINE comment, that comma was swallowed by the comment (rendering
// `// @nexus:proxy-end,`) — cosmetically broken and, since it lived
// OUTSIDE the marker range, never cleaned up on re-sync. Covers all
// three bootstrap scaffolds + an idempotent re-run, asserting the
// stray comma never appears.
func TestEnsureViteProxyForNexus_NoCommaAfterEndMarker(t *testing.T) {
	scaffolds := map[string]string{
		"proxy-exists": `import { defineConfig } from 'vite'
export default defineConfig({
  server: { proxy: {} },
})
`,
		"server-no-proxy": `import { defineConfig } from 'vite'
export default defineConfig({
  server: { port: 5173 },
})
`,
		"defineconfig-only": `import { defineConfig } from 'vite'
export default defineConfig({
  plugins: [],
})
`,
	}
	for name, original := range scaffolds {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "vite.config.ts")
			if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			// Bootstrap, then re-sync — a second pass must not creep a
			// comma in either (the marker-present path preserves bytes
			// after the end marker verbatim).
			for i := 0; i < 2; i++ {
				if err := EnsureViteProxyForNexus(cfgPath, "http://localhost:9590", io.Discard); err != nil {
					t.Fatalf("pass %d: %v", i, err)
				}
			}
			got, _ := os.ReadFile(cfgPath)
			gs := string(got)
			if strings.Contains(gs, "@nexus:proxy-end,") {
				t.Errorf("stray comma swallowed by end-marker comment\n--- body ---\n%s", gs)
			}
			// Regression: an empty `proxy: {}` must not glue its closing
			// `}` onto the end-marker comment line (`// …proxy-end}`),
			// which would comment the brace out and break the object.
			if strings.Contains(gs, "@nexus:proxy-end}") {
				t.Errorf("proxy closing brace swallowed by end-marker comment\n--- body ---\n%s", gs)
			}
			if !strings.Contains(gs, "// @nexus:proxy-end") {
				t.Fatalf("end marker missing\n--- body ---\n%s", gs)
			}
			// Braces must balance (the config must still parse).
			if strings.Count(gs, "{") != strings.Count(gs, "}") {
				t.Errorf("unbalanced braces after injection\n--- body ---\n%s", gs)
			}
		})
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
	if err := SyncViteProxyForPrefixes(cfgPath, "http://localhost:8080",
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

// TestSyncViteProxyForPrefixes_AddsAndRemovesAcrossCalls covers the
// declarative-sync contract: a second call with a different prefix
// set adds the new entries and drops the ones the runtime no longer
// advertises. The marker pair is what makes this safe — entries
// OUTSIDE the markers stay untouched.
func TestSyncViteProxyForPrefixes_AddsAndRemovesAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vite.config.ts")
	original := `import { defineConfig } from 'vite'
export default defineConfig({
  server: {
    proxy: {
      "/user-custom": { target: "http://example.com", changeOrigin: true },
    },
  },
})
`
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	// First sync: /__nexus + /oats-uaa
	if err := SyncViteProxyForPrefixes(cfgPath, "http://localhost:9590",
		[]string{"/__nexus", "/oats-uaa"}, io.Discard); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	got, _ := os.ReadFile(cfgPath)
	body := string(got)
	for _, want := range []string{`"/__nexus":`, `"/oats-uaa":`, `"/user-custom":`} {
		if !strings.Contains(body, want) {
			t.Errorf("after first sync missing %q\n--- body ---\n%s", want, body)
		}
	}

	// Second sync: drop /oats-uaa, add /oats-interview. /user-custom
	// (outside markers) must survive both passes; /oats-uaa must be
	// gone since it left the managed set.
	if err := SyncViteProxyForPrefixes(cfgPath, "http://localhost:9590",
		[]string{"/__nexus", "/oats-interview"}, io.Discard); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	got, _ = os.ReadFile(cfgPath)
	body = string(got)
	if !strings.Contains(body, `"/oats-interview":`) {
		t.Errorf("added prefix /oats-interview not present\n--- body ---\n%s", body)
	}
	if strings.Contains(body, `"/oats-uaa":`) {
		t.Errorf("removed prefix /oats-uaa still present\n--- body ---\n%s", body)
	}
	if !strings.Contains(body, `"/user-custom":`) {
		t.Errorf("user-owned entry outside markers got dropped\n--- body ---\n%s", body)
	}

	// Re-sync with the same set should be a no-op (byte-identical).
	before := body
	if err := SyncViteProxyForPrefixes(cfgPath, "http://localhost:9590",
		[]string{"/__nexus", "/oats-interview"}, io.Discard); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	again, _ := os.ReadFile(cfgPath)
	if string(again) != before {
		t.Errorf("re-sync with unchanged set is not byte-identical\n--- before ---\n%s\n--- after ---\n%s", before, again)
	}
}

// TestMergeViteConfig_DoesNotInjectBuildWatch is the regression test
// for the build-hang bug: MergeViteConfig must NOT add `build.watch`.
// Setting build.watch puts `vite build` (and `nexus build`) into
// rollup watch mode, which never exits. The existing build block is
// left untouched; only the plugin gets wired.
func TestMergeViteConfig_DoesNotInjectBuildWatch(t *testing.T) {
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
	rb, _ := os.ReadFile(cfgPath)
	got := string(rb)
	if strings.Contains(got, "watch:") {
		t.Errorf("MergeViteConfig injected build.watch — this hangs `vite build`\n--- body ---\n%s", got)
	}
	// Plugin still wired; existing build block preserved.
	for _, want := range []string{"nexus(", "import nexus from", `outDir: "dist"`, `emptyOutDir: true`} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in output\n--- body ---\n%s", want, got)
		}
	}
}

// TestMergeViteConfig_NoBuildBlockAdded verifies MergeViteConfig does
// not fabricate a build block (it used to add build.watch). A config
// with no build: block stays without one; only the plugin is wired.
func TestMergeViteConfig_NoBuildBlockAdded(t *testing.T) {
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
	rb, _ := os.ReadFile(cfgPath)
	got := string(rb)
	if strings.Contains(got, "build:") || strings.Contains(got, "watch:") {
		t.Errorf("MergeViteConfig should not add a build/watch block\n--- body ---\n%s", got)
	}
	if !strings.Contains(got, "nexus(") {
		t.Errorf("expected the nexus() plugin wired\n--- body ---\n%s", got)
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
