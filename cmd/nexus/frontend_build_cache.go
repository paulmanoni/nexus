package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Frontend-build cache. Avoids re-running the (slow) Vite/esbuild
// bundle when none of its inputs changed since the last successful
// build. Cuts the inner `nexus build` loop from ~12s to ~4s on
// Go-only edits, which is the common case during framework work.
//
// Design:
//
//  1. Inputs are hashed deterministically into a single SHA-256.
//  2. The hex digest is written to <projectRoot>/.nexus-cache/frontend-build.hash
//     after every successful build.
//  3. Before the next build, we recompute the digest. If it matches
//     AND the output directory is non-empty, the bundler step is
//     skipped.
//  4. The cache directory is project-local (one per repo) and
//     should be added to .gitignore. It's NOT placed inside the
//     output directory because `//go:embed all:islands` would bake
//     the hash file into the binary.
//
// Inputs that affect the bundle and therefore go into the hash:
//
//   - Every regular file under islands.src/ (content + relative path)
//   - nexus.lock (frontend dep set)
//   - package.json at project root (scripts / module type)
//   - tsconfig.json or jsconfig.json (whichever findProjectTSConfig picks)
//   - .env / .env.production / .env.local (define replacements)
//   - vite.config.* at project root or in islands.src
//   - cacheVersion (bump when bundler logic changes incompatibly)
//
// Bumping cacheVersion is the kill-switch when we change something
// that's not captured by any of the file inputs (a new built-in
// plugin's default behavior, a default option flip, etc.).
const cacheVersion = "v2"

// frontendCacheDirName is the project-local cache dir. Kept short
// + dot-prefixed so it stays out of the way + gets ignored by most
// scaffolders. Mirrors the `.nexus/` pattern but is build-only —
// safe to delete at any time to force a rebuild.
const frontendCacheDirName = ".nexus-cache"

// frontendCacheFileName is the file inside frontendCacheDirName that
// holds the most recent successful build's hash. Single-line file,
// 64 hex chars, no surrounding whitespace.
const frontendCacheFileName = "frontend-build.hash"

// envSkipFrontendCache disables the cache entirely when set to a
// truthy value. Useful for CI ("rebuild from scratch every time")
// and for debugging cache-related issues.
const envSkipFrontendCache = "NEXUS_FRONTEND_NO_CACHE"

// frontendBuildHash computes the deterministic digest of every
// input that, if it changes, should force a fresh frontend bundle.
//
// srcDir is the actual entry directory (after findFrontendEntries's
// auto-descent into src/ / app/ / client/), but we hash everything
// under <projectRoot>/<islandsSrcName()> recursively so a change
// outside the auto-resolved subdir (e.g. a sibling `public/`) still
// invalidates the cache.
func frontendBuildHash(projectRoot string) (string, error) {
	h := sha256.New()

	// Prefix the digest with the cache version. Cheapest possible
	// "global busting" lever — bump cacheVersion and every project
	// rebuilds without users having to clear caches manually.
	fmt.Fprintf(h, "nexus-frontend-cache/%s\n", cacheVersion)

	// 1. Hash every file under islands.src/ (recursive).
	srcRoot := filepath.Join(projectRoot, islandsSrcName())
	if err := hashTree(h, srcRoot, "islands.src"); err != nil {
		return "", err
	}

	// 2. Project-root files that affect the bundle. Listed in a
	// stable order so the digest is deterministic.
	rootFiles := []string{
		"nexus.lock",
		"package.json",
		"package-lock.json",
		// Vite-port projects ship a vite.config; treating it as
		// an input means tweaking the config invalidates the
		// cache even though we shell to esbuild internally.
		"vite.config.js",
		"vite.config.ts",
		"vite.config.mjs",
		// .env files contribute define-replacements; loadViteEnv
		// reads these.
		".env",
		".env.local",
		".env.production",
		".env.production.local",
	}
	for _, name := range rootFiles {
		if err := hashFile(h, filepath.Join(projectRoot, name), name); err != nil {
			return "", err
		}
	}

	// 3. tsconfig — whichever findProjectTSConfig would pick.
	// Hashed by its absolute path AND content so a swap from
	// jsconfig→tsconfig (or src/tsconfig→root/tsconfig) busts
	// the cache.
	if cfg := findProjectTSConfig(projectRoot); cfg != "" {
		rel, _ := filepath.Rel(projectRoot, cfg)
		if err := hashFile(h, cfg, "tsconfig:"+filepath.ToSlash(rel)); err != nil {
			return "", err
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashTree walks root recursively and folds each regular file's
// (relative path, content) pair into h. Walks in sorted order so
// the digest is deterministic across filesystems. Missing root is
// not an error — the caller handles "no islands.src" separately.
//
// label is mixed into the digest so two trees with identical
// contents but different roles (unlikely, but cheap insurance)
// produce different hashes.
func hashTree(h io.Writer, root, label string) error {
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(h, "%s:absent\n", label)
			return nil
		}
		return fmt.Errorf("frontend cache: stat %s: %w", root, err)
	}
	if !info.IsDir() {
		return hashFile(h, root, label)
	}

	type entry struct {
		rel  string
		full string
	}
	var files []entry
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Skip OS / editor garbage so saving from VSCode or
		// finder doesn't trash the cache.
		name := d.Name()
		if name == ".DS_Store" || strings.HasSuffix(name, "~") || strings.HasSuffix(name, ".swp") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, entry{rel: filepath.ToSlash(rel), full: path})
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("frontend cache: walk %s: %w", root, walkErr)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	fmt.Fprintf(h, "%s:dir:%d\n", label, len(files))
	for _, f := range files {
		if err := hashFile(h, f.full, label+"/"+f.rel); err != nil {
			return err
		}
	}
	return nil
}

