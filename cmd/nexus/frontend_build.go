package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// ssrEntry is the server entry `nexus build` looks for in the frontend
// dir; when it exists, a second `vite build --ssr` writes dist/ssr.
const ssrEntry = "src/ssr.ts"

// frontendBuild runs the frontend's own Vite so `go build` can embed its
// output. The frontend is the directory nexus dev would run
// (resolveFrontendDir: flag, NEXUS_FRONTEND_DIR, the ServeFrontend call in
// mainDir's source, then web/), so a build never skips the Vite that dev
// runs and embeds a stale dist. It is a Vite project when it has a
// package.json. Then, in order:
//
//  1. dependencies are installed when its Vite is missing, with the
//     package manager its lockfile names (ensureNodeModules);
//  2. nexus-vite-plugin is written into <dir>/sdk, so a vite.config that
//     imports it loads on a fresh checkout;
//  3. `vite build` runs with nexus.toml's [env] table in
//     NEXUS_FRONTEND_ENV (the plugin exposes it as import.meta.env.*);
//  4. when src/ssr.ts exists, `vite build --ssr src/ssr.ts` writes the
//     server bundle to dist/ssr, after the client build so the client's
//     emptyOutDir can't remove it, and without emptying dist itself;
//  5. dist must then hold .vite/manifest.json or index.html — what
//     ServeFrontend and the Inertia engine read.
//
// Vite's output streams to stdout/stderr, so a failing build shows why.
// A directory without a package.json is not built: a pure-Go app has
// none, and a static or hand-written dist is embedded as it is. A
// viteless-era directory is an error with the migration hint, since
// building the binary without its frontend would ship a stale bundle.
func frontendBuild(ctx context.Context, mainDir, flag string, stdout, stderr io.Writer) error {
	dir, _ := resolveFrontendDir(mainDir, flag)
	if dir == "" {
		return nil
	}
	p := inspectFrontend(dir)
	if p.Legacy != "" {
		return errors.New(p.legacyHint())
	}
	if !p.PackageJSON {
		return nil
	}

	if err := ensureNodeModules(ctx, p.Dir, stdout, stderr); err != nil {
		return err
	}
	if err := writeSDKPlugin(p.Dir, stdout); err != nil {
		return fmt.Errorf("write nexus-vite-plugin into %s: %w", filepath.Join(p.Dir, "sdk"), err)
	}
	env, err := frontendEnv(filepath.Join(mainDir, "nexus.toml"), stderr)
	if err != nil {
		return fmt.Errorf("read [env] from nexus.toml: %w", err)
	}

	dist := filepath.Join(p.Dir, "dist")
	fmt.Fprintf(stdout, "%s●%s frontend: vite build → %s\n", ansiCyan, ansiReset, dist)
	if err := runViteBuild(ctx, p.Dir, env, stdout, stderr, "build"); err != nil {
		return err
	}
	if fileExists(filepath.Join(p.Dir, filepath.FromSlash(ssrEntry))) {
		fmt.Fprintf(stdout, "%s●%s frontend: vite build --ssr %s → %s\n", ansiCyan, ansiReset, ssrEntry, filepath.Join(dist, "ssr"))
		if err := runViteBuild(ctx, p.Dir, env, stdout, stderr,
			"build", "--ssr", ssrEntry, "--outDir", "dist/ssr", "--emptyOutDir=false"); err != nil {
			return err
		}
	}

	if !fileExists(filepath.Join(dist, ".vite", "manifest.json")) && !fileExists(filepath.Join(dist, "index.html")) {
		return fmt.Errorf("vite build finished but %s has neither .vite/manifest.json nor index.html — "+
			"nexus embeds and serves <frontend>/dist, so build.outDir in vite.config must stay 'dist', "+
			"and nexus-vite-plugin must be in its plugins (it turns on build.manifest)", dist)
	}
	return nil
}

// runViteBuild runs the project's Vite with args to completion, streaming
// its output. Vite runs in its own process group (viteCmd), which a
// terminal's Ctrl-C does not reach, so cancelling ctx signals the whole
// group: SIGTERM first, then a kill if it hasn't exited after the grace.
func runViteBuild(ctx context.Context, webDir string, env []string, stdout, stderr io.Writer, args ...string) error {
	cmd, err := viteCmd(ctx, webDir, env, args...)
	if err != nil {
		return err
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := killProcessGroup(cmd.Process.Pid, syscall.SIGTERM); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = devKillGrace
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("vite %s interrupted", args[0])
		}
		return fmt.Errorf("vite %s failed in %s (its output is above): %w", strings.Join(args, " "), webDir, err)
	}
	return nil
}

// buildSignalContext is cancelled by Ctrl-C, SIGTERM or a hangup, so a Vite child
// in its own process group is stopped with the build.
func buildSignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), stopSignals...)
}
