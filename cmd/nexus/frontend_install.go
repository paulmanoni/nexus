package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Installing a frontend's dependencies, for nexus dev and nexus build.
//
// The install runs only when the project's Vite is missing (or an earlier
// install was interrupted), with the package manager the project uses —
// its lockfile decides, so a pnpm or Yarn project is never handed to npm,
// which would ignore the real lockfile and write a package-lock.json of
// its own.

// errNoNpm is returned when dependencies must be installed with npm and
// npm is not on PATH.
var errNoNpm = errors.New("npm is not on PATH — install Node.js 20 or later (https://nodejs.org), then run nexus again")

// installGrace is how long an interrupted install gets between SIGTERM
// and SIGKILL to its process group.
const installGrace = 2 * time.Second

// packageManager is how one frontend's dependencies are installed.
type packageManager struct {
	Name     string   // npm | pnpm | yarn | bun
	Args     []string // install arguments
	Lockfile string   // the lockfile that chose it (and makes the install exact); "" when none
	PnP      bool     // Yarn Plug'n'Play: installs no node_modules
}

func (pm packageManager) String() string { return pm.Name + " " + strings.Join(pm.Args, " ") }

// lockfiles map each lockfile to its package manager, in the order they
// are consulted when the project holds more than one.
var lockfiles = []struct{ file, pm string }{
	{"pnpm-lock.yaml", "pnpm"},
	{"yarn.lock", "yarn"},
	{"bun.lock", "bun"},
	{"bun.lockb", "bun"},
	{"package-lock.json", "npm"},
	{"npm-shrinkwrap.json", "npm"},
}

// detectPackageManager reads dir without running anything. The tool is
// package.json's "packageManager" field when it names one (Corepack's
// declaration, which wins over a stray lockfile), else the first lockfile
// present, else npm. With its lockfile the install is frozen — the
// lockfile is the contract, and a mismatch with package.json fails
// rather than silently rewriting it: npm ci, pnpm install
// --frozen-lockfile, yarn install --frozen-lockfile (Yarn 1) or
// --immutable (Yarn 2+), bun install --frozen-lockfile.
func detectPackageManager(dir string) packageManager {
	declared, version := declaredPackageManager(dir)
	pm := packageManager{Name: declared}
	for _, l := range lockfiles {
		if !fileExists(filepath.Join(dir, l.file)) {
			continue
		}
		if pm.Name == "" {
			pm.Name = l.pm
		}
		if l.pm == pm.Name {
			pm.Lockfile = l.file
			break
		}
	}
	if pm.Name == "" {
		pm.Name = "npm"
	}
	frozen := pm.Lockfile != ""
	switch pm.Name {
	case "npm":
		pm.Args = []string{"install"}
		if frozen {
			pm.Args = []string{"ci"}
		}
	case "yarn":
		pm.Args = []string{"install"}
		berry := yarnBerry(dir, version)
		if frozen {
			if berry {
				pm.Args = append(pm.Args, "--immutable")
			} else {
				pm.Args = append(pm.Args, "--frozen-lockfile")
			}
		}
		if berry {
			pm.PnP = yarnNodeLinker(dir) == "pnp"
		} else {
			pm.PnP = fileExists(filepath.Join(dir, ".pnp.cjs"))
		}
	default: // pnpm, bun
		pm.Args = []string{"install"}
		if frozen {
			pm.Args = append(pm.Args, "--frozen-lockfile")
		}
	}
	return pm
}

