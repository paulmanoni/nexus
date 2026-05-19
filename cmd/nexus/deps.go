package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paulmanoni/nexus/frontend/deps/fetcher"
	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

// depsContext bundles the three values every deps subcommand needs:
// the project's lockfile path, an open Store, and a Fetcher wired to
// the configured registry. Built once per command invocation by
// newDepsContext; the subcommand-specific code receives it and
// focuses on the verb.
type depsContext struct {
	lockfilePath string
	store        *store.Store
	fetcher      *fetcher.Fetcher
	stdout       io.Writer
	stderr       io.Writer
}

// newDepsContext resolves the cache root + registry from env vars,
// opens the store, loads or creates the project lockfile, and
// returns everything ready to use.
//
// Env overrides:
//
//	NEXUS_CACHE      cache root (default: ~/.nexus/cache)
//	NEXUS_REGISTRY   registry base URL (default: https://esm.sh)
//
// The lockfile is loaded LATER per-command — some commands (gc)
// don't need the project's lockfile, and even those that do want
// to control whether a missing lockfile is an error vs autocreate.
func newDepsContext(stdout, stderr io.Writer) (*depsContext, error) {
	cacheRoot := os.Getenv("NEXUS_CACHE")
	if cacheRoot == "" {
		cacheRoot = store.DefaultRoot()
	}
	st, err := store.New(cacheRoot)
	if err != nil {
		return nil, fmt.Errorf("nexus deps: open cache at %s: %w", cacheRoot, err)
	}
	registry := os.Getenv("NEXUS_REGISTRY")
	if registry == "" {
		registry = fetcher.DefaultRegistry
	}
	f := fetcher.New(st, registry)
	// Mark the framework-singleton peer deps so esm.sh-served
	// bundles leave them as bare imports rather than embedding
	// their own copy. Without this, `nexus add @vue-flow/core`
	// (or any other vue-consuming pkg) would bring along its
	// own pinned Vue alongside the project's `nexus add vue`,
	// and Vue's reactivity globals would split between the two
	// copies at runtime — "Fa is null" / "ce undefined" in
	// the bundle.
	//
	// The list is intentionally short: only the small set of
	// framework-singletons where dual-version bundling is a
	// known runtime failure. Other libs that happen to be
	// peer-dep'd by something CAN coexist at multiple versions
	// without splitting state.
	f.External = []string{"vue", "react", "react-dom"}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("nexus deps: getwd: %w", err)
	}
	return &depsContext{
		lockfilePath: filepath.Join(cwd, lockfile.Filename),
		store:        st,
		fetcher:      f,
		stdout:       stdout,
		stderr:       stderr,
	}, nil
}

// loadOrNewLockfile is the common helper for commands that mutate
// the lockfile. Loads if present, returns a fresh empty one
// otherwise, so first-run UX (no lockfile yet) is transparent.
func (c *depsContext) loadOrNewLockfile() (*lockfile.File, error) {
	return lockfile.LoadOrNew(c.lockfilePath)
}

// pinnedVersionsFrom extracts a name→version map from the lockfile's
// top-level entries. Used to seed the fetcher's PinnedVersions so
// transitive bare-spec recursion can't drift onto a different
// version than the project already committed to.
//
// Skips entries with empty Version (legacy lockfiles created before
// X-ESM-Path canonicalization landed) — there's nothing to pin
// against and emitting "vue@" as a spec breaks downstream code.
// Users with such lockfiles need to re-add to get a clean state.
func pinnedVersionsFrom(lf *lockfile.File) map[string]string {
	if lf == nil || len(lf.Packages) == 0 {
		return nil
	}
	out := make(map[string]string, len(lf.Packages))
	for _, p := range lf.Packages {
		if p.Spec == "" || p.Version == "" {
			continue
		}
		out[p.Spec] = p.Version
	}
	return out
}

func (c *depsContext) saveLockfile(lf *lockfile.File) error {
	return lf.Save(c.lockfilePath)
}

