package main

// Frontend folder names are configurable so projects that prefer
// `web/`, `client/`, or any other layout can drop in without
// renaming files and confusing the toolchain.
//
// Two settings, each defaulting to the canonical name:
//
//	NEXUS_ISLANDS_SRC   source folder (default: "islands.src")
//	NEXUS_ISLANDS_OUT   bundled output folder (default: "islands")
//
// The CLI (`nexus dev`, `nexus build`) reads these whenever it
// resolves a source / output path under the project root. The
// FRAMEWORK side (ServeFrontend / //go:embed) is unaffected —
// the operator passes whatever embed.FS they want at runtime.
// These env vars only adjust where the CLI looks + writes during
// `nexus dev` and `nexus build`.

import "os"

const (
	// defaultIslandsSrc is the canonical source-folder name; matches
	// scaffolder output + every doc example.
	defaultIslandsSrc = "islands.src"

	// defaultIslandsOut is the canonical bundle-output-folder name.
	// Embed via //go:embed islands and pass "islands" to
	// nexus.ServeFrontend to wire it up.
	defaultIslandsOut = "islands"

	// envIslandsSrc / envIslandsOut override the names above. Read
	// at every call site that needs a path, not cached, so a
	// long-running `nexus dev` session reflects env changes
	// without restart (env-var mutation between calls is a known
	// pattern for vscode tasks that flip configs mid-debug).
	envIslandsSrc = "NEXUS_ISLANDS_SRC"
	envIslandsOut = "NEXUS_ISLANDS_OUT"
)

// islandsSrcName returns the configured source-folder name, with
// the canonical default when unset. Returns just the basename —
// callers join it onto a project root themselves.
func islandsSrcName() string {
	if v := os.Getenv(envIslandsSrc); v != "" {
		return v
	}
	return defaultIslandsSrc
}

// islandsOutName returns the configured output-folder name, with
// the canonical default when unset. Same basename-only shape as
// islandsSrcName.
func islandsOutName() string {
	if v := os.Getenv(envIslandsOut); v != "" {
		return v
	}
	return defaultIslandsOut
}

// frontendDirName returns the Vite frontend project directory (holding
// package.json + vite.config + src/), relative to the project root.
// Default "web" (Vite-conventional, matches nexus.ServeFrontend's
// all:web/dist docstring); override with NEXUS_FRONTEND_DIR. Vite writes
// its build into <dir>/dist, which `go build` embeds.
func frontendDirName() string {
	if v := os.Getenv("NEXUS_FRONTEND_DIR"); v != "" {
		return v
	}
	return "web"
}
