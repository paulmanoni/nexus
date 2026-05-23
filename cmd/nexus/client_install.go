package main

// nexus-client install path — when an operator runs
// `nexus add nexus-client`, `nexus add nexus-client/vue`, or
// `nexus add nexus-client/react`, the spec is recognized here and
// resolved from a running nexus app's `/__nexus/client/*`
// endpoints instead of the configured registry (esm.sh).
//
// Why this lives in its own file: the standard add path (deps.go's
// runAdd) builds the lockfile + materializes node_modules from
// esm.sh URLs. The SDK is a different beast — bytes + version come
// from the app the operator is targeting, not a public registry —
// so the fetch + materialize logic forks. Everything downstream
// (lockfile entry shape, package.json update, integrity check)
// stays the same so `nexus install` on a fresh clone works
// identically.
//
// Spec → file plan:
//
//	nexus-client          → core: client.js + client.d.ts + manifest.json
//	nexus-client/vue      → core + vue.js + vue.d.ts
//	nexus-client/react    → core + react.js + react.d.ts
//
// Origin resolution priority (high → low):
//	1. --from <URL> flag on `nexus add` (when wired)
//	2. NEXUS_DEV_URL env var
//	3. http://localhost:8080 (the default `nexus dev` port)

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

const (
	// nexusClientName is the bare spec name. Specs may be the name
	// alone (`nexus-client`) or with an adapter subpath
	// (`nexus-client/vue`, `nexus-client/react`).
	nexusClientName = "nexus-client"

	// nexusClientPath is where the server-side client.Mount serves
	// the bundle at. Keep in lockstep with client.DefaultPath.
	nexusClientPath = "/__nexus/client"

	// nexusClientOriginEnv overrides the default origin. Empty →
	// http://localhost:8080. Honored by every nexus-client flow,
	// not just add, so `nexus install` against a previously-pinned
	// URL fetches from the same app the developer used at add time.
	nexusClientOriginEnv = "NEXUS_DEV_URL"

	// defaultNexusClientOrigin is the assumed app URL when nothing
	// else is configured. Mirrors the scaffold's default Gin port.
	defaultNexusClientOrigin = "http://localhost:8080"
)

// isNexusClientSpec reports whether spec targets the framework's
// SDK package (with or without an adapter subpath, with or without
// a version suffix). Recognized shapes:
//
//	nexus-client
//	nexus-client@1.2.3
//	nexus-client/vue
//	nexus-client/vue@1.2.3
//	nexus-client/react
//
// Anything else (including superficially-similar names like
// `nexus-client-other` or `@scope/nexus-client`) returns false so
// the regular esm.sh path keeps owning those.
func isNexusClientSpec(spec string) bool {
	name := spec
	if at := strings.IndexByte(spec, '@'); at > 0 {
		name = spec[:at]
	}
	if name == nexusClientName {
		return true
	}
	rest, ok := strings.CutPrefix(name, nexusClientName+"/")
	if !ok {
		return false
	}
	switch rest {
	case "vue", "react":
		return true
	}
	return false
}

// nexusClientFiles is the file list per adapter. The core is
// always pulled; an adapter subpath adds its pair on top.
// Order matters: client.js → client.d.ts → adapter.js → adapter.d.ts
// makes the boot-log output read top-down by importance.
type nexusClientFiles struct {
	core    []string // always: client.js, client.d.ts
	adapter []string // optional: vue.js + vue.d.ts (or react)
}

func filesForSpec(spec string) nexusClientFiles {
	// Strip @version + spec name; what remains is "", "vue", or "react".
	name := spec
	if at := strings.IndexByte(spec, '@'); at > 0 {
		name = spec[:at]
	}
	sub := strings.TrimPrefix(name, nexusClientName)
	sub = strings.TrimPrefix(sub, "/")

	core := []string{"client.js", "client.d.ts"}
	switch sub {
	case "vue":
		return nexusClientFiles{core: core, adapter: []string{"vue.js", "vue.d.ts"}}
	case "react":
		return nexusClientFiles{core: core, adapter: []string{"react.js", "react.d.ts"}}
	default:
		return nexusClientFiles{core: core}
	}
}

