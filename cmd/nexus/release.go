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
	cliPath string // module path of the CLI to install-check ("" = none)
}

// releaseModule is one go.mod in the repo.
type releaseModule struct {
	Dir      string // relative to the repo root, "." for the root
	Path     string // module path
	Requires bool   // requires the root module
	Publish  bool   // tagged (examples aren't)
}

func newReleaseCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := releaseOptions{remote: "origin"}
	cmd := &cobra.Command{
		Use:   "release <version>",
		Short: "Release the root module and every submodule at one version",
		Long: `Release this repository's Go modules at <version> (vX.Y.Z), in the order
the module graph needs:

  1. check: the version's major matches the root module path, CHANGELOG.md
     has a "## [X.Y.Z]" section, tracked files are committed (go.work aside),
     and no go.work replace points outside the repo
  2. tag the root vX.Y.Z and push it
  3. move every module that requires the root onto vX.Y.Z (go get, go mod
     tidy, go build — with GOPROXY=direct so a tag minutes old resolves),
     commit, tag the published ones <dir>/vX.Y.Z, push
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
	cli := opts.cliPath
	for _, m := range mods {
		if cli == "" && m.Dir == "cmd/nexus" {
			cli = m.Path
		}
	}

	// The plan.
	fmt.Fprintf(stdout, "release %s of %s\n\n", version, rootMod.Path)
	fmt.Fprintf(stdout, "  1. tag %s and push it to %s\n", version, opts.remote)
	var bump []releaseModule
	for _, m := range mods[1:] {
		if m.Requires {
			bump = append(bump, m)
		}
	}
	fmt.Fprintf(stdout, "  2. move %d module(s) onto %s %s:\n", len(bump), rootMod.Path, version)
	for _, m := range bump {
		tag := "not tagged (example)"
		if m.Publish {
			tag = "tag " + m.Dir + "/" + version
		}
		fmt.Fprintf(stdout, "       %-32s %s\n", m.Dir, tag)
	}
	fmt.Fprintf(stdout, "  3. commit \"chore(release): submodules require parent %s\", push\n", version)
	if cli != "" {
		fmt.Fprintf(stdout, "  4. go install %s@%s and run its version\n", cli, version)
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
	var tags []string
	for _, m := range bump {
		dir := filepath.Join(root, m.Dir)
		fmt.Fprintf(stdout, "  %s\n", m.Dir)
		for _, args := range [][]string{{"get", rootMod.Path + "@" + version}, {"mod", "tidy"}, {"build", "./..."}} {
			if _, err := r.run(dir, env, "go", args...); err != nil {
				return fmt.Errorf("nexus release: %s: %w", m.Dir, err)
			}
		}
		if m.Publish {
			tags = append(tags, m.Dir+"/"+version)
		}
	}
	if len(bump) > 0 {
		args := []string{"add"}
		for _, m := range bump {
			args = append(args, filepath.Join(m.Dir, "go.mod"), filepath.Join(m.Dir, "go.sum"))
		}
		if err := step("git", args...); err != nil {
			return err
		}
		if err := step("git", "commit", "-m", "chore(release): submodules require parent "+version); err != nil {
			return err
		}
		for _, t := range tags {
			if err := step("git", "tag", t); err != nil {
				return err
			}
		}
		branch, err := r.run(root, nil, "git", "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return err
		}
		if err := step("git", append([]string{"push", opts.remote, strings.TrimSpace(branch)}, tags...)...); err != nil {
			return err
		}
	}
	if cli != "" {
		if _, err := r.run(root, env, "go", "install", cli+"@"+version); err != nil {
			return fmt.Errorf("nexus release: the release is tagged and pushed, but installing the CLI failed: %w", err)
		}
		out, _ := r.run(root, nil, filepath.Base(strings.TrimSuffix(cli, "/"+semver.Major(version))), "version")
		fmt.Fprintf(stdout, "  installed: %s", out)
	}
	fmt.Fprintf(stdout, "\nreleased %s\n", version)
	return nil
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
		}
		mods = append(mods, m)
		return nil
	})
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
