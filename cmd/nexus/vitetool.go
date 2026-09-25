package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/client"
)

// Shared plumbing for driving a real Vite project from `nexus dev` and
// `nexus build`. Vite is the only frontend engine: a frontend is a
// directory with a package.json, its tools come from node_modules, and
// nexus-vite-plugin (written into <web>/sdk by writeSDKPlugin) carries
// what the Go side knows into Vite.

// frontendEnvVar carries nexus.toml's [env] table to nexus-vite-plugin as
// a JSON object of dotted keys to string values ({"client.id": "web"}).
// The plugin exposes each key as import.meta.env.<key> in dev and build.
const frontendEnvVar = "NEXUS_FRONTEND_ENV"

// resolveFrontendDir finds the frontend project directory for the Go main
// package in pkgDir (absolute) — one resolution for nexus dev and nexus
// build, so the Vite that dev runs is the one build builds. Precedence:
//
//  1. --frontend (flag, as typed: relative to the working directory);
//  2. NEXUS_FRONTEND_DIR (relative to the package dir);
//  3. the ServeFrontend / frontend.Plugin call in the package's source;
//  4. <pkgDir>/web when it holds a package.json, or is a viteless-era
//     directory (so it gets the migration hint) — for an app whose
//     ServeFrontend root is not a string literal.
//
// Returns "" when there is no frontend, else an absolute path and where it
// came from (for --verbose).
func resolveFrontendDir(pkgDir, flag string) (dir, source string) {
	abs := func(base, p string) string {
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		return filepath.Clean(p)
	}
	if flag != "" {
		wd, _ := os.Getwd()
		return abs(wd, flag), "--frontend"
	}
	if v := os.Getenv("NEXUS_FRONTEND_DIR"); v != "" {
		return abs(pkgDir, v), "NEXUS_FRONTEND_DIR"
	}
	if d := detectFrontendDir(pkgDir); d != "" {
		return abs(pkgDir, d), "detected in source"
	}
	web := filepath.Join(pkgDir, "web")
	if p := inspectFrontend(web); p.PackageJSON || p.Legacy != "" {
		return web, "web/"
	}
	return "", ""
}

// frontendProject is what a web directory holds, as far as the CLI cares.
type frontendProject struct {
	Dir         string // absolute
	PackageJSON bool   // a Vite project: nexus drives it
	Legacy      string // no package.json, but a file that says it was a viteless project ("" otherwise)
}

// inspectFrontend looks at dir without running anything.
func inspectFrontend(dir string) frontendProject {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	p := frontendProject{Dir: abs, PackageJSON: fileExists(filepath.Join(abs, "package.json"))}
	if !p.PackageJSON {
		for _, name := range []string{"viteless.config.ts", "viteless.config.js", "viteless.config.mjs", "viteless-env.d.ts"} {
			if fileExists(filepath.Join(abs, name)) {
				p.Legacy = name
				break
			}
		}
	}
	return p
}

// legacyHint explains what a viteless-era web directory needs now.
func (p frontendProject) legacyHint() string {
	return fmt.Sprintf("%s has %s but no package.json: nexus runs the frontend with Vite now, not viteless. "+
		"Add a package.json and a vite.config.ts that uses nexus-vite-plugin (`nexus init --frontend vue --force` "+
		"writes both; see `nexus docs frontend`).", p.Dir, p.Legacy)
}

// viteBinary returns the project's own Vite executable and whether it exists.
func viteBinary(webDir string) (string, bool) {
	name := "vite"
	if runtime.GOOS == "windows" {
		name = "vite.cmd"
	}
	bin := filepath.Join(webDir, "node_modules", ".bin", name)
	return bin, fileExists(bin)
}

// frontendEnv is the environment for a Vite child: this process's, plus
// frontendEnvVar holding nexus.toml's [env] table (none when the file or
// the table is absent).
//
// Only [env] is expanded — a ${DB_PASSWORD} elsewhere in nexus.toml is
// the app's concern at boot, not the bundle's. An [env] entry naming an
// unset variable is left out with a warning on warn (nil: silent) rather
// than failing: a build machine without that secret still builds, and
// the frontend sees the key as undefined.
func frontendEnv(tomlPath string, warn io.Writer) ([]string, error) {
	vars, skipped, err := nexus.EnvVarsSkippingUnset(tomlPath)
	if err != nil {
		return nil, err
	}
	if warn != nil {
		for _, s := range skipped {
			fmt.Fprintf(warn, "%s●%s [env] %s (%s:%d) left out of the frontend: ${%s} is not set\n",
				ansiYellow, ansiReset, s.Key, filepath.Base(tomlPath), s.Line, s.Var)
		}
	}
	if vars == nil {
		vars = map[string]string{}
	}
	b, err := json.Marshal(vars)
	if err != nil {
		return nil, err
	}
	return append(os.Environ(), frontendEnvVar+"="+string(b)), nil
}

// writeSDKPlugin writes nexus-vite-plugin into <webDir>/sdk before Vite
// starts: a vite.config that imports './sdk/nexus-vite-plugin.js' must
// load on a fresh checkout, before the Go app has ever dumped the SDK.
// Unchanged files are not touched (no watcher churn).
func writeSDKPlugin(webDir string, stdout io.Writer) error {
	dir := filepath.Join(webDir, "sdk")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := client.WriteIfChanged(filepath.Join(dir, "nexus-vite-plugin.js"), client.VitePluginJS(), stdout); err != nil {
		return err
	}
	return client.WriteIfChanged(filepath.Join(dir, "nexus-vite-plugin.d.ts"), client.VitePluginDTS(), stdout)
}

// viteCmd is the project's Vite with args, run in webDir with env, in its
// own process group so it (and anything it spawns) can be signalled as
// one — stop it with SIGTERM first, so nexus-vite-plugin removes its hot
// file, and SIGKILL only after a grace period.
func viteCmd(ctx context.Context, webDir string, env []string, args ...string) (*exec.Cmd, error) {
	bin, ok := viteBinary(webDir)
	if !ok {
		return nil, fmt.Errorf("%s has no node_modules/.bin/vite — install its dependencies there", webDir)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = webDir
	cmd.Env = env
	setProcessGroup(cmd)
	return cmd, nil
}

// fileExists reports whether path is an existing regular file.
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