// --- subcommands ----------------------------------------------------

// newAddCmd registers `nexus add <pkg>[@version] [<pkg>...]`. Fetches
// each spec via the fetcher (including transitive recursion), adds
// the resolved package + every transitive into the lockfile,
// re-saves the lockfile.
func newAddCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "add <spec> [<spec>...]",
		Short: "Add a frontend dependency to nexus.lock and the cache",
		Long: `Fetches the package from the configured registry (default https://esm.sh),
follows transitive imports, hashes + caches every file in ~/.nexus/cache,
and pins everything in nexus.lock.

Examples:
  nexus add vue
  nexus add react react-dom
  nexus add vue@3.4.21`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAdd(cmd.Context(), stdout, stderr, args)
		},
	}
}

func runAdd(ctx context.Context, stdout, stderr io.Writer, specs []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	dc, err := newDepsContext(stdout, stderr)
	if err != nil {
		return err
	}
	lf, err := dc.loadOrNewLockfile()
	if err != nil {
		return err
	}
	// Hand the fetcher the project's existing top-level pins so
	// transitive `import "vue"` recursion respects them. Without
	// this, `nexus add @vue-flow/core` would recurse into `import
	// "vue"` (bare, thanks to External) and pick up whatever esm.sh
	// is serving as latest stable today — diverging from the user's
	// previously pinned vue@<x.y.z>.
	dc.fetcher.PinnedVersions = pinnedVersionsFrom(lf)

	// resolvedVersions records the version each spec landed on after
	// fetch + X-ESM-Path canonicalization, so the package.json
	// update below uses the actual pinned version (not whatever
	// version the user happened to type — which may have been
	// empty, a range, or out of date).
	resolvedVersions := make(map[string]string, len(specs))
	for _, spec := range specs {
		fmt.Fprintf(stdout, "nexus add %s — fetching from %s\n", spec, dc.fetcher.Registry)
		res, err := dc.fetcher.Fetch(ctx, spec)
		if err != nil {
			return fmt.Errorf("nexus add %s: %w", spec, err)
		}
		lf.Add(res.Root)
		for _, dep := range res.Transitive {
			lf.Add(dep)
		}
		resolvedVersions[res.Root.Spec] = res.Root.Version
		fmt.Fprintf(stdout, "  resolved %s @ %s — %s (+%d transitive)\n",
			res.Root.Spec, res.Root.Version, res.Root.Integrity[:23]+"…",
			len(res.Transitive))
	}

	if err := dc.saveLockfile(lf); err != nil {
		return fmt.Errorf("nexus add: save %s: %w", dc.lockfilePath, err)
	}
	fmt.Fprintf(stdout, "wrote %s\n", dc.lockfilePath)

	// Mirror the new pins into package.json so IDEs / Dependabot /
	// Renovate can see what the project depends on without parsing
	// nexus.lock. Each spec lands under "dependencies" keyed by its
	// bare name with the resolved version (^-prefixed by addDep);
	// transitives are NOT recorded — package.json is the
	// human-facing intent file, nexus.lock is the full resolution.
	//
	// Best-effort: a missing package.json is auto-created at the
	// project root; explicit save errors surface, but a read error
	// other than "not exist" is swallowed (we'd rather complete the
	// add than fail because the user's package.json had a typo).
	cwd, _ := os.Getwd()
	pjPath := filepath.Join(cwd, packageJSONFilename)
	if pj, perr := loadPackageJSON(pjPath); perr == nil {
		var changed int
		for _, spec := range specs {
			name, _ := parseSpecForPJ(spec)
			version := resolvedVersions[name]
			before := pj.Dependencies[name]
			pj.addDep(name, version)
			if pj.Dependencies[name] != before {
				changed++
			}
		}
		if changed > 0 {
			if err := pj.save(pjPath); err != nil {
				fmt.Fprintf(stderr, "warning: couldn't update package.json: %v\n", err)
			} else {
				fmt.Fprintf(stdout, "updated %s (%d dep%s)\n", packageJSONFilename, changed, plural(changed))
			}
		}
	}

	// Append ambient module declarations for each newly-added
	// SPEC (not its transitives) to the IDE shims file. Without
	// this the user gets TS2307 "Cannot find module 'vue'" on
	// imports, because the IDE has no node_modules tree to walk.
	// Best-effort: silent skip when nexus-shims.d.ts doesn't
	// exist (project either pre-dates the new scaffold or
	// doesn't use the IDE shims layer).
	shimsPath := filepath.Join(cwd, "nexus-shims.d.ts")
	if _, statErr := os.Stat(shimsPath); statErr == nil {
		var added int
		for _, spec := range specs {
			bare, _ := parseSpecForPJ(spec)
			if appendShimIfMissing(shimsPath, bare) {
				added++
			}
		}
		if added > 0 {
			fmt.Fprintf(stdout, "appended %d module declaration(s) to nexus-shims.d.ts\n", added)
		}
	}

	// Fetch the real TypeScript declarations from esm.sh and write
	// them under node_modules/<pkg>/ so the IDE gets full
	// IntelliSense (autocomplete, parameter types, jump-to-defn)
	// rather than the shim file's `any`-typed fallback. Best-effort
	// — failures here log a warning but don't fail the add, since
	// the shim layer keeps the type checker quiet either way.
	//
	// We only fetch types for the SPECS the user passed (not their
	// transitive deps) — the type files themselves carry their own
	// imports which fetchAll follows recursively, mirroring the
	// minimum slice of the dep graph the user's code can reach.
	typeSpecs := make(map[string]string, len(specs))
	for _, spec := range specs {
		name, _ := parseSpecForPJ(spec)
		if v, ok := resolvedVersions[name]; ok && v != "" {
			typeSpecs[name] = v
		}
	}
	if len(typeSpecs) > 0 {
		tf := newTypeFetcher()
		written, _ := tf.fetchAll(ctx, typeSpecs, cwd, stderr)
		if written > 0 {
			fmt.Fprintf(stdout, "fetched %d type file%s into ./node_modules/\n", written, plural(written))
			gitignoreEnsureNodeModules(cwd)
		}
		// Real types just landed for these specs — drop any
		// `declare module "<spec>";` ambient shim that would
		// otherwise shadow them and force tsserver to resolve the
		// import as `any`.
		prunable := make([]string, 0, len(typeSpecs))
		for name := range typeSpecs {
			prunable = append(prunable, name)
		}
		if n := pruneShimsForResolvedTypes(cwd, prunable); n > 0 {
			fmt.Fprintf(stdout, "removed %d shadowing module shim%s from nexus-shims.d.ts\n", n, plural(n))
		}
	}

	return nil
}

