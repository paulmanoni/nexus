package client

import "os"

// frontendMarkers are the files that make a directory the project's
// frontend root: a Vite config, in any of the extensions Vite loads. It
// is the strongest signal — every nexus-scaffolded project has one, no
// arbitrary "static assets" folder coincidentally does, and it implies the
// rest of the layout (tsconfig.json next to it, sdk/ as the dump target).
// Used by applyFrontendDefaults below.
var frontendMarkers = []string{"vite.config.ts", "vite.config.mts", "vite.config.js", "vite.config.mjs", "vite.config.cts", "vite.config.cjs"}

// candidateFrontendDirs lists the paths to probe for the marker
// file. Ordered "most likely first" so the first match wins:
//   - "web" matches `nexus new`'s scaffold layout (the canonical case)
//   - "frontend" / "client" / "app" cover common conventions in
//     hand-rolled projects that adopt the framework after the fact
//
// All paths are cwd-relative; absolute callers configure explicitly.
var candidateFrontendDirs = []string{"web", "frontend", "client", "app"}

// Off, assigned to Config.OutDir, TSConfig or ViteConfig, means "none —
// and don't auto-detect one". An empty field is UNSET and is filled from
// the detected frontend dir; Off is an explicit refusal the defaults leave
// alone. OutDir = Off disables the boot-time dump entirely (TSConfig and
// ViteConfig then have nothing to point at); TSConfig = Off keeps the dump
// but never edits tsconfig/jsconfig. Handler.AutoDumpConfig reports Off as
// "".
const Off = "-"

// ApplyFrontendDefaults is the exported entry to applyFrontendDefaults,
// for the nexus.Config.SDK one-switch path which builds a Config outside
// this package and needs the same OutDir/TSConfig/ViteConfig auto-detection.
func ApplyFrontendDefaults(cfg Config) Config { return applyFrontendDefaults(cfg) }

// applyFrontendDefaults fills in OutDir / TSConfig / ViteConfig from
// the project's frontend dir when the user left them empty. The
// detection is opt-in by filesystem layout, not by config flag:
// either the project has a recognisable frontend dir (one of
// candidateFrontendDirs containing a Vite config) and the
// defaults light up, or it doesn't and the empty fields stay empty
// (so SDK files don't get dumped to a nonexistent location).
//
// Only UNSET ("") fields are filled: an explicit path wins, and Off is
// an explicit "none" that stays Off. OutDir = Off stops the fill for all
// three — with no dump there is nothing for a tsconfig mapping or a vite
// config to point at. Idempotent, so Mount re-applying it over a Config
// the caller already defaulted (nexus.Config.SDK) changes nothing.
//
// Whether a dump actually happens is decided at boot, not here: the
// OnStart hook in nexus writes only in development (`nexus dev` or
// environment = "development"), whatever OutDir holds.
//
// Returns cfg by value so the caller's local copy gets the
// defaults; the original Config that was passed into nexus.Config
// is unaffected.
func applyFrontendDefaults(cfg Config) Config {
	if cfg.OutDir == Off {
		return cfg
	}
	dir, marker := detectFrontendDir()
	if dir == "" {
		return cfg
	}
	if cfg.OutDir == "" {
		cfg.OutDir = "./" + dir + "/sdk"
	}
	if cfg.TSConfig == "" {
		ts := "./" + dir + "/tsconfig.json"
		if fileExists(ts) {
			cfg.TSConfig = ts
		}
	}
	if cfg.ViteConfig == "" {
		// The Vite config is the marker — its existence is implied by
		// detectFrontendDir returning non-empty. Still default through
		// the same shape so cfg.ViteConfig is meaningful for the rest
		// of Mount even when the marker file disappears between
		// detection and use (race-free for our purposes; the file
		// system is the source of truth on every dev rebuild).
		cfg.ViteConfig = "./" + dir + "/" + marker
	}
	return cfg
}

// detectFrontendDir walks candidateFrontendDirs looking for a marker
// file. Returns the basename of the first matching dir and the marker
// found there, or "" when nothing matches. Cheap (a small fixed number of
// stat calls) and runs once per Mount.
func detectFrontendDir() (dir, marker string) {
	for _, d := range candidateFrontendDirs {
		for _, m := range frontendMarkers {
			if fileExists(d + "/" + m) {
				return d, m
			}
		}
	}
	return "", ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// ApplyVisibilityDefaults aligns Client.Public with the framework's
// top-level Introspection toggle. When introspection is open, the
// runtime manifest is open too — gating Public separately is
// security theatre because anything the skinny projection hides is
// already reachable through GraphQL's __schema query when
// introspection is on. When introspection is closed, Public's
// explicit value (or the default false) stands as-is.
//
// This collapses the two-flag posture down to one in the common
// case: users only need to flip Introspection, and the
// runtime-manifest exposure follows automatically. The pathological
// "introspection on, manifest skinny" combination is removed; if
// someone genuinely needs that, they're better served by a custom
// route gate downstream of Mount.
func ApplyVisibilityDefaults(cfg Config, introspection bool) Config {
	if introspection {
		cfg.Public = true
	}
	return cfg
}
