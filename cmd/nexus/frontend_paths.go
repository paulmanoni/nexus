package main

import "os"

// frontendDirName returns the Vite frontend project directory (holding
// package.json + vite.config + src/), relative to the project root unless
// absolute. Default "web" (Vite-conventional, matching
// nexus.ServeFrontend's all:web/dist); override with NEXUS_FRONTEND_DIR.
// Vite writes its build into <dir>/dist, which `go build` embeds. Read at
// every call, not cached.
func frontendDirName() string {
	if v := os.Getenv("NEXUS_FRONTEND_DIR"); v != "" {
		return v
	}
	return "web"
}
