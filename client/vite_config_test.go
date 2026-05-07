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