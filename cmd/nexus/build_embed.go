package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// execCommand is package-level so tests can stub it out; same
// pattern the rest of cmd/nexus uses.
var execCommand = exec.Command

// embedConfigVar is the fully-qualified linker target for the framework's
// build-time config embed. Must match the package var in
// github.com/paulmanoni/nexus/config_embed.go.
const embedConfigVar = "github.com/paulmanoni/nexus.embeddedConfigB64"

// embedConfigLDFlag reads nexus.toml from the main package's directory and
// returns a `-ldflags` value that bakes it (base64-encoded) into the
// binary via the linker, plus the raw byte count for logging. When no
// nexus.toml is present it returns ("", 0, nil) — a pure-Go app without
// config embeds nothing. base64 keeps the value a single -X-safe token.
//
// The raw file bytes are embedded verbatim (${VAR} placeholders intact),
// so the binary carries the config template, not resolved secrets — those
// expand from the runtime environment when Boot loads it.
func embedConfigLDFlag(mainDir string) (string, int, error) {
	path := filepath.Join(mainDir, "nexus.toml")
	raw, err := os.ReadFile(path) // #nosec G304 -- project-local config
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, nil
		}
		return "", 0, err
	}
	enc := base64.StdEncoding.EncodeToString(raw)
	return fmt.Sprintf("-X %s=%s", embedConfigVar, enc), len(raw), nil
}

// simpleBuildOptions configures runSimpleBuild.
type simpleBuildOptions struct {
	Output      string
	MainPackage string
	Stdout      io.Writer
	Stderr      io.Writer
}

// runSimpleBuild builds the frontend (when the project has one), then the
// Go binary that embeds it, injecting decorator-form handler
// registrations via a build overlay (no source files written).
//
// Pipeline order matters:
//
//  1. frontendBuild runs the frontend's own Vite (installing its
//     dependencies first when needed) and writes <frontend>/dist, plus
//     dist/ssr when src/ssr.ts exists. Skipped when the frontend dir has
//     no package.json (a pure-Go app, or a static dist).
//  2. buildHandlerOverlay scans //@ annotations into a temp overlay.
//  3. go build compiles the main package. The app's own //go:embed (the
//     `//go:embed all:web/dist` next to ServeFrontend) picks up the fresh
//     bundle, and nexus.toml is baked in via -ldflags.
func runSimpleBuild(opts simpleBuildOptions) error {
	pkg := opts.MainPackage
	if pkg == "" {
		pkg = "."
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	mainDir := resolveMainDir(cwd, pkg)

	ctx, stop := buildSignalContext()
	defer stop()
	if err := frontendBuild(ctx, mainDir, opts.Stdout, opts.Stderr); err != nil {
		return fmt.Errorf("nexus build: frontend: %w", err)
	}

	// Inject the decorator-form handler registrations (//@rest / //@provide / …)
	// via a `go build -overlay`, so NOTHING is written into the source tree (zero
	// churn) — mirroring `nexus dev`. Run `nexus generate handlers` to eject
	// committed *_gen.go for a bare `go build` / `go install` / `go test`.
	//
	// A scan error here is FATAL (unlike `nexus dev`, which warns and lets
	// `go run` surface it): shipping a binary that silently omits handler
	// registrations would be worse than a failed build.
	overlayPath, cleanupOverlay, err := buildHandlerOverlay(cwd)
	if err != nil {
		return fmt.Errorf("nexus build: handler codegen: %w", err)
	}
	defer cleanupOverlay()

	args := []string{"build"}
	if overlayPath != "" {
		args = append(args, "-overlay="+overlayPath)
		fmt.Fprintln(opts.Stdout, "handler codegen: injected via overlay")
	}
	// Bake nexus.toml into the binary via the linker so the built artifact
	// is self-contained — no config file needs to ship alongside it. The
	// disk file still wins at runtime (operators can override without a
	// rebuild); this is the fallback the embedded binary carries. The raw
	// file is embedded with ${VAR} placeholders intact, so secrets resolve
	// from the runtime environment, never baked in.
	if ldflag, n, err := embedConfigLDFlag(mainDir); err != nil {
		return fmt.Errorf("nexus build: embed nexus.toml: %w", err)
	} else if ldflag != "" {
		args = append(args, "-ldflags", ldflag)
		fmt.Fprintf(opts.Stdout, "embedded nexus.toml (%d bytes)\n", n)
	}
	if opts.Output != "" {
		args = append(args, "-o", opts.Output)
	}
	args = append(args, pkg)
	cmd := execCommand("go", args...)
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	fmt.Fprintf(opts.Stdout, "go %s\n", strings.Join(printableBuildArgs(args), " "))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build failed: %w", err)
	}
	out := opts.Output
	if out == "" {
		out = filepath.Base(mainDir)
	}
	fmt.Fprintf(opts.Stdout, "built %s\n", out)
	return nil
}

// resolveMainDir converts the user-facing main-package import path (e.g.
// ".", "./cmd/server", "github.com/foo/bar/cmd/x") into an absolute
// directory on disk relative to projectRoot. The frontend dir and
// nexus.toml are looked up there.
//
// Relative paths join with projectRoot; absolute import paths are
// best-effort treated as projectRoot's relative equivalents (split on the
// module path is the caller's problem in v1 — most apps use "." or
// "./cmd/x" style).
func resolveMainDir(projectRoot, mainPkg string) string {
	if mainPkg == "" || mainPkg == "." {
		return projectRoot
	}
	if strings.HasPrefix(mainPkg, "./") || strings.HasPrefix(mainPkg, "../") {
		return filepath.Join(projectRoot, mainPkg)
	}
	// Absolute import path: not always resolvable to a path without
	// `go list -m` plumbing. Fall back to projectRoot, which is right for
	// module-rooted main packages; otherwise pass the package as
	// ./cmd/foo.
	return projectRoot
}

// printableBuildArgs is args for the log line: the embedded nexus.toml is
// shown by name, not as its base64 — kilobytes of noise, and plaintext to
// anyone who decodes a CI log, whatever literal values the file holds.
func printableBuildArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "-X "+embedConfigVar+"=") {
			a = "-X " + embedConfigVar + "=<nexus.toml>"
		}
		out[i] = a
	}
	return out
}
