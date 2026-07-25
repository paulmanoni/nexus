package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// pkgIndex caches the `go list` answers the handler codegen needs, so the
// dev loop stops paying for them on every restart.
//
// The codegen used to shell out once for the main package's directory and
// once more per annotated package to get its import path — N+1 subprocesses
// per rebuild, each loading the module graph again. Measured on this repo
// that was 200-350ms of the ~250-420ms overlay step, ahead of the compile
// and with the app's restart waiting on it, while the answers barely ever
// change: import paths move only when a package is added, renamed, or the
// module path itself changes.
//
// So: one `go list -find ./...` per session fills a dir -> import-path map
// (plus the main package's dir), and it's reused until something can
// invalidate it —
//
//   - go.mod / go.sum changed (module path or deps moved), detected by the
//     stamp below;
//   - a lookup misses, which is what a newly created package dir looks
//     like: the index reloads once and retries before giving up.
//
// The module graph behind qualified //@pkg.Func decorators (`go list -deps
// -json`, the expensive one) is cached the same way and stays lazy: it is
// only built when a selector can't be resolved from the annotated file's
// own imports.
//
// Safe for concurrent use; `nexus build` and the dev loop share the index.
type pkgIndex struct {
	mu sync.Mutex

	root  string // absolute scan root the cache was built for
	stamp string // go.mod/go.sum fingerprint

	mainPkgDir string            // dir of the first `package main` under root
	byDir      map[string]string // package dir -> import path
	loaded     bool

	graph       map[string][]string // package name -> import path(s)
	graphErr    error
	graphLoaded bool

	// listRuns counts `go list` invocations; tests assert the cache holds.
	listRuns int
}

// pkgs is the process-wide index. One dev session (or one `nexus build`)
// shares a single cache.
var pkgs = &pkgIndex{}

// mainDir returns the absolute dir of the `package main` under root, or ""
// when there is none (an unbuildable tree included — the caller then just
// skips the import aggregator).
func (i *pkgIndex) mainDir(root string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.ensure(root); err != nil {
		return "", err
	}
	// A main package that moved out from under us is worth one reload:
	// the aggregator writes into this directory.
	if i.mainPkgDir != "" {
		if fi, err := os.Stat(i.mainPkgDir); err != nil || !fi.IsDir() {
			if err := i.reload(root); err != nil {
				return "", err
			}
		}
	}
	return i.mainPkgDir, nil
}

// importPath returns the import path of the package in dir.
func (i *pkgIndex) importPath(root, dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.ensure(root); err != nil {
		return "", err
	}
	if p, ok := i.byDir[abs]; ok {
		return p, nil
	}
	// A miss is what a package created mid-session looks like. Reload once,
	// then fall back to asking about this directory alone — it may live in
	// a nested module that `./...` from root never covers.
	if err := i.reload(root); err != nil {
		return "", err
	}
	if p, ok := i.byDir[abs]; ok {
		return p, nil
	}
	p, err := i.listOne(abs)
	if err != nil {
		return "", err
	}
	i.byDir[abs] = p
	return p, nil
}

// moduleGraph indexes every package in the build graph by its real package
// name. Lazy: only the qualified-decorator path needs it. refresh forces a
// rebuild, which is how a selector miss gets a second chance after the user
// adds a dependency mid-session.
func (i *pkgIndex) moduleGraph(root string, refresh bool) (map[string][]string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.ensure(root); err != nil {
		return nil, err
	}
	if i.graphLoaded && !refresh {
		return i.graph, i.graphErr
	}
	i.graph, i.graphErr = i.loadGraph(i.root)
	i.graphLoaded = true
	return i.graph, i.graphErr
}

// ensure loads the index when it is empty or built for another root, and
// reloads it when the module's own files changed. Caller holds i.mu.
func (i *pkgIndex) ensure(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	stamp := moduleStamp(abs)
	if i.loaded && i.root == abs && i.stamp == stamp {
		return nil
	}
	i.root, i.stamp = abs, stamp
	return i.reload(abs)
}

// reload rebuilds the dir -> import-path map. The module graph is dropped
// with it: whatever invalidated the packages can have moved a dependency
// too. Caller holds i.mu; root is absolute.
func (i *pkgIndex) reload(root string) error {
	byDir, mainDir, err := i.listAll(root)
	if err != nil {
		return err
	}
	i.byDir, i.mainPkgDir, i.loaded = byDir, mainDir, true
	i.graph, i.graphErr, i.graphLoaded = nil, nil, false
	i.stamp = moduleStamp(root)
	return nil
}

// listAll runs the one `go list` the whole cache is built from. -find skips
// import resolution, so it stays cheap on large trees.
//
// A non-zero exit is tolerated as long as some packages came back: a single
// unparsable file (mid-edit, which is the normal state under a file
// watcher) must not blank the index.
func (i *pkgIndex) listAll(root string) (map[string]string, string, error) {
	i.listRuns++
	cmd := exec.Command("go", "list", "-find", "-f", "{{.Name}}\t{{.Dir}}\t{{.ImportPath}}", "./...")
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	byDir := map[string]string{}
	mainDir := ""
	for _, line := range strings.Split(stdout.String(), "\n") {
		parts := strings.Split(strings.TrimSpace(line), "\t")
		if len(parts) != 3 || parts[1] == "" {
			continue
		}
		name, dir, importPath := parts[0], parts[1], parts[2]
		byDir[dir] = importPath
		if name == "main" && mainDir == "" {
			mainDir = dir
		}
	}
	if len(byDir) == 0 && runErr != nil {
		// No module, or the tree is too broken to list at all. Callers treat
		// this as "no aggregator" rather than a hard failure.
		return map[string]string{}, "", nil
	}
	return byDir, mainDir, nil
}

// listOne asks about a single directory — the fallback for a package the
// root's ./... pattern doesn't cover (a nested module).
func (i *pkgIndex) listOne(dir string) (string, error) {
	i.listRuns++
	cmd := exec.Command("go", "list", "-find", "-f", "{{.ImportPath}}", ".")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// loadGraph indexes the build-graph closure by package name. Main packages
// are skipped — they can't be imported by the generated file.
func (i *pkgIndex) loadGraph(root string) (map[string][]string, error) {
	i.listRuns++
	cmd := exec.Command("go", "list", "-deps", "-json", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	graph := map[string][]string{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var p struct{ ImportPath, Name string }
		if err := dec.Decode(&p); err != nil {
			break
		}
		if p.Name == "" || p.Name == "main" {
			continue
		}
		if !slices.Contains(graph[p.Name], p.ImportPath) {
			graph[p.Name] = append(graph[p.Name], p.ImportPath)
		}
	}
	return graph, nil
}

// findModuleRoot walks up from dir to the nearest directory holding a
// go.mod, or "" when there is none.
func findModuleRoot(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	for {
		if _, err := os.Stat(filepath.Join(abs, "go.mod")); err == nil {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return ""
		}
		abs = parent
	}
}

// moduleStamp fingerprints the go.mod / go.sum governing dir, walking up to
// find them. Any change to either (module path, requirements) invalidates
// the cached import paths. Returns "" when no module is found — the index
// then relies on lookup misses to notice staleness.
func moduleStamp(dir string) string {
	modDir := findModuleRoot(dir)
	if modDir == "" {
		return ""
	}
	var b strings.Builder
	for _, name := range []string{"go.mod", "go.sum"} {
		fi, err := os.Stat(filepath.Join(modDir, name))
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "%s:%d:%d;", name, fi.Size(), fi.ModTime().UnixNano())
	}
	return modDir + "|" + b.String()
}
