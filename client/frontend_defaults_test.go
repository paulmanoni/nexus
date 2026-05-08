package client

import (
	"os"
	"path/filepath"
	"testing"
)

// withTempCwd runs fn inside a temp directory, restoring the
// original cwd on exit. The frontend-detection helpers stat
// cwd-relative paths so we need to physically chdir for each case.
func withTempCwd(t *testing.T, build func(dir string)) func() {
	t.Helper()
	dir := t.TempDir()
	build(dir)
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	return func() { _ = os.Chdir(prev) }
}

// TestApplyFrontendDefaults_WebLayout pins the canonical case:
// the scaffold's web/ dir + vite.config.ts triggers all three
// defaults. tsconfig.json existence gates the TSConfig default
// because hand-rolled projects sometimes use jsconfig.json or no
// config at all — defaulting to a nonexistent file would break
// downstream consumers that try to read it.
func TestApplyFrontendDefaults_WebLayout(t *testing.T) {
	defer withTempCwd(t, func(d string) {
		_ = os.Mkdir(filepath.Join(d, "web"), 0o755)
		_ = os.WriteFile(filepath.Join(d, "web", "vite.config.ts"), []byte("//"), 0o644)
		_ = os.WriteFile(filepath.Join(d, "web", "tsconfig.json"), []byte("{}"), 0o644)
	})()

	got := applyFrontendDefaults(Config{Enabled: true})

	if got.OutDir != "./web/sdk" {
		t.Errorf("OutDir = %q; want ./web/sdk", got.OutDir)
	}
	if got.TSConfig != "./web/tsconfig.json" {
		t.Errorf("TSConfig = %q; want ./web/tsconfig.json", got.TSConfig)
	}
	if got.ViteConfig != "./web/vite.config.ts" {
		t.Errorf("ViteConfig = %q; want ./web/vite.config.ts", got.ViteConfig)
	}
}

// TestApplyFrontendDefaults_AlternateDirs covers projects that
// use frontend/ or client/ instead of web/. Same detection rule:
// presence of vite.config.ts is the marker.
func TestApplyFrontendDefaults_AlternateDirs(t *testing.T) {
	for _, dirName := range []string{"frontend", "client", "app"} {
		t.Run(dirName, func(t *testing.T) {
			defer withTempCwd(t, func(d string) {
				_ = os.Mkdir(filepath.Join(d, dirName), 0o755)
				_ = os.WriteFile(filepath.Join(d, dirName, "vite.config.ts"), []byte("//"), 0o644)
			})()
			got := applyFrontendDefaults(Config{Enabled: true})
			if got.OutDir != "./"+dirName+"/sdk" {
				t.Errorf("OutDir = %q; want ./%s/sdk", got.OutDir, dirName)
			}
		})
	}
}

// TestApplyFrontendDefaults_NoFrontendDir is the "Go-only API"
// case: nothing at the candidate paths, all defaults stay empty,
// the SDK dump never tries to write to a phantom location.
func TestApplyFrontendDefaults_NoFrontendDir(t *testing.T) {
	defer withTempCwd(t, func(d string) {
		// Empty dir — no candidate match.
		_ = d
	})()

	got := applyFrontendDefaults(Config{Enabled: true})

	if got.OutDir != "" || got.TSConfig != "" || got.ViteConfig != "" {
		t.Errorf("expected all empty, got %+v", got)
	}
}

// TestApplyFrontendDefaults_ExplicitOverrides confirms user-set
// values win over auto-detection. A non-standard layout (e.g.
// monorepo with apps/web) configures explicitly and the helper
// stays out of the way.
func TestApplyFrontendDefaults_ExplicitOverrides(t *testing.T) {
	defer withTempCwd(t, func(d string) {
		_ = os.Mkdir(filepath.Join(d, "web"), 0o755)
		_ = os.WriteFile(filepath.Join(d, "web", "vite.config.ts"), []byte("//"), 0o644)
		_ = os.WriteFile(filepath.Join(d, "web", "tsconfig.json"), []byte("{}"), 0o644)
	})()

	got := applyFrontendDefaults(Config{
		Enabled:    true,
		OutDir:     "./apps/web/sdk",
		TSConfig:   "./apps/web/tsconfig.json",
		ViteConfig: "./apps/web/vite.config.ts",
	})

	if got.OutDir != "./apps/web/sdk" {
		t.Errorf("OutDir = %q; want explicit value preserved", got.OutDir)
	}
	if got.TSConfig != "./apps/web/tsconfig.json" {
		t.Errorf("TSConfig overridden, got %q", got.TSConfig)
	}
	if got.ViteConfig != "./apps/web/vite.config.ts" {
		t.Errorf("ViteConfig overridden, got %q", got.ViteConfig)
	}
}

// TestApplyVisibilityDefaults pins the Introspection→Public bridge:
// introspection ON forces Public=true regardless of the user's
// explicit value (the "introspection on, manifest skinny" combo is
// security theatre and intentionally collapsed); introspection OFF
// leaves Public at whatever the user set (or default false).
func TestApplyVisibilityDefaults(t *testing.T) {
	cases := []struct {
		name          string
		introspection bool
		inPublic      bool
		want          bool
	}{
		{"intr=on,pub=on  → on", true, true, true},
		{"intr=on,pub=off → on (forced)", true, false, true},
		{"intr=off,pub=on → on (preserved)", false, true, true},
		{"intr=off,pub=off → off (default)", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplyVisibilityDefaults(Config{Public: tc.inPublic}, tc.introspection)
			if got.Public != tc.want {
				t.Errorf("Public = %v; want %v", got.Public, tc.want)
			}
		})
	}
}

// TestApplyFrontendDefaults_TSConfigOnlyWhenPresent guards the
// "tsconfig.json is conditional" rule: a vue project without one
// keeps TSConfig empty so the dump path doesn't try to read a
// nonexistent file. OutDir + ViteConfig still light up.
func TestApplyFrontendDefaults_TSConfigOnlyWhenPresent(t *testing.T) {
	defer withTempCwd(t, func(d string) {
		_ = os.Mkdir(filepath.Join(d, "web"), 0o755)
		_ = os.WriteFile(filepath.Join(d, "web", "vite.config.ts"), []byte("//"), 0o644)
		// no tsconfig.json
	})()

	got := applyFrontendDefaults(Config{Enabled: true})

	if got.OutDir != "./web/sdk" {
		t.Errorf("OutDir = %q; want ./web/sdk", got.OutDir)
	}
	if got.TSConfig != "" {
		t.Errorf("TSConfig = %q; want empty (file missing)", got.TSConfig)
	}
	if got.ViteConfig != "./web/vite.config.ts" {
		t.Errorf("ViteConfig = %q; want ./web/vite.config.ts", got.ViteConfig)
	}
}