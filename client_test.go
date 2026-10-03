package nexus

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2/client"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/dev"
)

// dumpProject makes a temp project dir with a detectable frontend
// (web/vite.config.ts + web/tsconfig.json), chdirs into it, and isolates
// the dev-mode env so the caller decides the mode. Returns the dir and the
// tsconfig's original bytes.
func dumpProject(t *testing.T, nexusDev string) (string, []byte) {
	t.Helper()
	t.Setenv(dev.Env, nexusDev)
	t.Setenv(dev.RootEnv, "")
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	tsconfig := []byte("{\n  \"compilerOptions\": {}\n}\n")
	if err := os.WriteFile(filepath.Join(dir, "web", "vite.config.ts"), []byte("export default {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "web", "tsconfig.json"), tsconfig, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, tsconfig
}

// bootOnce runs a full lifecycle start + stop (no listener), which is
// where the SDK dump fires, and returns what the std logger printed.
func bootOnce(t *testing.T, cfg config.Runtime) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	_, stop, err := InProcess(cfg)
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	if err := stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	return buf.String()
}

func sdkWritten(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "web", "sdk", "manifest.json"))
	return err == nil
}

// TestAutoDumpClientSDK pins when the running app writes the client SDK
// to disk: only in development — `nexus dev` (NEXUS_DEV=1) or environment
// "development", the same rule as the Vite hot file — and only where the
// mount says. A production binary booting next to a web/vite.config.ts
// used to write ./web/sdk into its working directory on every start.
func TestAutoDumpClientSDK(t *testing.T) {
	cases := []struct {
		name      string
		nexusDev  string
		cfg       config.Runtime
		wantWrite bool
	}{
		{"production + SDK writes nothing", "", config.Runtime{Environment: "production", SDK: true}, false},
		{"unset environment + SDK writes nothing", "", config.Runtime{SDK: true}, false},
		{"production + explicit OutDir writes nothing", "",
			config.Runtime{Environment: "production", Client: client.Config{Enabled: true, OutDir: "./web/sdk"}}, false},
		{"staging + SDK writes nothing", "", config.Runtime{Environment: "staging", SDK: true}, false},
		{"environment development + SDK writes web/sdk", "", config.Runtime{Environment: "development", SDK: true}, true},
		{"NEXUS_DEV=1 + SDK writes web/sdk", "1", config.Runtime{Environment: "production", SDK: true}, true},
		{"environment development + Client.Enabled writes web/sdk", "",
			config.Runtime{Environment: "development", Client: client.Config{Enabled: true}}, true},
		{"NEXUS_DEV=1 implicit dev mount writes web/sdk", "1", config.Runtime{}, true},
		{"NEXUS_DEV=1 implicit dev mount, DevDisabled", "1", config.Runtime{Client: client.Config{DevDisabled: true}}, false},
		{"NEXUS_DEV=1 implicit dev mount, OutDir Off", "1", config.Runtime{Client: client.Config{OutDir: client.Off}}, false},
		{"environment development + OutDir Off", "",
			config.Runtime{Environment: "development", Client: client.Config{Enabled: true, OutDir: client.Off}}, false},
		{"NEXUS_DEV=1 + OutDir Off", "1", config.Runtime{Client: client.Config{Enabled: true, OutDir: client.Off}}, false},
		{"environment development, no client mounted", "", config.Runtime{Environment: "development"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := dumpProject(t, tc.nexusDev)
			out := bootOnce(t, tc.cfg)
			if got := sdkWritten(dir); got != tc.wantWrite {
				t.Fatalf("web/sdk written = %v, want %v (log: %q)", got, tc.wantWrite, out)
			}
			if !tc.wantWrite {
				if _, err := os.Stat(filepath.Join(dir, "web", "sdk")); err == nil {
					t.Error("web/sdk directory created although nothing should be written")
				}
				if strings.Contains(out, "sdk") {
					t.Errorf("a boot that writes nothing must print nothing about it; log: %q", out)
				}
			}
		})
	}
}

