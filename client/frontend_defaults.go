package client

import "os"

// frontendMarker is the file we look for to decide a directory is
// the project's frontend root. vite.config.ts is the strongest
// signal: every nexus-scaffolded project has it, no arbitrary
// "static assets" folder coincidentally has it, and it implies the
// presence of the rest of the layout (tsconfig.json next to it,
// sdk/ as the dump target). Used by applyFrontendDefaults below.
const frontendMarker = "vite.config.ts"

// candidateFrontendDirs lists the paths to probe for the marker
// file. Ordered "most likely first" so the first match wins:
//   - "web" matches `nexus new`'s scaffold layout (the canonical case)
//   - "frontend" / "client" / "app" cover common conventions in
//     hand-rolled projects that adopt the framework after the fact
// All paths are cwd-relative; absolute callers configure explicitly.
var candidateFrontendDirs = []string{"web", "frontend", "client", "app"}

// applyFrontendDefaults fills in OutDir / TSConfig / ViteConfig from
// the project's frontend dir when the user left them empty. The
// detection is opt-in by filesystem layout, not by config flag:
// either the project has a recognisable frontend dir (one of
// candidateFrontendDirs containing a vite.config.ts) and the
// defaults light up, or it doesn't and the empty fields stay empty
// (so SDK files don't get dumped to a nonexistent location).
//
// Explicit values always win — applyFrontendDefaults only fills
// fields that were left at the zero value. Callers with a
// non-standard layout override per field.
//
// Returns cfg by value so the caller's local copy gets the
// defaults; the original Config that was passed into nexus.Config
// is unaffected.
func applyFrontendDefaults(cfg Config) Config {
	dir := detectFrontendDir()
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
		// vite.config.ts is the marker — its existence is implied by
		// detectFrontendDir returning non-empty. Still default through
		// the same shape so cfg.ViteConfig is meaningful for the rest
		// of Mount even when the marker file disappears between
		// detection and use (race-free for our purposes; the file
		// system is the source of truth on every dev rebuild).
		cfg.ViteConfig = "./" + dir + "/" + frontendMarker
	}
	return cfg
}

// detectFrontendDir walks candidateFrontendDirs looking for the
// marker file. Returns the basename of the first match, or "" when
// nothing matches. Cheap (a small fixed number of stat calls) and
// runs once per Mount.
func detectFrontendDir() string {
	for _, dir := range candidateFrontendDirs {
		if fileExists(dir + "/" + frontendMarker) {
			return dir
		}
	}
	return ""
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