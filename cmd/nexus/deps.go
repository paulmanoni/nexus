package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

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
		fmt.Fprintf(stdout, "  resolved %s @ %s — %s (+%d transitive)\n",
			res.Root.Spec, res.Root.Version, res.Root.Integrity[:23]+"…",
			len(res.Transitive))
	}

	if err := dc.saveLockfile(lf); err != nil {
		return fmt.Errorf("nexus add: save %s: %w", dc.lockfilePath, err)
	}
	fmt.Fprintf(stdout, "wrote %s\n", dc.lockfilePath)
	return nil
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
	return dc.saveLockfile(lf)
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
	lf, err := lockfile.Load(dc.lockfilePath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(stderr, "nexus install: no nexus.lock found — run `nexus add <pkg>` first")
			return nil
		}
		return err
	}

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
	return nil
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