// hashFile folds (label, content) into h. Missing files mix in a
// stable "absent" marker so adding the file later (or removing it)
// changes the digest. The label is what makes the digest position-
// aware: two files with identical content at different paths still
// produce different hashes overall.
func hashFile(h io.Writer, path, label string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(h, "%s:absent\n", label)
			return nil
		}
		return fmt.Errorf("frontend cache: read %s: %w", path, err)
	}
	fmt.Fprintf(h, "%s:%d:", label, len(data))
	if _, err := h.Write(data); err != nil {
		return fmt.Errorf("frontend cache: hash %s: %w", path, err)
	}
	fmt.Fprintln(h)
	return nil
}

// frontendCachePath returns the absolute path to the hash file. Does
// not create the parent directory — that's writeFrontendBuildHash's
// job.
func frontendCachePath(projectRoot string) string {
	return filepath.Join(projectRoot, frontendCacheDirName, frontendCacheFileName)
}

// readFrontendBuildHash returns the previously-stored hash, or ""
// when the cache doesn't exist yet (first build) or is unreadable
// (treat as cache-miss; rebuild is the safe response).
func readFrontendBuildHash(projectRoot string) string {
	data, err := os.ReadFile(frontendCachePath(projectRoot))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeFrontendBuildHash persists hash after a successful build.
// Failures are surfaced as warnings to the caller, not hard errors
// — a missing cache file just means the next build doesn't get the
// fast path; everything else still works.
func writeFrontendBuildHash(projectRoot, hash string) error {
	dir := filepath.Join(projectRoot, frontendCacheDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("frontend cache: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, frontendCacheFileName)
	if err := os.WriteFile(path, []byte(hash+"\n"), 0o644); err != nil {
		return fmt.Errorf("frontend cache: write %s: %w", path, err)
	}
	return nil
}

// outputDirHasFiles checks that the build's output directory
// actually has bundled artifacts. Without this guard a stale hash
// + empty islands/ would incorrectly skip the build, producing a
// binary that embeds nothing. We accept any regular file under
// outDir as evidence of a prior successful build.
func outputDirHasFiles(outDir string) bool {
	info, err := os.Stat(outDir)
	if err != nil || !info.IsDir() {
		return false
	}
	var found bool
	_ = filepath.WalkDir(outDir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// frontendCacheDisabled honors the env-var kill switch. Anything
// other than the empty string / literal "0" / literal "false"
// counts as enabled. CI systems setting it to "1" / "true" / "yes"
// all force a rebuild.
func frontendCacheDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envSkipFrontendCache))) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}
