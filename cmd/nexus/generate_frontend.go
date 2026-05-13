package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/client"
	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/extension/frontend"
	"github.com/paulmanoni/nexus/registry"
)

// frontendOptions is the flag-bound state for `nexus generate frontend`.
// Two manifest sources are supported, mirroring `nexus client`:
//
//   - --manifest <path>   read JSON from disk ("-" for stdin)
//   - --url <origin>      GET <origin>/__nexus/client/manifest.json
//
// One must be set. Output goes into --out (default: web/src/__nexus).
// --check exits non-zero on drift between disk and the rendered tree,
// suitable for CI gating.
type frontendOptions struct {
	Manifest  string
	URL       string
	Out       string
	Framework string
	Root      string
	Check     bool
}

func newGenerateFrontendCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := frontendOptions{
		Out:       "web/src/__nexus",
		Framework: string(frontend.Vue),
		Root:      "web",
	}
	cmd := &cobra.Command{
		Use:   "frontend",
		Short: "Generate the typed TS source tree from a nexus manifest",
		Long: `Generate the typed TS source tree consumed by a frontend app.

Reads a client-manifest JSON (either from disk or from a running app's
HTTP surface) and emits four files under --out:

  _client.ts   transport dispatcher (no runtime manifest fetch)
  types.ts     one export interface per named struct in the ref pool
  index.ts     one typed export per endpoint — listUsers(), createUser()
  vue.ts       one composable per GraphQL op (only when --framework=vue)

Files are written byte-equal: a no-op generation pass preserves the
on-disk mtime so an IDE watcher doesn't reindex on every CI run.

Examples:
    nexus generate frontend --url http://localhost:8080
    nexus generate frontend --manifest sdk/manifest.json --framework vue
    nexus generate frontend --url http://localhost:8080 --check  # CI gate`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runGenerateFrontend(opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.Manifest, "manifest", "", "path to a manifest JSON file (use '-' for stdin)")
	cmd.Flags().StringVar(&opts.URL, "url", "", "origin of a running app — GET <url>/__nexus/client/manifest.json")
	cmd.Flags().StringVar(&opts.Out, "out", opts.Out, "output directory for the generated TS source tree")
	cmd.Flags().StringVar(&opts.Framework, "framework", opts.Framework, "per-framework adapter: vue | react | svelte | none")
	cmd.Flags().StringVar(&opts.Root, "root", opts.Root, "frontend project root (informational; recorded in generated config Extras)")
	cmd.Flags().BoolVar(&opts.Check, "check", false, "exit non-zero if the on-disk tree differs from the rendered output (no writes)")
	return cmd
}

func runGenerateFrontend(opts frontendOptions, stdout, stderr io.Writer) error {
	if opts.Manifest == "" && opts.URL == "" {
		return errors.New("nexus generate frontend: one of --manifest or --url is required")
	}
	if opts.Manifest != "" && opts.URL != "" {
		return errors.New("nexus generate frontend: --manifest and --url are mutually exclusive")
	}

	// "none" is the CLI alias for the typed None const (empty
	// string). Normalising here keeps the Config switch on a single
	// canonical value while the user-facing flag reads naturally.
	fwk := opts.Framework
	if fwk == "none" {
		fwk = ""
	}
	cfg := frontend.Config{
		Root:      opts.Root,
		Framework: frontend.Framework(fwk),
		Output:    "dist",
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("nexus generate frontend: %w", err)
	}

	manifestBytes, err := loadFrontendManifest(opts)
	if err != nil {
		return fmt.Errorf("nexus generate frontend: %w", err)
	}
	var m client.Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return fmt.Errorf("nexus generate frontend: parse manifest JSON: %w", err)
	}

	reg := registryFromManifest(m)
	files, err := frontend.Render(cfg, extension.GenerateContext{
		Registry: reg,
		Refs:     m.Refs,
		BasePath: m.BasePath,
	})
	if err != nil {
		return fmt.Errorf("nexus generate frontend: render: %w", err)
	}

	abs, err := filepath.Abs(opts.Out)
	if err != nil {
		return fmt.Errorf("nexus generate frontend: resolve --out: %w", err)
	}
	generated := toGeneratedFiles(files)

	if opts.Check {
		drift, err := checkFrontendDrift(abs, generated)
		if err != nil {
			return fmt.Errorf("nexus generate frontend: check: %w", err)
		}
		if len(drift) == 0 {
			fmt.Fprintln(stdout, "ok: generated tree matches on-disk state")
			return nil
		}
		fmt.Fprintln(stderr, "drift detected — run `nexus generate frontend` to refresh:")
		for _, d := range drift {
			fmt.Fprintf(stderr, "  %s  (%s)\n", d.Path, d.Reason)
		}
		return fmt.Errorf("frontend codegen drift: %d file(s) out of date", len(drift))
	}

	changed, unchanged, err := frontend.Write(abs, generated, stdout)
	if err != nil {
		return fmt.Errorf("nexus generate frontend: write: %w", err)
	}
	fmt.Fprintf(stdout, "frontend codegen: %d written, %d unchanged → %s\n", changed, unchanged, abs)
	return nil
}

