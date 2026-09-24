package client

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmanoni/nexus/httpx/stdrouter"
	"github.com/paulmanoni/nexus/registry"
)

// webLayout builds the canonical detectable layout: web/vite.config.ts
// plus web/tsconfig.json.
func webLayout(d string) {
	_ = os.Mkdir(filepath.Join(d, "web"), 0o755)
	_ = os.WriteFile(filepath.Join(d, "web", "vite.config.ts"), []byte("//"), 0o644)
	_ = os.WriteFile(filepath.Join(d, "web", "tsconfig.json"), []byte("{}"), 0o644)
}

// TestApplyFrontendDefaults_OffOutDirFillsNothing pins the explicit
// "no dump": OutDir = Off survives detection, and so do the TSConfig /
// ViteConfig it would otherwise pull in — with no dump they have
// nothing to point at.
func TestApplyFrontendDefaults_OffOutDirFillsNothing(t *testing.T) {
	defer withTempCwd(t, webLayout)()

	got := applyFrontendDefaults(Config{Enabled: true, OutDir: Off})
	if got.OutDir != Off || got.TSConfig != "" || got.ViteConfig != "" {
		t.Errorf("got OutDir=%q TSConfig=%q ViteConfig=%q; want Off, \"\", \"\"",
			got.OutDir, got.TSConfig, got.ViteConfig)
	}
}

// TestApplyFrontendDefaults_OffTSConfigKeepsDump covers the per-knob
// refusal the dev auto-mount uses: the dump location still defaults,
// the tsconfig is never picked.
func TestApplyFrontendDefaults_OffTSConfigKeepsDump(t *testing.T) {
	defer withTempCwd(t, webLayout)()

	got := applyFrontendDefaults(Config{Enabled: true, TSConfig: Off})
	if got.OutDir != "./web/sdk" {
		t.Errorf("OutDir = %q; want ./web/sdk", got.OutDir)
	}
	if got.TSConfig != Off {
		t.Errorf("TSConfig = %q; want Off preserved", got.TSConfig)
	}
}

// TestApplyFrontendDefaults_Idempotent: nexus.Config.SDK defaults the
// Config and Mount defaults it again; the second pass must change
// nothing.
func TestApplyFrontendDefaults_Idempotent(t *testing.T) {
	defer withTempCwd(t, webLayout)()

	for _, in := range []Config{{}, {OutDir: Off}, {TSConfig: Off}} {
		once := applyFrontendDefaults(in)
		twice := applyFrontendDefaults(once)
		if twice.OutDir != once.OutDir || twice.TSConfig != once.TSConfig || twice.ViteConfig != once.ViteConfig {
			t.Errorf("not idempotent for %+v: %+v then %+v", in, once, twice)
		}
	}
}

// TestMount_AutoDumpConfigHonoursOff is the regression for the mount
// path itself: MountWithContributions runs the frontend defaults, which
// used to overwrite an explicit "no dump" with ./web/sdk. AutoDumpConfig
// must report Off as "" so the boot hook never sees the sentinel.
func TestMount_AutoDumpConfigHonoursOff(t *testing.T) {
	defer withTempCwd(t, webLayout)()

	cases := []struct {
		name                      string
		cfg                       Config
		wantOut, wantTS, wantVite string
	}{
		{"unset fills from web/", Config{}, "./web/sdk", "./web/tsconfig.json", "./web/vite.config.ts"},
		{"OutDir Off dumps nothing", Config{OutDir: Off}, "", "", ""},
		{"OutDir Off beats explicit TSConfig", Config{OutDir: Off, TSConfig: "./web/tsconfig.json"}, "", "", ""},
		{"TSConfig Off keeps the dump", Config{TSConfig: Off}, "./web/sdk", "", "./web/vite.config.ts"},
		{"explicit path wins", Config{OutDir: "./elsewhere"}, "./elsewhere", "./web/tsconfig.json", "./web/vite.config.ts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Mount(stdrouter.New(), registry.New(), nil, nil, "", tc.cfg)
			out, ts, vite := h.AutoDumpConfig()
			if out != tc.wantOut || ts != tc.wantTS || vite != tc.wantVite {
				t.Errorf("AutoDumpConfig() = (%q, %q, %q); want (%q, %q, %q)",
					out, ts, vite, tc.wantOut, tc.wantTS, tc.wantVite)
			}
		})
	}
}