// declaredPackageManager returns the tool and version package.json's
// "packageManager" field names ("pnpm@9.12.0" → pnpm, 9.12.0), or "" when
// it names none nexus knows.
func declaredPackageManager(dir string) (name, version string) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", ""
	}
	var pkg struct {
		PackageManager string `json:"packageManager"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return "", ""
	}
	name, version, _ = strings.Cut(pkg.PackageManager, "@")
	switch name {
	case "npm", "pnpm", "yarn", "bun":
		version, _, _ = strings.Cut(version, "+") // drop a "+sha512.…" hash
		return name, version
	}
	return "", ""
}

// yarnBerry reports whether dir is a Yarn 2+ project: the declared version,
// else a .yarnrc.yml (Yarn 1 reads .yarnrc), else a lockfile in Berry's
// format (it opens with a __metadata block; Yarn 1's does not).
func yarnBerry(dir, declaredVersion string) bool {
	if declaredVersion != "" {
		major, _, _ := strings.Cut(declaredVersion, ".")
		n, err := strconv.Atoi(major)
		return err == nil && n >= 2
	}
	if fileExists(filepath.Join(dir, ".yarnrc.yml")) {
		return true
	}
	f, err := os.Open(filepath.Join(dir, "yarn.lock"))
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 1024)
	n, _ := io.ReadFull(f, head)
	return bytes.Contains(head[:n], []byte("__metadata:"))
}

// yarnNodeLinker returns .yarnrc.yml's nodeLinker, "pnp" (Yarn 2+'s
// default) when unset.
func yarnNodeLinker(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".yarnrc.yml"))
	if err != nil {
		return "pnp"
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == "nodeLinker" {
			v, _, _ = strings.Cut(v, "#")
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return "pnp"
}

// errPnP explains why a Plug'n'Play project is refused. nexus runs the
// project's Vite binary itself — it matches the hot file to that
// process, signals its process group to stop it, and resolves the
// Windows .cmd shim — and PnP has no binary on disk to run. Wrapping it
// as `yarn vite` would put Yarn between nexus and Vite on every signal;
// the node-modules linker is a documented one-line opt-in instead.
func errPnP(dir string) error {
	return fmt.Errorf("%s uses Yarn Plug'n'Play, which installs no node_modules/.bin/vite for nexus to run — "+
		"add `nodeLinker: node-modules` to %s and run yarn install", dir, filepath.Join(dir, ".yarnrc.yml"))
}

// ensureNodeModules installs the frontend's dependencies when its Vite is
// missing, or when an earlier install was interrupted (see
// installPendingPath), with the package manager detectPackageManager
// picks. A project whose Vite is installed is left alone.
//
// The installer runs in its own process group, and cancelling ctx stops
// the whole group — SIGTERM, then SIGKILL after installGrace — so an
// interrupted install leaves no package-manager children behind.
func ensureNodeModules(ctx context.Context, webDir string, stdout, stderr io.Writer) error {
	pending := installPendingPath(webDir)
	if _, ok := viteBinary(webDir); ok && (pending == "" || !fileExists(pending)) {
		return nil
	}
	pm := detectPackageManager(webDir)
	if pm.PnP {
		return errPnP(webDir)
	}
	bin, err := exec.LookPath(pm.Name)
	if err != nil {
		if pm.Name == "npm" {
			return errNoNpm
		}
		why := "package.json declares it"
		if pm.Lockfile != "" {
			why = "the project has " + pm.Lockfile
		}
		return fmt.Errorf("%s is not on PATH, and %s — install %s (or run `corepack enable`), then run nexus again", pm.Name, why, pm.Name)
	}

	if _, ok := viteBinary(webDir); ok {
		fmt.Fprintf(stdout, "  an earlier dependency install in %s did not finish; running it again\n", webDir)
	}
	fmt.Fprintf(stdout, "  installing frontend dependencies (%s in %s)…\n", pm, webDir)
	if pending != "" {
		if err := os.MkdirAll(filepath.Dir(pending), 0o755); err == nil {
			_ = os.WriteFile(pending, []byte(webDir+"\n"), 0o644)
		}
	}
	cmd := exec.Command(bin, pm.Args...)
	cmd.Dir = webDir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	setProcessGroup(cmd)
	cmd.WaitDelay = installGrace
	if err := runInGroup(ctx, cmd, installGrace); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s in %s interrupted", pm, webDir)
		}
		return fmt.Errorf("%s in %s: %w", pm, webDir, err)
	}
	if pending != "" {
		_ = os.Remove(pending)
	}
	if _, ok := viteBinary(webDir); !ok {
		return fmt.Errorf("%s finished but %s has no node_modules/.bin/vite — add vite to devDependencies in package.json", pm, webDir)
	}
	return nil
}

// installMarkerDir is where installPendingPath keeps its markers; a
// variable so tests can point it at a temporary directory.
var installMarkerDir = func() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "nexus", "install-pending"), nil
}

// installPendingPath is the marker that says an install in webDir started
// and has not finished: written before the package manager runs, removed
// only once it succeeds. An install that was interrupted can leave
// node_modules/.bin/vite behind with half a dependency tree around it, so
// "Vite is there" alone would never retry; the marker makes the next run
// install again.
//
// It lives in the user's cache directory, keyed by the frontend's path,
// not in the project: under node_modules it would be deleted by the very
// `npm ci` it guards (which empties node_modules first), and beside it
// it would be one more file to keep out of version control. "" when
// there is no cache directory — then an interrupted install is not
// detected, as before.
func installPendingPath(webDir string) string {
	dir, err := installMarkerDir()
	if err != nil || dir == "" {
		return ""
	}
	abs, err := filepath.Abs(webDir)
	if err != nil {
		abs = webDir
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(dir, hex.EncodeToString(sum[:12]))
}

// runInGroup starts cmd (already in its own process group, see
// setProcessGroup) and waits for it. When ctx ends first, the whole group
// is stopped — SIGTERM, then SIGKILL after grace — and waited for, so
// nothing the command spawned outlives it.
func runInGroup(ctx context.Context, cmd *exec.Cmd, grace time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	var werr error
	go func() {
		werr = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		stopProcessGroup(cmd.Process.Pid, done, grace, nil)
	}
	return werr
}