// loadFrontendManifest reads the manifest JSON from stdin, a file, or
// an HTTP origin. Mirrors loadClientManifest in client_cmd.go — kept
// separate to avoid coupling the two commands' option structs, since
// they may diverge as the codegen flow grows.
func loadFrontendManifest(opts frontendOptions) ([]byte, error) {
	if opts.Manifest != "" {
		if opts.Manifest == "-" {
			return io.ReadAll(os.Stdin)
		}
		return os.ReadFile(opts.Manifest)
	}
	url := opts.URL + "/__nexus/client/manifest.json"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, r.StatusCode)
	}
	return io.ReadAll(r.Body)
}

// registryFromManifest projects a client.Manifest's endpoint list into
// a fresh registry.Registry. The frontend renderer reads endpoints
// through the Registry interface, so reconstructing one keeps the
// renderer's API identical between in-process (driver) and offline
// (CLI) call sites — no codegen-specific seam to maintain.
//
// Only the fields the renderer reads are populated: Transport, Name,
// Method, Path, Description, ArgsSchema, ReturnSchema, Deprecated,
// DeprecationReason. The manifest doesn't carry middleware /
// auth-flow info, which the renderer doesn't currently consume.
func registryFromManifest(m client.Manifest) *registry.Registry {
	reg := registry.New()
	for _, e := range m.Endpoints {
		reg.RegisterEndpoint(registry.Endpoint{
			Service:           e.Service,
			Module:            e.Module,
			Name:              e.Name,
			Transport:         registry.Transport(e.Transport),
			Method:            e.Method,
			Path:              e.Path,
			Description:       e.Description,
			ArgsSchema:        e.Args,
			ReturnSchema:      e.Return,
			Deprecated:        e.Deprecated,
			DeprecationReason: e.DeprecationReason,
		})
	}
	return reg
}

// toGeneratedFiles converts the extension.File slice the renderer
// returns into the nexus.GeneratedFile shape the writer expects.
// Trivial copy — split out so the CLI's read flow reads top-down
// without an inline map.
func toGeneratedFiles(files []extension.File) []nexus.GeneratedFile {
	out := make([]nexus.GeneratedFile, len(files))
	for i, f := range files {
		out[i] = nexus.GeneratedFile{Path: f.Path, Body: f.Body}
	}
	return out
}

// driftEntry pairs a relative path with the human-readable reason it
// failed the --check pass. Used by the CI gate to print a hint big
// enough to fix without re-running with `-v`.
type driftEntry struct {
	Path   string
	Reason string
}

// checkFrontendDrift compares the rendered tree to the on-disk state
// without writing anything. Two failure modes count as drift: a
// generated file missing from disk, or a present file whose bytes
// differ. Extra files on disk under outDir are intentionally
// ignored — the writer never deletes files, so neither does --check.
func checkFrontendDrift(outDir string, files []nexus.GeneratedFile) ([]driftEntry, error) {
	var drift []driftEntry
	for _, f := range files {
		dst := filepath.Join(outDir, filepath.FromSlash(f.Path))
		cur, err := os.ReadFile(dst)
		if err != nil {
			if os.IsNotExist(err) {
				drift = append(drift, driftEntry{Path: f.Path, Reason: "missing"})
				continue
			}
			return nil, fmt.Errorf("read %s: %w", dst, err)
		}
		if string(cur) != string(f.Body) {
			drift = append(drift, driftEntry{Path: f.Path, Reason: "bytes differ"})
		}
	}
	return drift, nil
}
