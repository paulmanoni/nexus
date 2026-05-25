package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// loadViteEnv reads .env files from projectRoot (and
// projectRoot/islands.src, since Vite ports often keep env
// files next to the source tree) and returns the merged map.
// Search order, later overrides earlier:
//
//  1. <root>/.env
//  2. <root>/.env.local
//  3. <root>/.env.<mode>
//  4. <root>/.env.<mode>.local
//  5. <root>/<islands>/.env
//  6. <root>/<islands>/.env.local
//  7. <root>/<islands>/.env.<mode>
//  8. <root>/<islands>/.env.<mode>.local
//
// Only VITE_-prefixed entries are returned by default — Vite's
// safety convention to prevent accidentally leaking process-env
// secrets (DATABASE_URL, etc.) into the public browser bundle.
// Operators wanting to expose non-VITE_ vars must rename them
// to VITE_X or use runtime config (nexus.Get[T]).
//
// Missing files are not errors — operators may have any subset.
// Parse errors ARE surfaced (better than silently emitting a
// build with the wrong substitutions).
//
// stdout receives a single line summarizing which files were
// loaded so operators can see what's in play.
func loadViteEnv(projectRoot, mode string, stdout io.Writer) (map[string]string, error) {
	candidates := []string{
		filepath.Join(projectRoot, ".env"),
		filepath.Join(projectRoot, ".env.local"),
	}
	if mode != "" {
		candidates = append(candidates,
			filepath.Join(projectRoot, ".env."+mode),
			filepath.Join(projectRoot, ".env."+mode+".local"),
		)
	}
	if islands := islandsSrcName(); islands != "" {
		candidates = append(candidates,
			filepath.Join(projectRoot, islands, ".env"),
			filepath.Join(projectRoot, islands, ".env.local"),
		)
		if mode != "" {
			candidates = append(candidates,
				filepath.Join(projectRoot, islands, ".env."+mode),
				filepath.Join(projectRoot, islands, ".env."+mode+".local"),
			)
		}
	}
	merged := map[string]string{}
	var loaded []string
	for _, p := range candidates {
		entries, err := parseDotEnvFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		loaded = append(loaded, p)
		for k, v := range entries {
			// Only VITE_-prefixed vars get exposed to the
			// client bundle. Others are read but silently
			// dropped — operators can still set them for
			// their own purposes (e.g. consumed by a build
			// script) without leaking into the browser.
			if strings.HasPrefix(k, "VITE_") {
				merged[k] = v
			}
		}
	}
	if len(loaded) > 0 && stdout != nil {
		fmt.Fprintf(stdout, "env: loaded %d file(s), %d VITE_* var(s)\n", len(loaded), len(merged))
	}
	return merged, nil
}

// parseDotEnvFile reads a .env file in the minimal Vite-
// compatible shape: KEY=value per line, # comments, blank
// lines ignored, surrounding quotes stripped, no variable
// interpolation (`${VAR}` is taken literally — operators who
// want interpolation should use runtime config).
//
// Returns os-style ErrNotExist when the file's absent so the
// caller can skip-without-erroring cleanly.
func parseDotEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied env path
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only

	result := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Strip optional leading "export " (some teams ship
		// .env files dual-purposed as shell sourceables).
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			// Lines without "=" are malformed; tolerate
			// silently (Vite does the same).
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		// Strip matching surrounding quotes.
		if len(val) >= 2 {
			first, last := val[0], val[len(val)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		result[key] = val
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