// TestAutoDumpClientSDK_TSConfig: every dev mount wires the tsconfig
// paths — Vue's SFC compiler finds 'nexus-client' only through them — and
// TSConfig / OutDir = client.Off keep the file (or the whole dump) out,
// under the SDK switch as under the implicit mount.
func TestAutoDumpClientSDK_TSConfig(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  config.Runtime
	}{
		{"SDK switch", config.Runtime{SDK: true}},
		{"implicit dev mount", config.Runtime{}},
	} {
		t.Run(c.name+" merges tsconfig", func(t *testing.T) {
			dir, orig := dumpProject(t, "1")
			bootOnce(t, c.cfg)
			got, _ := os.ReadFile(filepath.Join(dir, "web", "tsconfig.json"))
			if bytes.Equal(got, orig) || !bytes.Contains(got, []byte(`"nexus-client"`)) {
				t.Errorf("want the nexus-client mapping merged into web/tsconfig.json, got:\n%s", got)
			}
		})
		t.Run(c.name+" honours TSConfig = Off", func(t *testing.T) {
			dir, orig := dumpProject(t, "1")
			cfg := c.cfg
			cfg.Client.TSConfig = client.Off
			bootOnce(t, cfg)
			if !sdkWritten(dir) {
				t.Fatal("precondition: the SDK should still be dumped")
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "web", "tsconfig.json")); !bytes.Equal(got, orig) {
				t.Errorf("TSConfig = Off, yet web/tsconfig.json was edited:\n%s", got)
			}
		})
		t.Run(c.name+" honours OutDir = Off", func(t *testing.T) {
			dir, orig := dumpProject(t, "1")
			cfg := c.cfg
			cfg.Client.OutDir = client.Off
			bootOnce(t, cfg)
			if sdkWritten(dir) {
				t.Error("OutDir = Off, yet web/sdk was written")
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "web", "tsconfig.json")); !bytes.Equal(got, orig) {
				t.Errorf("OutDir = Off, yet web/tsconfig.json was edited:\n%s", got)
			}
		})
	}
}

// TestAutoDumpClientSDK_QuietWhenUnchanged: every dev boot dumps, so a
// restart that changed nothing must print nothing.
func TestAutoDumpClientSDK_QuietWhenUnchanged(t *testing.T) {
	dir, _ := dumpProject(t, "1")
	first := bootOnce(t, config.Runtime{SDK: true})
	if !sdkWritten(dir) || !strings.Contains(first, "manifest.json") {
		t.Fatalf("first boot should write and report the SDK files; log: %q", first)
	}
	if second := bootOnce(t, config.Runtime{SDK: true}); strings.Contains(second, "sdk") {
		t.Errorf("unchanged SDK re-dump printed output: %q", second)
	}
}

// NEXUS_ENVIRONMENT overrides nexus.toml's environment, so a deployment
// that ships the scaffold's environment = "development" can still say it
// is production — and then nothing is written.
func TestAutoDumpClientSDK_NexusEnvironmentOverrides(t *testing.T) {
	dir, orig := dumpProject(t, "")
	t.Setenv("NEXUS_ENVIRONMENT", "production")
	bootOnce(t, config.Runtime{SDK: true, Environment: "development"})
	if sdkWritten(dir) {
		t.Error("NEXUS_ENVIRONMENT=production, yet web/sdk was written")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "web", "tsconfig.json")); !bytes.Equal(got, orig) {
		t.Errorf("NEXUS_ENVIRONMENT=production, yet web/tsconfig.json was edited:\n%s", got)
	}

	dir, _ = dumpProject(t, "")
	t.Setenv("NEXUS_ENVIRONMENT", "development")
	bootOnce(t, config.Runtime{SDK: true})
	if !sdkWritten(dir) {
		t.Error("NEXUS_ENVIRONMENT=development with no configured environment should dump")
	}
}

