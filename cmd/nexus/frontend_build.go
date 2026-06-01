package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// frontendBuild builds the frontend with Vite so `go build` can embed the
// output. It resolves <projectRoot>/web (or NEXUS_FRONTEND_DIR), runs
// `npm ci`/`npm install` when node_modules is absent, then `npm run build`
// (vite build → web/dist). The embed-gen step that runs next bakes web/dist
// into the binary.
//
// This is the function `nexus build` calls before `go build`. It requires
// Node/npm on PATH (the accepted build-time dependency of the Vite pipeline);
// the runtime stays a single Go binary with the SPA embedded.
//
// Skips silently when <dir>/package.json is absent (a pure-Go app with no
// frontend). Returns an error when npm/vite fail.
func frontendBuild(projectRoot string, stdout, stderr io.Writer) error {
	dir := filepath.Join(projectRoot, frontendDirName())
	pkgJSON := filepath.Join(dir, "package.json")
	if _, err := os.Stat(pkgJSON); errors.Is(err, fs.ErrNotExist) {
		return nil // no frontend project here — pure-Go app, skip
	} else if err != nil {
		return fmt.Errorf("frontend build: stat %s: %w", pkgJSON, err)
	}

	// Install deps when node_modules is absent. Prefer the reproducible
	// `npm ci` when a lockfile exists, else `npm install`.
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); errors.Is(err, fs.ErrNotExist) {
		sub := "install"
		if _, lerr := os.Stat(filepath.Join(dir, "package-lock.json")); lerr == nil {
			sub = "ci"
		}
		fmt.Fprintf(stdout, "%s●%s frontend: node_modules missing — npm %s in %s\n", ansiCyan, ansiReset, sub, dir)
		if err := runFrontendNpm(dir, stdout, stderr, sub); err != nil {
			return fmt.Errorf("frontend build: npm %s in %s: %w", sub, dir, err)
		}
	}

	fmt.Fprintf(stdout, "%s●%s frontend: npm run build (vite) in %s\n", ansiCyan, ansiReset, dir)
	if err := runFrontendNpm(dir, stdout, stderr, "run", "build"); err != nil {
		return fmt.Errorf("frontend build: npm run build in %s: %w", dir, err)
	}
	return nil
}

// runFrontendNpm runs `npm <args...>` in dir, streaming output. execCommand
// (build_embed.go) is the package-level seam tests stub out.
func runFrontendNpm(dir string, stdout, stderr io.Writer, args ...string) error {
	cmd := execCommand("npm", args...)
	cmd.Dir = dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
