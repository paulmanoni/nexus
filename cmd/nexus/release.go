package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// A release of a multi-module repo is a fixed dance: tag the root, push
// it, move every submodule that requires it onto the new version, commit,
// tag the published ones, push, and check the CLI installs. nexus release
// does it in that order, from one version argument.

type releaseOptions struct {
	dir     string
	yes     bool
	remote  string
	cliPath string            // module path of the CLI to install-check ("" = none)
	modules map[string]string // dir → version, for modules versioned on their own
}

// releaseModule is one go.mod in the repo.
type releaseModule struct {
	Dir      string   // relative to the repo root, "." for the root
	Path     string   // module path
	Requires bool     // requires the root module
	Publish  bool     // tagged (examples aren't)
	Deps     []string // the repo's modules it requires
}

func newReleaseCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := releaseOptions{remote: "origin"}
	cmd := &cobra.Command{
		Use:   "release <version>",
		Short: "Release the root module and every submodule at one version",
		Long: `Release this repository's Go modules at <version> (vX.Y.Z), in the order
the module graph needs. A module whose path names another major (a v0
module beside a v2 root) is versioned on its own: --module dir=vX.Y.Z tags
it, and the modules requiring it move in a later wave, once its tag is
pushed.


  1. check: the version's major matches the root module path, CHANGELOG.md
     has a "## [X.Y.Z]" section, tracked files are committed (go.work aside),
     and no go.work replace points outside the repo
  2. tag the root vX.Y.Z and push it
  3. move every module that requires a released module onto its new version
     (go get, go mod tidy, go build — with GOPROXY=direct so a tag minutes
     old resolves), commit, tag the published ones <dir>/<version>, push;
     in waves, a module after the modules it requires
  4. go install the CLI at that version and print its version

Without --yes it prints the plan and changes nothing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runRelease(stdout, stderr, args[0], opts, execRunner{})
		},
	}
	cmd.Flags().BoolVar(&opts.yes, "yes", false, "run the release (tags, commits, pushes); without it, print the plan")
	cmd.Flags().StringVar(&opts.dir, "dir", ".", "repository root")
	cmd.Flags().StringVar(&opts.remote, "remote", "origin", "git remote to push to")
	cmd.Flags().StringToStringVar(&opts.modules, "module", nil, "dir=vX.Y.Z: tag a module versioned on its own (a v0 module beside a v2 root) at that version; repeatable")
	cmd.Flags().StringVar(&opts.cliPath, "install", "", "module path of a command to go install and run after the release (default: this repo's cmd/nexus, if any)")
	return cmd
}

// releaseRunner runs commands; tests swap it.
type releaseRunner interface {
	run(dir string, env []string, name string, args ...string) (string, error)
}

type execRunner struct{}

func (execRunner) run(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out.String())
	}
	return out.String(), nil
}

func runRelease(stdout, stderr io.Writer, version string, opts releaseOptions, r releaseRunner) error {
	root, err := filepath.Abs(opts.dir)
	if err != nil {
		return err
	}
	if !semver.IsValid(version) || semver.Prerelease(version) == "" && semver.Canonical(version) != version {
		return fmt.Errorf("nexus release: %q is not a version like v2.0.0", version)
	}
	mods, err := releaseModules(root)
	if err != nil {
		return err
	}
	rootMod := mods[0]
	if _, major, _ := module.SplitPathVersion(rootMod.Path); major != "" && "/"+semver.Major(version) != major ||
		major == "" && semver.Major(version) != "v0" && semver.Major(version) != "v1" {
		return fmt.Errorf("nexus release: %s is a %s version but the root module is %s", version, semver.Major(version), rootMod.Path)
	}
	if err := releaseChecks(root, version, r); err != nil {
		return err
	}
	waves, versions, err := releasePlan(mods, version, opts.modules)
	if err != nil {
		return err
	}
	cli := opts.cliPath
	for _, m := range mods {
		if cli == "" && m.Dir == "cmd/nexus" {
			cli = m.Path
		}
	}
	cliVersion := versions[cli]

	// The plan.
	fmt.Fprintf(stdout, "release %s of %s\n\n", version, rootMod.Path)
	fmt.Fprintf(stdout, "  1. tag %s and push it to %s\n", version, opts.remote)
	n := 2
	for _, wave := range waves {
		fmt.Fprintf(stdout, "  %d. move %d module(s) onto the new versions, commit, tag, push:\n", n, len(wave))
		n++
		for _, m := range wave {
			tag := "not tagged (example)"
			switch {
			case versions[m.Path] != "":
				tag = "tag " + m.Dir + "/" + versions[m.Path]
			case m.Publish:
				tag = "not tagged (versioned on its own: --module " + m.Dir + "=vX.Y.Z tags it)"
			}
			fmt.Fprintf(stdout, "       %-32s %s\n", m.Dir, tag)
		}
	}
	if cli != "" && cliVersion != "" {
		fmt.Fprintf(stdout, "  %d. go install %s@%s and run its version\n", n, cli, cliVersion)
	}
	if !opts.yes {
		fmt.Fprintln(stdout, "\nnothing done — run again with --yes to release")
		return nil
	}

	env := []string{"GOWORK=off", "GOPROXY=direct", "GOFLAGS=-mod=mod",
		"GONOSUMDB=" + privatePattern(rootMod.Path), "GONOPROXY=" + privatePattern(rootMod.Path)}
	step := func(name string, args ...string) error {
		fmt.Fprintf(stdout, "  $ %s %s\n", name, strings.Join(args, " "))
		_, err := r.run(root, nil, name, args...)
		return err
	}
	if err := step("git", "tag", version); err != nil {
		return err
	}
	if err := step("git", "push", opts.remote, version); err != nil {
		return err
	}
	branch, err := r.run(root, nil, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	for _, wave := range waves {
		var tags, dirs []string
		add := []string{"add"}
		for _, m := range wave {
			dir := filepath.Join(root, m.Dir)
			fmt.Fprintf(stdout, "  %s\n", m.Dir)
			get := []string{"get"}
			for _, d := range m.Deps {
				if versions[d] != "" {
					get = append(get, d+"@"+versions[d])
				}
			}
			for _, args := range [][]string{get, {"mod", "tidy"}, {"build", "./..."}} {
				if _, err := r.run(dir, env, "go", args...); err != nil {
					return fmt.Errorf("nexus release: %s: %w", m.Dir, err)
				}
			}
			if v := versions[m.Path]; v != "" {
				tags = append(tags, m.Dir+"/"+v)
			}
			dirs = append(dirs, m.Dir)
			add = append(add, filepath.Join(m.Dir, "go.mod"), filepath.Join(m.Dir, "go.sum"))
		}
		msg := "chore(release): submodules require parent " + version
		if len(waves) > 1 {
			msg = "chore(release): " + strings.Join(dirs, ", ") + " require parent " + version
		}
		if err := step("git", add...); err != nil {
			return err
		}
		if err := step("git", "commit", "-m", msg); err != nil {
			return err
		}
		for _, t := range tags {
			if err := step("git", "tag", t); err != nil {
				return err
			}
		}
		if err := step("git", append([]string{"push", opts.remote, strings.TrimSpace(branch)}, tags...)...); err != nil {
			return err
		}
	}
	if cli != "" && cliVersion != "" {
		if _, err := r.run(root, env, "go", "install", cli+"@"+cliVersion); err != nil {
			return fmt.Errorf("nexus release: the release is tagged and pushed, but installing the CLI failed: %w", err)
		}
		out, _ := r.run(root, nil, filepath.Base(strings.TrimSuffix(cli, "/"+semver.Major(cliVersion))), "version")
		fmt.Fprintf(stdout, "  installed: %s", out)
	}
	fmt.Fprintf(stdout, "\nreleased %s\n", version)
	return nil
}

// releasePlan is the modules a release moves, in waves — a module comes
// after every module it requires that the release tags, whose tag must be
// pushed before go get can resolve it — and the version each is tagged
// at: the root's, or for a module whose path names another major (a v0
// module beside a v2 root) the one --module gives, else none.
func releasePlan(mods []releaseModule, version string, given map[string]string) ([][]releaseModule, map[string]string, error) {
	versions := map[string]string{mods[0].Path: version}
	byDir := map[string]bool{}
	for _, m := range mods[1:] {
		byDir[m.Dir] = true
	}
	for dir, v := range given {
		if !byDir[dir] {
			return nil, nil, fmt.Errorf("nexus release: --module %s: no module in %s", dir, dir)
		}
		if !semver.IsValid(v) || semver.Canonical(v) != v && semver.Prerelease(v) == "" {
			return nil, nil, fmt.Errorf("nexus release: --module %s=%s: not a version like v0.2.0", dir, v)
		}
	}
	for _, m := range mods[1:] {
		if !m.Publish {
			continue
		}
		_, major, _ := module.SplitPathVersion(m.Path)
		own := major == "" && (semver.Major(version) == "v0" || semver.Major(version) == "v1") || major == "/"+semver.Major(version)
		switch v, ok := given[m.Dir]; {
		case ok:
			if _, major, _ := module.SplitPathVersion(m.Path); major != "" && major != "/"+semver.Major(v) ||
				major == "" && semver.Major(v) != "v0" && semver.Major(v) != "v1" {
				return nil, nil, fmt.Errorf("nexus release: --module %s=%s: %s takes %s versions", m.Dir, v, m.Path, strings.TrimPrefix(orDefault(major, "/v0 or v1"), "/"))
			}
			versions[m.Path] = v
		case own:
			versions[m.Path] = version
		}
	}
	// Waves: a module moves when it requires a module the release tags.
	wave := map[string]int{mods[0].Path: 0}
	var waves [][]releaseModule
	for changed := true; changed; {
		changed = false
		for _, m := range mods[1:] {
			if _, done := wave[m.Path]; done {
				continue
			}
			w, moves, ready := 0, false, true
			for _, d := range m.Deps {
				if versions[d] == "" {
					continue
				}
				dw, ok := wave[d]
				if !ok {
					if movesAtAll(mods, d, versions) {
						ready = false
					}
					continue
				}
				moves = true
				w = max(w, dw+1)
			}
			if !ready || !moves {
				continue
			}
			wave[m.Path] = w
			for len(waves) < w {
				waves = append(waves, nil)
			}
			waves[w-1] = append(waves[w-1], m)
			changed = true
		}
	}
	for path := range versions {
		if _, ok := wave[path]; !ok {
			delete(versions, path) // nothing it requires is released
		}
	}
	return waves, versions, nil
}

// movesAtAll is whether the module at path requires, directly or not, a
// module the release tags: a module that does takes its own wave first.
func movesAtAll(mods []releaseModule, path string, versions map[string]string) bool {
	seen := map[string]bool{}
	var visit func(string) bool
	visit = func(p string) bool {
		if seen[p] {
			return false
		}
		seen[p] = true
		for _, m := range mods {
			if m.Path != p {
				continue
			}
			for _, d := range m.Deps {
				if d == mods[0].Path || versions[d] != "" && visit(d) {
					return true
				}
			}
		}
		return false
	}
	return visit(path)
}

// releaseChecks refuses a release that would publish something broken or
// unrecorded.
func releaseChecks(root, version string, r releaseRunner) error {
	var problems []string
	changelog, _ := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if !bytes.Contains(changelog, []byte("## ["+strings.TrimPrefix(version, "v")+"]")) {
		problems = append(problems, "CHANGELOG.md has no \"## ["+strings.TrimPrefix(version, "v")+"]\" section")
	}
	status, err := r.run(root, nil, "git", "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(status), "\n") {
		if f := strings.TrimSpace(line); f != "" && !strings.HasSuffix(f, "go.work") && !strings.HasSuffix(f, "go.work.sum") {
			problems = append(problems, "uncommitted change: "+f)
		}
	}
	if out, err := r.run(root, nil, "git", "rev-parse", "-q", "--verify", "refs/tags/"+version); err == nil && strings.TrimSpace(out) != "" {
		problems = append(problems, version+" is already tagged — a published tag never moves; release the next version")
	}
	if raw, err := os.ReadFile(filepath.Join(root, "go.work")); err == nil {
		if wf, err := modfile.ParseWork("go.work", raw, nil); err == nil {
			for _, rep := range wf.Replace {
				p := rep.New.Path
				if !modfile.IsDirectoryPath(p) {
					continue
				}
				if !filepath.IsAbs(p) {
					p = filepath.Join(root, p)
				}
				if rel, err := filepath.Rel(root, p); err != nil || strings.HasPrefix(rel, "..") {
					problems = append(problems, "go.work replaces "+rep.Old.Path+" with "+rep.New.Path+", outside the repo — release that module first and require it")
				}
			}
		}
	}
	if len(problems) > 0 {
		return errors.New("nexus release: not ready:\n  - " + strings.Join(problems, "\n  - "))
	}
	return nil
}

var exampleDir = regexp.MustCompile(`(^|/)(examples?|testdata)(/|$)`)

// releaseModules lists the repo's modules, the root first.
func releaseModules(root string) ([]releaseModule, error) {
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("nexus release: %s is not a module root: %w", root, err)
	}
	rootPath := modfile.ModulePath(raw)
	mods := []releaseModule{{Dir: ".", Path: rootPath, Publish: true}}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if p != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" || filepath.Dir(p) == root {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mf, err := modfile.Parse(p, raw, nil)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		rel = filepath.ToSlash(rel)
		m := releaseModule{Dir: rel, Path: mf.Module.Mod.Path, Publish: !exampleDir.MatchString(rel)}
		for _, req := range mf.Require {
			if req.Mod.Path == rootPath {
				m.Requires = true
			}
			m.Deps = append(m.Deps, req.Mod.Path)
		}
		mods = append(mods, m)
		return nil
	})
	inRepo := map[string]bool{}
	for _, m := range mods {
		inRepo[m.Path] = true
	}
	for i := range mods {
		deps := mods[i].Deps[:0]
		for _, d := range mods[i].Deps {
			if inRepo[d] {
				deps = append(deps, d)
			}
		}
		mods[i].Deps = deps
	}
	sort.Slice(mods[1:], func(i, j int) bool { return mods[1+i].Dir < mods[1+j].Dir })
	return mods, err
}

// privatePattern is the GONOSUMDB/GONOPROXY pattern for a module path's
// owner: github.com/owner/*.
func privatePattern(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) >= 2 {
		return strings.Join(parts[:2], "/") + "/*"
	}
	return path
}