// parseSpecForPJ splits a CLI spec into (bare name, version). Same
// shape as lockfile.SplitKey but lives here to avoid a back-edge
// dependency from cmd/nexus into a package that already imports it.
func parseSpecForPJ(spec string) (name, version string) {
	if i := strings.LastIndex(spec, "@"); i > 0 {
		return spec[:i], spec[i+1:]
	}
	return spec, ""
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// appendShimIfMissing adds `declare module "<spec>";` to path
// when no such line is already there. Returns true iff a line
// was written.
//
// O(file-size) per call — read whole file, scan, append. Fine
// for a shims file that tops out at a few dozen entries; if it
// ever grows past that we'd switch to a sidecar `.nexus-deps`
// state file the scaffold consults at build time.
func appendShimIfMissing(path, spec string) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	line := `declare module "` + spec + `";`
	if strings.Contains(string(body), line) {
		return false
	}
	out := body
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, []byte(line+"\n")...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return false
	}
	return true
}

// pruneShimsForResolvedTypes removes `declare module "<spec>";` lines
// from nexus-shims.d.ts for any spec whose REAL types now exist under
// node_modules/<spec>/package.json. An empty-body ambient declaration
// shadows real types in tsserver's resolution — it gets matched first
// and resolves the import to `any`, which is exactly the bug users
// hit ("import works, but no IntelliSense"). Pruning the shim after
// types land lets tsserver fall through to the real node_modules entry.
//
// Best-effort: silent skip when nexus-shims.d.ts doesn't exist. Specs
// without a node_modules entry (type fetch failed, no X-TypeScript-
// Types header) keep their shim so TS2307 stays silenced even without
// real types.
func pruneShimsForResolvedTypes(projectRoot string, specs []string) int {
	shimsPath := filepath.Join(projectRoot, "nexus-shims.d.ts")
	body, err := os.ReadFile(shimsPath)
	if err != nil {
		return 0
	}
	original := string(body)
	updated := original
	var removed int
	for _, spec := range specs {
		pjPath := filepath.Join(projectRoot, "node_modules", filepath.FromSlash(spec), "package.json")
		if _, err := os.Stat(pjPath); err != nil {
			continue
		}
		line := `declare module "` + spec + `";`
		// Match the bare line ± trailing newline so we strip the
		// blank line it left behind. Two passes (with-newline and
		// without) handle both end-of-file and mid-file shims.
		for _, variant := range []string{line + "\n", line} {
			if strings.Contains(updated, variant) {
				updated = strings.Replace(updated, variant, "", 1)
				removed++
				break
			}
		}
	}
	if updated == original {
		return 0
	}
	if err := os.WriteFile(shimsPath, []byte(updated), 0o644); err != nil {
		return 0
	}
	return removed
}