// TestDevAutoMountClientSDK covers the dev-only client SDK fallback:
// under NEXUS_DEV=1 a plain app (no Client config) gets the SDK mounted
// so `nexus dev` can read /__nexus/client/manifest.json, while an
// explicit opt-out, the non-dev case, and an already-mounted handler
// are all respected.
func TestDevAutoMountClientSDK(t *testing.T) {
	t.Run("dev mounts when nothing else did", func(t *testing.T) {
		t.Setenv(dev.Env, "1")
		a := New(config.Runtime{})
		if a.ClientHandler() != nil {
			t.Fatal("precondition: handler should be nil before the late invoke")
		}
		devAutoMountClientSDK(a)
		if a.ClientHandler() == nil {
			t.Error("expected client SDK auto-mounted under NEXUS_DEV=1")
		}
	})

	t.Run("no mount when not in dev", func(t *testing.T) {
		// NEXUS_DEV explicitly empty for this subtest.
		t.Setenv(dev.Env, "")
		a := New(config.Runtime{})
		devAutoMountClientSDK(a)
		if a.ClientHandler() != nil {
			t.Error("client SDK must NOT auto-mount outside dev")
		}
	})

	t.Run("DevDisabled opts out even in dev", func(t *testing.T) {
		t.Setenv(dev.Env, "1")
		a := New(config.Runtime{Client: client.Config{DevDisabled: true}})
		devAutoMountClientSDK(a)
		if a.ClientHandler() != nil {
			t.Error("DevDisabled should keep the SDK closed in dev")
		}
	})

	t.Run("does not replace an explicit mount", func(t *testing.T) {
		t.Setenv(dev.Env, "1")
		a := New(config.Runtime{Client: client.Config{Enabled: true}})
		first := a.ClientHandler()
		if first == nil {
			t.Fatal("explicit Client.Enabled should have mounted in New()")
		}
		devAutoMountClientSDK(a)
		if a.ClientHandler() != first {
			t.Error("dev fallback must not replace an explicitly-mounted handler")
		}
	})
}

// TestSDKSwitch covers the one-switch Config.SDK front door: it mounts the
// full client SDK wherever it's set, independently of Introspection — the
// app's own browser bundle imports the SDK, so a production binary that
// locks the dashboard down must still be able to serve it.
func TestSDKSwitch(t *testing.T) {
	t.Run("mounts under dev", func(t *testing.T) {
		t.Setenv(dev.Env, "1")
		a := New(config.Runtime{SDK: true})
		if a.ClientHandler() == nil {
			t.Error("SDK=true should mount the client SDK under NEXUS_DEV=1")
		}
	})

	t.Run("mounts when introspection is on, even outside dev", func(t *testing.T) {
		t.Setenv(dev.Env, "")
		a := New(config.Runtime{SDK: true, Introspection: true})
		if a.ClientHandler() == nil {
			t.Error("SDK=true should mount when Introspection is true")
		}
	})

	t.Run("mounts with introspection off, outside dev", func(t *testing.T) {
		t.Setenv(dev.Env, "")
		a := New(config.Runtime{SDK: true}) // introspection off, not in dev
		if a.ClientHandler() == nil {
			t.Error("SDK=true should mount regardless of Introspection")
		}
	})

	t.Run("stays closed when unset", func(t *testing.T) {
		t.Setenv(dev.Env, "")
		a := New(config.Runtime{})
		if a.ClientHandler() != nil {
			t.Error("no SDK switch, no client mount")
		}
	})

	// Mounting isn't enough: with introspection off the routes used to
	// mount behind a gate that 404s every non-allowlisted peer, which
	// for a browser is indistinguishable from not mounting at all.
	t.Run("routes answer an anonymous request with introspection off", func(t *testing.T) {
		t.Setenv(dev.Env, "")
		a := New(config.Runtime{SDK: true})
		for _, path := range []string{
			"/__nexus/client/client.js",
			"/__nexus/client/manifest.json",
		} {
			w := httptest.NewRecorder()
			a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", path, w.Code)
			}
		}
	})
}
