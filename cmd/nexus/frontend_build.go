package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/viteless"
)

// frontendBuild builds the frontend with the embedded viteless engine so
// `go build` can embed the output. It resolves <projectRoot>/web (or
// NEXUS_FRONTEND_DIR) and produces web/dist — no npm, no Node required (the
// runtime and the build are a single Go binary). If a real Vite is installed
// in the project, viteless delegates to it; otherwise it uses its own engine.
//
// This is the function `nexus build` calls before `go build`; the embed-gen
// step that runs next bakes web/dist into the binary.
//
// Skips silently when the frontend dir has no source tree (a pure-Go app).
// Returns an error when the build fails.
func frontendBuild(projectRoot string, stdout, stderr io.Writer) error {
	dir := filepath.Join(projectRoot, frontendDirName())
	// A frontend project is identified by an index.html or a src/ tree —
	// package.json is no longer required (viteless reads viteless.config.ts /
	// vite.config.ts, or works with none).
	hasFrontend := false
	for _, marker := range []string{"index.html", "src"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			hasFrontend = true
			break
		}
	}
	if !hasFrontend {
		return nil // pure-Go app, skip
	}

	// Expose the nexus.toml [env] table to the frontend build as
	// import.meta.env.<dotted.name> (e.g. import.meta.env.client.id).
	env, _ := nexus.EnvVars(filepath.Join(projectRoot, "nexus.toml"))

	fmt.Fprintf(stdout, "%s●%s frontend: viteless build → %s\n", ansiCyan, ansiReset, filepath.Join(dir, "dist"))
	res, err := viteless.Build(viteless.BuildConfig{
		Root: dir,
		Env:  env,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(stdout, "%s[web]%s %s\n", ansiCyan, ansiReset, fmt.Sprintf(format, args...))
		},
	})
	if err != nil {
		return fmt.Errorf("frontend build: %w", err)
	}
	if len(res.Errors) > 0 {
		for _, e := range res.Errors {
			fmt.Fprintf(stderr, "%s●%s frontend build error: %s\n", ansiYellow, ansiReset, e)
		}
		return fmt.Errorf("frontend build: %d error(s)", len(res.Errors))
	}
	return nil
}