// newRemoveCmd registers `nexus remove <pkg>`. Removes the entry
// (and any transitive entries not referenced by other roots) from
// nexus.lock. Doesn't touch the cache — gc handles that.
func newRemoveCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <spec> [<spec>...]",
		Short: "Remove a frontend dependency from nexus.lock",
		Long: `Drops the entry from nexus.lock. The cache on disk is left
untouched — run "nexus gc" to reclaim space from packages no
project still references.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRemove(stdout, stderr, args)
		},
	}
}

func runRemove(stdout, stderr io.Writer, specs []string) error {
	dc, err := newDepsContext(stdout, stderr)
	if err != nil {
		return err
	}
	lf, err := dc.loadOrNewLockfile()
	if err != nil {
		return err
	}

	removed := 0
	for _, spec := range specs {
		// Best-effort: try the spec as a full key first
		// ("vue@3.4.21"), then fall back to spec-only match
		// (drop any entry whose Spec field equals the arg).
		if lf.Remove(spec) {
			fmt.Fprintf(stdout, "removed %s\n", spec)
			removed++
			continue
		}
		var toRemove []string
		for k, p := range lf.Packages {
			if p.Spec == spec {
				toRemove = append(toRemove, k)
			}
		}
		if len(toRemove) == 0 {
			fmt.Fprintf(stderr, "nexus remove: %s not in lockfile, skipping\n", spec)
			continue
		}
		for _, k := range toRemove {
			lf.Remove(k)
			fmt.Fprintf(stdout, "removed %s\n", k)
			removed++
		}
	}
	if removed == 0 {
		return nil
	}
	if err := dc.saveLockfile(lf); err != nil {
		return err
	}
	// Drop matching entries from package.json too — same source-of-
	// truth semantics as `nexus add`. Best-effort: don't fail the
	// remove if package.json is broken or missing.
	cwd, _ := os.Getwd()
	pjPath := filepath.Join(cwd, packageJSONFilename)
	if pj, perr := loadPackageJSON(pjPath); perr == nil {
		var changed int
		for _, spec := range specs {
			name, _ := parseSpecForPJ(spec)
			if pj.removeDep(name) {
				changed++
			}
		}
		if changed > 0 {
			if err := pj.save(pjPath); err != nil {
				fmt.Fprintf(stderr, "warning: couldn't update package.json: %v\n", err)
			} else {
				fmt.Fprintf(stdout, "updated %s (-%d dep%s)\n", packageJSONFilename, changed, plural(changed))
			}
		}
	}
	return nil
}

// newInstallCmd registers `nexus install`. Walks every entry in the
// lockfile and ensures the cache has the corresponding blob. Used
// on fresh clones and in CI where ~/.nexus/cache starts empty.
func newInstallCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Sync the cache to the lockfile (fetch any missing blobs)",
		Long: `Walks nexus.lock and ensures every pinned entry has a corresponding
