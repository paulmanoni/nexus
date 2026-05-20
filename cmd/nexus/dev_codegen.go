package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/paulmanoni/nexus/client"
	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/extension/frontend"
)

// devCodegenWatch fires the frontend codegen once per Go-child boot.
// Hooked from runDev's restart loop: after startDevChild succeeds we
// kick off this goroutine, which waits for the binary's listen port,
// detects whether frontend.Plugin is wired in, and renders the typed
// TS surface against the live manifest + contributions. Each loop
// iteration (re-spawn) gets its own invocation, so a schema change
// on the Go side that survives a rebuild propagates to the TS tree
// without the user re-running `nexus generate frontend` by hand.
//
// Silent skip cases:
//   - the binary never binds within `bootDeadline` (caller-side bug
//     or compile error; the parent loop already surfaces those)
//   - the running app didn't register frontend.Plugin (we read the
//     plugins endpoint to detect; no point codegen'ing for an app
//     that isn't wired)
//
// Hard-failure cases (logged to stderr, non-fatal to the dev loop):
//   - manifest parse error (corrupt JSON from the manifest route)
//   - filesystem write failure (perms, disk full)
//   - contributor 5xx (a broken plugin's contribution shouldn't
//     bring down the dev runner; we log it and skip just the
//     contributor merge)
func devCodegenWatch(ctx context.Context, addr, frontendDir, framework, proxyURL string, stdout, stderr io.Writer) {
	if frontendDir == "" {
		// No --frontend flag → the user isn't running a frontend
		// alongside this dev session. Codegen would emit into an
		// arbitrary path; skip.
		return
	}
	probe := normalizeProbeAddr(addr)
	if !devProbeReady(ctx, probe, 30*time.Second) {
		return
	}
	baseURL := "http://" + probe
	if err := devRunCodegen(ctx, baseURL, frontendDir, framework, proxyURL, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "%sfrontend codegen:%s %v\n", ansiYellow, ansiReset, err)
	}
}

// devProbeReady waits for the dev child to bind on addr. Mirrors
// the probe waitAndOpen does but kept private here so the codegen
// path doesn't have to share state with the banner/open-browser
// flow. Returns false on timeout or context cancellation.
func devProbeReady(ctx context.Context, addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
	}
	return false
}

// devRunCodegen is the once-per-boot work: fetch manifest +
// contributions, render to disk, log a summary. Output structure
// matches the standalone `nexus generate frontend` CLI so a manual
// re-run produces the same bytes.
//
// proxyURL is the http://host:port the vite proxy should forward to —
// when non-empty, this function also re-syncs the vite proxy block
// against the manifest's advertised prefixes so add/remove of
// modules with a RoutePrefix shows up in the SPA without a manual
// vite.config.ts edit.
func devRunCodegen(ctx context.Context, baseURL, frontendDir, framework, proxyURL string, stdout, stderr io.Writer) error {
	// Detection short-circuit: ask the plugins endpoint whether the
	// app has frontend.Plugin wired. A response missing the entry
	// means codegen isn't expected — skip silently rather than
	// printing a misleading "codegen: 0 files" line every restart.
	hasFrontend, err := devDetectFrontendPlugin(ctx, baseURL)
	if err != nil {
		// Network failure on the plugins endpoint is a real problem
		// — we just probed the bind port. Surface it so the user can
		// fix the gap; the dev loop continues either way.
		return fmt.Errorf("plugins probe: %w", err)
	}
	if !hasFrontend {
		return nil
	}

	m, err := devFetchManifest(ctx, baseURL)
	if err != nil {
		return fmt.Errorf("manifest fetch: %w", err)
	}

	// Re-sync vite proxy against the manifest's actual prefixes.
	// Runs before codegen so a missing proxy entry doesn't make
	// the first SPA request (manifest fetch from the browser) fail
	// while codegen happens. Failures are non-fatal — the codegen
	// itself is what the user cares about.
	if proxyURL != "" {
		if cfg := findViteConfig(frontendDir); cfg != "" {
			prefixes := manifestProxyPrefixes(m)
			if err := client.SyncViteProxyForPrefixes(cfg, proxyURL, prefixes, stdout); err != nil {
				fmt.Fprintf(stderr, "%svite proxy sync skipped:%s %v\n", ansiDim, ansiReset, err)
			}
		}
	}

	contribs, err := devFetchContributions(ctx, baseURL, framework)
	if err != nil {
		// Non-fatal: contributor failures shouldn't break the
		// renderer's own output. Log + continue with empty contribs.
		fmt.Fprintf(stderr, "%scontributions skipped:%s %v\n", ansiDim, ansiReset, err)
		contribs = nil
	}

	cfg := frontend.Config{
		Root:      frontendDir,
		Framework: frontend.Framework(framework),
		Output:    "dist",
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	reg := registryFromManifest(m)
	files, err := frontend.Render(cfg, extension.GenerateContext{
		Registry:     reg,
		Refs:         m.Refs,
		BasePath:     m.BasePath,
		Contributors: contribs,
	})
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}

	outDir := filepath.Join(frontendDir, "src", "__nexus")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}
	generated := toGeneratedFiles(files)
	changed, _, err := frontend.Write(outDir, generated, io.Discard)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if changed > 0 {
		// Only announce when something actually changed — a no-op
		// re-render on every restart would spam the log.
		fmt.Fprintf(stdout, "%s●%s frontend codegen · %d file(s) updated in %s\n",
			ansiCyan, ansiReset, changed, outDir)
	}
	return nil
}

