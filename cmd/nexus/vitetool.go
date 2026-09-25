package main

import (
	"context"
	"encoding/json"
	"errors"
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

// errNoNpm is returned when dependencies must be installed and npm is not
// on PATH.
var errNoNpm = errors.New("npm is not on PATH — install Node.js 20 or later (https://nodejs.org), then run nexus again")

// ensureNodeModules installs the frontend's dependencies when its Vite is
// missing: `npm ci` with a package-lock.json (exact, reproducible), else
// `npm install`. A project whose Vite is already installed is left alone.
func ensureNodeModules(ctx context.Context, webDir string, stdout, stderr io.Writer) error {
	if _, ok := viteBinary(webDir); ok {
		return nil
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		return errNoNpm
	}
	args := []string{"install"}
	if fileExists(filepath.Join(webDir, "package-lock.json")) {
		args = []string{"ci"}
	}
	fmt.Fprintf(stdout, "  installing frontend dependencies (npm %s in %s)…\n", args[0], webDir)
	cmd := exec.CommandContext(ctx, npm, args...)
	cmd.Dir = webDir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npm %s in %s: %w", args[0], webDir, err)
	}
	if _, ok := viteBinary(webDir); !ok {
		return fmt.Errorf("npm %s finished but %s has no node_modules/.bin/vite — add vite to devDependencies in package.json", args[0], webDir)
	}
	return nil
}

// frontendEnv is the environment for a Vite child: this process's, plus
// frontendEnvVar holding nexus.toml's [env] table (none when the file or
// the table is absent).
func frontendEnv(tomlPath string) ([]string, error) {
	vars, err := nexus.EnvVars(tomlPath)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("%s has no node_modules/.bin/vite — run npm install there", webDir)
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