cached blob in ~/.nexus/cache. Designed for fresh clones and CI
where the cache starts empty.

Does not modify nexus.lock. To bump versions, use "nexus update".`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInstall(cmd.Context(), stdout, stderr)
		},
	}
}

func runInstall(ctx context.Context, stdout, stderr io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	dc, err := newDepsContext(stdout, stderr)
	if err != nil {
		return err
	}

	// Read package.json (the human-facing dep spec) and figure out
	// which top-level deps are missing from the lockfile. This is
	// what makes a fresh clone work: a user with package.json but no
	// nexus.lock can run `nexus install` and have everything fetched
	// in one shot — same UX as `npm install`.
	cwd, _ := os.Getwd()
	pj, _ := loadPackageJSON(filepath.Join(cwd, packageJSONFilename))

	lf, err := lockfile.Load(dc.lockfilePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// No lockfile yet. If package.json has deps we can build one
		// from scratch by adding each. Otherwise nothing to do.
		if pj == nil || len(pj.Dependencies) == 0 {
			fmt.Fprintln(stderr, "nexus install: no nexus.lock and no package.json deps — run `nexus add <pkg>` first")
			return nil
		}
		lf = lockfile.New()
	}

	// Phase 1: ensure every package.json dep is in the lockfile. New
	// entries get added via the fetcher (which writes blobs into the
	// store as a side effect). Existing entries are left untouched
	// for now — Phase 2 verifies cache integrity.
	dc.fetcher.PinnedVersions = pinnedVersionsFrom(lf)
	if pj != nil {
		added := 0
		for name, ver := range pj.Dependencies {
			// Look up by spec name; if any version of this dep is
			// already pinned, skip — `nexus update` is the way to
			// bump, not `nexus install`.
			if hasSpec(lf, name) {
				continue
			}
			clean := strings.TrimLeft(ver, "^~>=< ")
			spec := name
			if clean != "" {
				spec = name + "@" + clean
			}
			fmt.Fprintf(stdout, "nexus install: + %s\n", spec)
			res, ferr := dc.fetcher.Fetch(ctx, spec)
			if ferr != nil {
				return fmt.Errorf("nexus install %s: %w", spec, ferr)
			}
			lf.Add(res.Root)
			for _, dep := range res.Transitive {
				lf.Add(dep)
			}
			added++
		}
		if added > 0 {
			if err := dc.saveLockfile(lf); err != nil {
				return fmt.Errorf("nexus install: save lockfile: %w", err)
			}
		}
	}

	// Phase 2: walk the lockfile and ensure each pinned blob is in
	// the cache. Designed for CI / fresh clones where ~/.nexus/cache
	// starts empty but nexus.lock is committed.
	fetched, skipped := 0, 0
	for key, pkg := range lf.Packages {
		hex := fetcher.IntegrityHex(pkg.Integrity)
		if dc.store.Has(hex) {
			skipped++
			continue
		}
		fmt.Fprintf(stdout, "nexus install: fetching %s\n", key)
		// Use the RESOLVED URL (pinned) rather than re-resolving
		// the bare spec — the whole point of the lockfile is to
		// pin against version drift.
		if _, err := dc.fetcher.Fetch(ctx, pkg.Resolved); err != nil {
			return fmt.Errorf("nexus install %s: %w", key, err)
		}
		fetched++
	}
	fmt.Fprintf(stdout, "nexus install: %d fetched, %d already cached\n", fetched, skipped)

	// Refresh the node_modules type tree for every top-level pkg
	// the project depends on, so a fresh clone (where node_modules
	// is gitignored) gets IntelliSense on the first install rather
	// than after the first re-add. Best-effort: log warnings on
	// per-pkg failures and keep going.
	//
	// We feed the type fetcher only TOP-LEVEL specs (lockfile
	// entries whose spec name matches a package.json dep), not
	// every transitive — the type files' own imports carry the
	// type-graph reachability and the recursive walker mirrors
	// what's actually needed.
	if pj != nil && len(pj.Dependencies) > 0 {
		topLevel := make(map[string]string, len(pj.Dependencies))
		for name := range pj.Dependencies {
			for _, p := range lf.Packages {
				if p.Spec == name && p.Version != "" {
					topLevel[name] = p.Version
					break
				}
			}
		}
		if len(topLevel) > 0 {
			tf := newTypeFetcher()
			written, _ := tf.fetchAll(ctx, topLevel, cwd, stderr)
			if written > 0 {
				fmt.Fprintf(stdout, "nexus install: %d type file%s fetched into ./node_modules/\n", written, plural(written))
				gitignoreEnsureNodeModules(cwd)
			}
			// Always run shim pruning (not just on first-fetch),
			// because existing projects upgrading from earlier
			// nexus versions have shims AND types coexisting —
			// the shim shadows the real types until we strip it.
			prunable := make([]string, 0, len(topLevel))
			for name := range topLevel {
				prunable = append(prunable, name)
			}
			if n := pruneShimsForResolvedTypes(cwd, prunable); n > 0 {
				fmt.Fprintf(stdout, "nexus install: removed %d shadowing module shim%s from nexus-shims.d.ts\n", n, plural(n))
			}
		}
	}
	return nil
}

// hasSpec reports whether any lockfile entry has the given bare
// spec name. Used by `nexus install` to decide whether a package.json
// dep needs fetching or is already pinned.
func hasSpec(lf *lockfile.File, name string) bool {
	if lf == nil {
		return false
	}
	for _, p := range lf.Packages {
		if p.Spec == name {
			return true
		}
	}
	return false
}

// newUpdateCmd registers `nexus update [<spec>...]`. Re-resolves
// the supplied specs (or all specs when none supplied) against the
// registry, picking up any newer version available, and writes the
// new pin to nexus.lock.
func newUpdateCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "update [<spec>...]",
		Short: "Bump pinned versions to whatever the registry currently serves",
		Long: `Re-resolves each named spec (or every spec in the lockfile when none
named) against the registry and updates nexus.lock with the new
resolved version. Cached blobs for the OLD versions stay on disk
until "nexus gc" runs.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(cmd.Context(), stdout, stderr, args)
		},
	}
}