// watchAndResyncViteProxy watches the user's vite.config.ts and
// re-runs SyncViteProxyForPrefixes whenever it changes. Without
// this the proxy sync only fires on a Go restart, so editing the
// vite config directly (e.g., deleting the managed block to test
// what nexus puts back) doesn't trigger re-injection — the user
// would have to bounce nexus dev to recover.
//
// Cost is low: the sync function is a no-op when bytes already
// match, and the manifest fetch is local HTTP. When nexus itself
// writes the file, fsnotify fires once more, the re-sync is
// byte-identical, no infinite loop.
//
// Editor save styles: we watch both the file AND its parent
// directory, since some editors atomic-rename a temp into place
// (which fsnotify reports against the dir, not the original file
// handle). Debounce coalesces the rename + write pair into a
// single sync call.
func watchAndResyncViteProxy(ctx context.Context, addr, viteConfigPath, proxyURL string, stdout, stderr io.Writer) {
	if viteConfigPath == "" || proxyURL == "" {
		return
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(stderr, "%svite proxy watcher disabled:%s %v\n", ansiDim, ansiReset, err)
		return
	}
	defer w.Close()
	if err := w.Add(viteConfigPath); err != nil {
		fmt.Fprintf(stderr, "%svite proxy watcher disabled:%s %v\n", ansiDim, ansiReset, err)
		return
	}
	// Atomic-rename saves (vim, JetBrains) only emit events on the
	// containing dir against a new inode; watching the dir too is
	// the only way to catch those.
	_ = w.Add(filepath.Dir(viteConfigPath))

	probe := normalizeProbeAddr(addr)
	baseURL := "http://" + probe
	base := filepath.Base(viteConfigPath)

	var debounce *time.Timer
	fire := func() {
		// Re-fetch manifest each time so a module added/removed
		// between syncs reflects in the prefix set. The endpoint is
		// local, sub-millisecond; cheap to call.
		ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		m, err := devFetchManifest(ctx2, baseURL)
		if err != nil {
			return // app probably restarting; the next Go boot will re-sync
		}
		cfg := findViteConfig(filepath.Dir(viteConfigPath))
		if cfg == "" {
			cfg = viteConfigPath
		}
		_ = client.SyncViteProxyForPrefixes(cfg, proxyURL, manifestProxyPrefixes(m), stdout)
	}

	for {
		select {
		case <-ctx.Done():
			if debounce != nil {
				debounce.Stop()
			}
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			// Filter to the config file (or a sibling named the same
			// — atomic-rename writes hit the dir with the same base).
			if filepath.Base(ev.Name) != base {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			if debounce != nil {
				debounce.Stop()
			}
			debounce = time.AfterFunc(250*time.Millisecond, fire)
		case _, ok := <-w.Errors:
			if !ok {
				return
			}
		}
	}
}

// manifestProxyPrefixes derives the set of URL prefixes the vite
// dev server should proxy to the Go app. Combines the framework's
// fixed prefixes (/__nexus, /graphql, /oauth, /ws) with one entry
// per distinct top-level segment found across every endpoint in
// the manifest. Each endpoint's effective URL is BasePath+Path;
// we take its first segment so a module declared with
// nexus.RoutePrefix("/oats-uaa") gets "/oats-uaa" proxied without
// the SPA having to know about every sub-route.
//
// All transports count — REST, WebSocket, and GraphQL. A module
// with RoutePrefix("/oats-uaa") mounts its GraphQL endpoint at
// /oats-uaa/graphql; excluding graphql here would drop the only
// signal we have for that prefix.
//
// Duplicates and overlaps with the framework defaults collapse
// naturally — SyncViteProxyForPrefixes deduplicates.
func manifestProxyPrefixes(m client.Manifest) []string {
	out := append([]string{}, client.DefaultNexusProxyPrefixes...)
	seen := map[string]bool{}
	for _, p := range out {
		seen[p] = true
	}
	for _, ep := range m.Endpoints {
		seg := firstPathSegment(m.BasePath + ep.Path)
		if seg == "" || seen[seg] {
			continue
		}
		seen[seg] = true
		out = append(out, seg)
	}
	return out
}

// firstPathSegment returns "/foo" for any path starting with "/foo/…"
// or "/foo". Empty / root-only paths yield "" so the caller skips.
// Used to derive a proxy prefix from an endpoint URL — the vite
// proxy matches by prefix, so the first segment is the minimal
// rule that catches every route under the same root.
func firstPathSegment(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	rest := p[1:]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		return ""
	}
	return "/" + rest
}

