package frontend

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmanoni/nexus"
)

// TestPlugin_DumpFollowsRuntimeSDK boots a real app in development with a
// detectable web/vite.config.ts in the working directory. RuntimeSDK:false
// leaves SDKOutDir unset, which means "no dump" — it used to be filled back
// in with ./web/sdk by the client package's detection. RuntimeSDK:true
// defaults SDKOutDir to ./web/sdk and dumps there.
func TestPlugin_DumpFollowsRuntimeSDK(t *testing.T) {
	modes := []struct {
		name     string
		nexusDev string
		env      string
	}{
		{"nexus dev", "1", "production"},
		{"environment development", "", "development"},
	}
	for _, m := range modes {
		for _, runtimeSDK := range []bool{false, true} {
			name := m.name + "/RuntimeSDK=false"
			if runtimeSDK {
				name = m.name + "/RuntimeSDK=true"
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv(nexus.NexusDevEnv, m.nexusDev)
				t.Setenv(nexus.NexusDevRootEnv, "")
				dir := t.TempDir()
				t.Chdir(dir)
				if err := os.MkdirAll(filepath.Join(dir, "web"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "web", "vite.config.ts"), []byte("export default {}\n"), 0o644); err != nil {
					t.Fatal(err)
				}

				_, stop, err := nexus.InProcess(nexus.Config{Environment: m.env},
					Plugin(Config{Root: "web", FS: minimalFS(), RuntimeSDK: runtimeSDK}))
				if err != nil {
					t.Fatalf("InProcess: %v", err)
				}
				if err := stop(context.Background()); err != nil {
					t.Fatalf("stop: %v", err)
				}

				_, statErr := os.Stat(filepath.Join(dir, "web", "sdk"))
				if wrote := statErr == nil; wrote != runtimeSDK {
					t.Errorf("web/sdk written = %v, want %v", wrote, runtimeSDK)
				}
			})
		}
	}
}