// resolveNexusClientOrigin picks the URL to fetch from. Explicit
// arg wins (from --from <URL>); NEXUS_DEV_URL env is the fallback;
// http://localhost:8080 the last resort.
//
// Trailing slashes are stripped so URL composition stays clean.
// Schemeless inputs ("localhost:8080") get http:// prepended,
// matching what config.Client does for the same convenience.
func resolveNexusClientOrigin(explicit string) string {
	pick := explicit
	if pick == "" {
		pick = os.Getenv(nexusClientOriginEnv)
	}
	if pick == "" {
		pick = defaultNexusClientOrigin
	}
	pick = strings.TrimRight(pick, "/")
	if !strings.Contains(pick, "://") {
		pick = "http://" + pick
	}
	return pick
}

// appManifest is the trimmed-down decoding of /manifest.json that
// the install flow uses to learn the SDK's reported version. Many
// other fields exist in the JSON (endpoints, refs, auth …); we
// don't care about them at install time.
type appManifest struct {
	Version string `json:"version"`
}

// addNexusClient is the fork that runs in place of the regular
// esm.sh fetch when the user types `nexus add nexus-client[/...]`.
// Returns the list of lockfile.Package entries (one per file
// downloaded) and the resolved version string.
//
// Behaviors:
//   - One HTTP request per file. The bytes are cached in the
//     deps Store by URL → content hash, so subsequent `nexus
//     install` calls on the same lockfile no-op when bytes are
//     unchanged (matches the esm.sh path).
//   - Files are also materialized under ./node_modules/nexus-client/
//     so bundlers (vite, esbuild, webpack, …) and TS servers can
//     resolve `import { useQuery } from 'nexus-client'` without
//     reaching into ~/.nexus/cache. A stub package.json under
//     ./node_modules/nexus-client/ wires the exports map (`.`,
//     `./vue`, `./react`).
//   - Per-file integrity is sha256 of the bytes. Mismatch on a
//     re-fetch (e.g. someone tampered with the response) trips an
//     IntegrityError just like the esm.sh path.
//
// Reachability + manifest are checked once up-front; a typo in
// the --from URL surfaces before any blobs land in the cache.
func addNexusClient(ctx context.Context, dc *depsContext, spec, originOverride string) ([]lockfile.Package, string, error) {
	origin := resolveNexusClientOrigin(originOverride)

	// Probe the app's /manifest.json for the version. Doubles as a
	// reachability check — a typo'd URL or stopped server fails
	// here before the per-file fetches start writing cache entries.
	mf, err := fetchAppManifest(ctx, origin)
	if err != nil {
		return nil, "", fmt.Errorf("origin %s unreachable: %w", origin, err)
	}
	version := mf.Version
	if version == "" {
		version = "0.0.0" // no version reported — pin as 0.0.0 so the lockfile is still valid
	}

	plan := filesForSpec(spec)

	// Project root for node_modules materialization. Best-effort —
	// a getwd error doesn't kill the cache write, only the
	// materialization step.
	cwd, _ := os.Getwd()
	nodeModulesDir := filepath.Join(cwd, "node_modules", nexusClientName)

	files := append([]string{}, plan.core...)
	files = append(files, plan.adapter...)

	pkgs := make([]lockfile.Package, 0, len(files))
	for _, f := range files {
		fileURL := origin + nexusClientPath + "/" + f
		body, contentType, err := fetchFile(ctx, fileURL)
		if err != nil {
			return nil, "", fmt.Errorf("fetch %s: %w", fileURL, err)
		}
		sum := sha256.Sum256(body)
		hash := hex.EncodeToString(sum[:])

		if _, err := dc.store.Put(fileURL, bytes.NewReader(body), hash, store.Metadata{
			URL:           fileURL,
			ContentSHA256: hash,
			ContentType:   contentType,
			ResolvedURL:   fileURL,
		}); err != nil {
			return nil, "", fmt.Errorf("cache %s: %w", fileURL, err)
		}

		// Materialize under node_modules so bundlers + IDEs resolve
		// 'nexus-client' without spelunking the deps cache. Each
		// file goes to ./node_modules/nexus-client/<file>; the stub
		// package.json (written below) gives them a canonical entry
		// point + exports map.
		if cwd != "" {
			if err := writeNodeModulesFile(nodeModulesDir, f, body); err != nil {
				return nil, "", fmt.Errorf("write %s: %w", filepath.Join(nodeModulesDir, f), err)
			}
		}

		pkgs = append(pkgs, lockfile.Package{
			Spec:        nexusClientName + "/" + f,
			Version:     version,
			Resolved:    fileURL,
			Integrity:   "sha256-" + hash,
			ContentType: contentType,
		})
	}

	// Stub package.json under ./node_modules/nexus-client/. Tells
	// bundlers + tsserver where the entry points live so the user's
	// `import { useQuery } from 'nexus-client'` (or
	// `'nexus-client/vue'`) resolves without an extra config.
	if cwd != "" {
		if err := writeNexusClientPkgStub(nodeModulesDir, version); err != nil {
			return nil, "", fmt.Errorf("write %s/package.json: %w", nodeModulesDir, err)
		}
	}

	return pkgs, version, nil
}