// devDetectFrontendPlugin reads /__nexus/plugins and returns true
// when an entry named "frontend" is present. We can't import the
// extension package's name constant directly without coupling the
// CLI to its identity — the literal is the contract.
func devDetectFrontendPlugin(ctx context.Context, baseURL string) (bool, error) {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, baseURL+"/__nexus/plugins", nil)
	if err != nil {
		return false, err
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return false, fmt.Errorf("HTTP %d", r.StatusCode)
	}
	var payload struct {
		Plugins []struct {
			Name string `json:"name"`
		} `json:"plugins"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		return false, err
	}
	for _, p := range payload.Plugins {
		if p.Name == "frontend" {
			return true, nil
		}
	}
	return false, nil
}

// devFetchManifest reads /__nexus/client/manifest.json. Separate
// from the CLI's loadFrontendManifest because the dev path always
// has a URL (no file source) and uses a tighter timeout.
func devFetchManifest(ctx context.Context, baseURL string) (client.Manifest, error) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, baseURL+"/__nexus/client/manifest.json", nil)
	if err != nil {
		return client.Manifest{}, err
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return client.Manifest{}, err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return client.Manifest{}, fmt.Errorf("HTTP %d", r.StatusCode)
	}
	var m client.Manifest
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		return client.Manifest{}, err
	}
	return m, nil
}

// devFetchContributions fetches the contributor output for the
// requested framework. 404 is treated as "no contributions wired"
// (older nexus or no frontend.Plugin), translated to (nil, nil).
// Other failures bubble up.
func devFetchContributions(ctx context.Context, baseURL, framework string) ([]extension.ClientContributor, error) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	u := baseURL + "/__nexus/client/contributions.json"
	if framework != "" {
		u += "?framework=" + framework
	}
	req, err := http.NewRequestWithContext(c, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if r.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if r.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", r.StatusCode)
	}
	var resp client.ContributionsResponse
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		return nil, err
	}
	out := make([]extension.ClientContributor, 0, len(resp.Plugins))
	for _, p := range resp.Plugins {
		files := make([]extension.File, 0, len(p.Files))
		for _, f := range p.Files {
			files = append(files, extension.File{Path: f.Path, Body: []byte(f.Body)})
		}
		if len(files) > 0 {
			out = append(out, extension.StaticContributor(files))
		}
	}
	return out, nil
}