func runUpdate(ctx context.Context, stdout, stderr io.Writer, specs []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	dc, err := newDepsContext(stdout, stderr)
	if err != nil {
		return err
	}
	lf, err := lockfile.Load(dc.lockfilePath)
	if err != nil {
		return err
	}

	// When no specs supplied: rebuild the list from existing
	// top-level entries (everything in the lockfile is "top
	// level" in our v0.1 model — no nested resolution).
	if len(specs) == 0 {
		seen := map[string]bool{}
		for _, p := range lf.Packages {
			if !seen[p.Spec] {
				specs = append(specs, p.Spec)
				seen[p.Spec] = true
			}
		}
		sort.Strings(specs)
	}

	for _, spec := range specs {
		fmt.Fprintf(stdout, "nexus update: re-resolving %s\n", spec)
		res, err := dc.fetcher.Fetch(ctx, spec)
		if err != nil {
			return fmt.Errorf("nexus update %s: %w", spec, err)
		}
		// Drop the OLD entry for this spec (whatever its old
		// version) and add the new one. Transitive deps may have
		// shifted too — those are added as-is.
		for k, p := range lf.Packages {
			if p.Spec == spec {
				lf.Remove(k)
			}
		}
		lf.Add(res.Root)
		for _, dep := range res.Transitive {
			lf.Add(dep)
		}
		fmt.Fprintf(stdout, "  updated %s → %s\n", spec, res.Root.Version)
	}
	return dc.saveLockfile(lf)
}