// fetchAppManifest hits /manifest.json on the target origin. Used
// to learn the SDK's version BEFORE any blobs are committed to the
// cache so a 404 / unreachable / wrong-content-type origin fails
// loud before the lockfile mutates.
func fetchAppManifest(ctx context.Context, origin string) (appManifest, error) {
	url := origin + nexusClientPath + "/manifest.json"
	body, _, err := fetchFile(ctx, url)
	if err != nil {
		return appManifest{}, err
	}
	var mf appManifest
	if err := json.Unmarshal(body, &mf); err != nil {
		return appManifest{}, fmt.Errorf("decode %s: %w (is this a nexus app?)", url, err)
	}
	return mf, nil
}

// fetchFile is the HTTP GET used for every nexus-client artifact.
// One short timeout because the target is always a same-machine /
// same-network nexus app — slow responses there are almost
// certainly a misconfigured URL, not transient flakiness.
//
// Non-2xx responses surface as errors carrying status + URL so the
// operator can locate the typo without re-running with --verbose.
func fetchFile(ctx context.Context, url string) ([]byte, string, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/javascript, application/typescript, application/json, */*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// writeNodeModulesFile drops one fetched artifact into
// ./node_modules/nexus-client/<file>. Creates the directory tree
// idempotently. The file is overwritten on every add — that's
// fine because the cache is the source of truth and re-extracts
// are cheap.
func writeNodeModulesFile(dir, filename string, body []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, filename), body, 0o644)
}

// writeNexusClientPkgStub writes the synthetic package.json under
// ./node_modules/nexus-client/ that maps the package's entry +
// adapter subpaths. The `exports` block is the modern Node /
// bundler-resolved spec; `main`/`types` cover older tooling that
// hasn't adopted exports yet.
//
// Idempotent — re-add overwrites with the latest version. The
// content is generated, not authored, so no merge logic is
// needed. Re-running won't churn unrelated file timestamps because
// the JSON shape is fully deterministic.
func writeNexusClientPkgStub(dir, version string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	pkg := map[string]any{
		"name":    nexusClientName,
		"version": version,
		"type":    "module",
		// `main` + `types` for legacy tooling; `exports` for modern.
		// Both point at the same files so the choice of resolver
		// doesn't change which bytes get loaded.
		"main":    "./client.js",
		"types":   "./client.d.ts",
		"exports": map[string]any{
			".": map[string]string{
				"types":  "./client.d.ts",
				"import": "./client.js",
				"default": "./client.js",
			},
			"./vue": map[string]string{
				"types":  "./vue.d.ts",
				"import": "./vue.js",
				"default": "./vue.js",
			},
			"./react": map[string]string{
				"types":  "./react.d.ts",
				"import": "./react.js",
				"default": "./react.js",
			},
		},
		"sideEffects": false,
	}
	buf, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "package.json"), append(buf, '\n'), 0o644)
}
