package nexus

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/client"
)

// dumpProject makes a temp project dir with a detectable frontend
// (web/vite.config.ts + web/tsconfig.json), chdirs into it, and isolates
// the dev-mode env so the caller decides the mode. Returns the dir and the
// tsconfig's original bytes.
func dumpProject(t *testing.T, nexusDev string) (string, []byte) {
	t.Helper()
	t.Setenv(NexusDevEnv, nexusDev)
	t.Setenv(NexusDevRootEnv, "")
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
func bootOnce(t *testing.T, cfg Config) string {
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
		cfg       Config
		wantWrite bool
	}{
		{"production + SDK writes nothing", "", Config{Environment: "production", SDK: true}, false},
		{"unset environment + SDK writes nothing", "", Config{SDK: true}, false},
		{"production + explicit OutDir writes nothing", "",
			Config{Environment: "production", Client: client.Config{Enabled: true, OutDir: "./web/sdk"}}, false},
		{"staging + SDK writes nothing", "", Config{Environment: "staging", SDK: true}, false},
		{"environment development + SDK writes web/sdk", "", Config{Environment: "development", SDK: true}, true},
		{"NEXUS_DEV=1 + SDK writes web/sdk", "1", Config{Environment: "production", SDK: true}, true},
		{"environment development + Client.Enabled writes web/sdk", "",
			Config{Environment: "development", Client: client.Config{Enabled: true}}, true},
		{"NEXUS_DEV=1 implicit dev mount writes web/sdk", "1", Config{}, true},
		{"NEXUS_DEV=1 implicit dev mount, DevDisabled", "1", Config{Client: client.Config{DevDisabled: true}}, false},
		{"NEXUS_DEV=1 implicit dev mount, OutDir Off", "1", Config{Client: client.Config{OutDir: client.Off}}, false},
		{"environment development + OutDir Off", "",
			Config{Environment: "development", Client: client.Config{Enabled: true, OutDir: client.Off}}, false},
		{"NEXUS_DEV=1 + OutDir Off", "1", Config{Client: client.Config{Enabled: true, OutDir: client.Off}}, false},
		{"environment development, no client mounted", "", Config{Environment: "development"}, false},
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

// TestAutoDumpClientSDK_TSConfig: the explicit SDK switch wires the
// tsconfig path mappings; the implicit dev mount dumps but never edits a
// tsconfig the app didn't hand it.
func TestAutoDumpClientSDK_TSConfig(t *testing.T) {
	t.Run("SDK switch merges tsconfig", func(t *testing.T) {
		dir, orig := dumpProject(t, "1")
		bootOnce(t, Config{SDK: true})
		got, _ := os.ReadFile(filepath.Join(dir, "web", "tsconfig.json"))
		if bytes.Equal(got, orig) {
			t.Error("SDK=true under nexus dev should merge path mappings into web/tsconfig.json")
		}
	})
	t.Run("implicit dev mount leaves tsconfig alone", func(t *testing.T) {
		dir, orig := dumpProject(t, "1")
		bootOnce(t, Config{})
		if !sdkWritten(dir) {
			t.Fatal("precondition: implicit dev mount should dump web/sdk")
		}
		got, _ := os.ReadFile(filepath.Join(dir, "web", "tsconfig.json"))
		if !bytes.Equal(got, orig) {
			t.Errorf("implicit dev mount edited web/tsconfig.json:\n%s", got)
		}
	})
}

// TestAutoDumpClientSDK_QuietWhenUnchanged: every dev boot dumps, so a
// restart that changed nothing must print nothing.
func TestAutoDumpClientSDK_QuietWhenUnchanged(t *testing.T) {
	dir, _ := dumpProject(t, "1")
	first := bootOnce(t, Config{SDK: true})
	if !sdkWritten(dir) || !strings.Contains(first, "manifest.json") {
		t.Fatalf("first boot should write and report the SDK files; log: %q", first)
	}
	if second := bootOnce(t, Config{SDK: true}); strings.Contains(second, "sdk") {
		t.Errorf("unchanged SDK re-dump printed output: %q", second)
	}
}