// newVendorCmd registers `nexus vendor`. Copies every blob the
// project's lockfile references into ./vendor/nexus/ so the project
// builds without network access (CI, air-gapped environments).
func newVendorCmd(stdout, stderr io.Writer) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "vendor",
		Short: "Copy cached blobs into ./vendor/nexus/ for air-gapped builds",
		Long: `Writes every blob referenced by nexus.lock into ./vendor/nexus/, with
filenames keyed by the resolved URL. Subsequent builds with
NEXUS_CACHE=./vendor/nexus run without hitting the registry.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVendor(stdout, stderr, out)
		},
	}
	cmd.Flags().StringVar(&out, "out", "vendor/nexus", "directory to write vendored blobs into")
	return cmd
}

func runVendor(stdout, stderr io.Writer, outDir string) error {
	dc, err := newDepsContext(stdout, stderr)
	if err != nil {
		return err
	}
	lf, err := lockfile.Load(dc.lockfilePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("nexus vendor: mkdir %s: %w", outDir, err)
	}

	copied := 0
	for key, pkg := range lf.Packages {
		blob, _, err := dc.store.Get(pkg.Resolved)
		if err != nil {
			return fmt.Errorf("nexus vendor: %s blob not in cache: %w", key, err)
		}
		dest := filepath.Join(outDir, fetcher.IntegrityHex(pkg.Integrity))
		if err := copyFile(blob, dest); err != nil {
			return fmt.Errorf("nexus vendor: copy %s → %s: %w", blob, dest, err)
		}
		copied++
	}
	fmt.Fprintf(stdout, "nexus vendor: copied %d blobs to %s\n", copied, outDir)
	return nil
}

// newGCCmd registers `nexus gc`. Walks the store's blob set and
// removes any blob not referenced by any URL → blob mapping (or,
// with --keep <lockfile>, by any of the supplied lockfiles).
func newGCCmd(stdout, stderr io.Writer) *cobra.Command {
	var keep []string
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Reclaim cache space from unreferenced blobs",
		Long: `Removes blobs in ~/.nexus/cache that no URL mapping (or no supplied
lockfile via --keep) references. By default scans only the cache's
own url-index; pass one or more --keep <lockfile.path> to keep
blobs referenced by specific projects too.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGC(stdout, stderr, keep)
		},
	}
	cmd.Flags().StringSliceVar(&keep, "keep", nil, "lockfile path(s) whose pinned blobs should be preserved")
	return cmd
}

func runGC(stdout, stderr io.Writer, keepLockfiles []string) error {
	dc, err := newDepsContext(stdout, stderr)
	if err != nil {
		return err
	}
	// Mark phase: collect every reachable content hash.
	reachable := map[string]bool{}
	_ = dc.store.EachURL(func(meta store.Metadata) error {
		reachable[meta.ContentSHA256] = true
		return nil
	})
	for _, lfPath := range keepLockfiles {
		lf, err := lockfile.Load(lfPath)
		if err != nil {
			return fmt.Errorf("nexus gc: load %s: %w", lfPath, err)
		}
		for _, pkg := range lf.Packages {
			if h := fetcher.IntegrityHex(pkg.Integrity); h != "" {
				reachable[h] = true
			}
		}
	}

	// Sweep phase.
	removed := 0
	var freed int64
	_ = dc.store.EachBlob(func(hash, path string) error {
		if reachable[hash] {
			return nil
		}
		info, err := os.Stat(path)
		if err == nil {
			freed += info.Size()
		}
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(stderr, "nexus gc: failed to remove %s: %v\n", path, err)
			return nil
		}
		removed++
		return nil
	})
	fmt.Fprintf(stdout, "nexus gc: removed %d blobs (%d bytes)\n", removed, freed)
	return nil
}

// copyFile is a small helper used by `nexus vendor`. Streams via
// io.Copy so a multi-MB blob doesn't all sit in memory at once.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
