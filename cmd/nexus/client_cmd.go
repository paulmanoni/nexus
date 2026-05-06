package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/paulmanoni/nexus/client"
)

// clientCmdOptions are the flags for `nexus client`. Three input
// shapes for sourcing the manifest, parallel to `nexus reconcile`:
//
//	nexus client --out ./web/sdk                                   # static files only
//	nexus client --out ./web/sdk --url http://localhost:8080       # fetch live
//	nexus client --out ./web/sdk --manifest ./manifest.json        # read from file (- for stdin)
//
// Resolution order when multiple sources are passed: --manifest >
// --url > none. The CLI is intentionally non-blocking on a missing
// running app — when --url is given but the app isn't reachable,
// the CLI warns and writes the static JS without manifest/.d.ts,
// so a developer who forgot to start their app still gets a useful
// dump rather than an error.
type clientCmdOptions struct {
	Out      string
	URL      string
	Manifest string
	JSConfig string
}

func newClientCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := clientCmdOptions{}
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Write the embedded JS/TS client SDK to disk",
		Long: `Dump the nexus client SDK files to a directory for users who prefer
checking the SDK into their frontend repo (or vendoring it into a
non-nexus build pipeline) instead of fetching from a running app
at /__nexus/client/*.

Without --url or --manifest, only the runtime files land:

    <out>/client.js     # ESM runtime — REST/GraphQL/WS/CRUD/auth
    <out>/vue.js        # Vue 3 composables

Pass --url <origin> to also fetch the live manifest from a running
app and generate the matching .d.ts:

    <out>/manifest.json # SDK-tailored manifest snapshot
    <out>/client.d.ts   # generated TypeScript types

Pass --jsconfig <path> (or its alias --tsconfig <path>) to also
write a jsconfig.json / tsconfig.json that maps the runtime URL
imports ('/__nexus/client/client.js' and friends) to the dumped
relative paths — IDE go-to-definition + completion "just work"
against the URL-style imports the SDK README suggests. The flag
merges with an existing config rather than overwriting; user
fields (compilerOptions.target, include, exclude, custom paths
entries, …) are preserved.

The two flags are aliases — both write the same content; the
filename is the only difference. Use --jsconfig for plain JS
projects, --tsconfig for TS projects. Setting both is redundant;
the second-parsed wins.

Examples:

    nexus client --out ./web/src/sdk
    nexus client --out ./web/src/sdk --url http://localhost:8080
    nexus client --out ./web/src/sdk --manifest ./manifest.json
    nexus client --out ./web/src/sdk --manifest -            # JSON on stdin
    nexus client --out ./web/src/sdk --jsconfig ./web/jsconfig.json
    nexus client --out ./web/src/sdk --tsconfig ./web/tsconfig.json
`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if opts.Out == "" {
				return fmt.Errorf("nexus client: --out is required")
			}
			return runClientCmd(opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.Out, "out", "", "directory to write the SDK files into (created if missing)")
	cmd.Flags().StringVar(&opts.URL, "url", "", "origin of a running nexus app; the CLI fetches /__nexus/client/manifest.json from here")
	cmd.Flags().StringVar(&opts.Manifest, "manifest", "", "path to a manifest JSON file (- for stdin)")
	cmd.Flags().StringVar(&opts.JSConfig, "jsconfig", "", "path to write (or merge into) a jsconfig.json with URL→file path mappings; enables IDE go-to-definition on '/__nexus/client/*' imports")
	// --tsconfig is an alias bound to the same backing variable —
	// the file shape is identical (compilerOptions.paths). TS
	// projects pass --tsconfig, JS projects pass --jsconfig; both
	// accepted so users don't have to mentally translate.
	cmd.Flags().StringVar(&opts.JSConfig, "tsconfig", "", "alias for --jsconfig; pass when targeting a tsconfig.json (same content, different filename)")
	return cmd
}

func runClientCmd(opts clientCmdOptions, stdout, stderr io.Writer) error {
	if err := os.MkdirAll(opts.Out, 0o755); err != nil {
		return fmt.Errorf("nexus client: mkdir %s: %w", opts.Out, err)
	}

	// Always write the static runtime files. The embedded contents
	// come from client/ui via the same //go:embed directives the
	// HTTP path uses, so there's exactly one source of truth across
	// the CLI dump and the live route. writeIfChanged skips the
	// disk write when bytes match — avoids touching mtime and
	// triggering file-watch / hot-reload churn on no-op runs.
	if err := writeIfChanged(filepath.Join(opts.Out, "client.js"), client.RuntimeJS(), stdout); err != nil {
		return err
	}
	if err := writeIfChanged(filepath.Join(opts.Out, "vue.js"), client.VueJS(), stdout); err != nil {
		return err
	}

	// jsconfig is the IDE-config helper — wired before the manifest
	// step so a static-only dump (no --url / --manifest) still gets
	// the path mappings. The IDE benefit doesn't depend on the
	// .d.ts being present.
	if err := writeJSConfig(opts, stdout); err != nil {
		return err
	}

	// Manifest source — file or URL. Skip when neither is set; the
	// dump-of-static-files-only mode is a valid use case.
	manifestBytes, err := loadClientManifest(opts, stderr)
	if err != nil {
		// Non-fatal: warn + continue with static-only output. A
		// developer running this against a non-running app should
		// still get a useful dump, not a failed exit.
		fmt.Fprintf(stderr, "warn: %v\n", err)
		fmt.Fprintln(stderr, "warn: skipped manifest.json + client.d.ts (no manifest source)")
		return nil
	}
	if manifestBytes == nil {
		// Neither --url nor --manifest set; not an error.
		return nil
	}

	var m client.Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return fmt.Errorf("nexus client: parse manifest JSON: %w", err)
	}

	if err := writeIfChanged(filepath.Join(opts.Out, "manifest.json"), manifestBytes, stdout); err != nil {
		return err
	}
	dts := client.GenerateDTS(m)
	if err := writeIfChanged(filepath.Join(opts.Out, "client.d.ts"), []byte(dts), stdout); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "  manifest endpoints: %d\n", len(m.Endpoints))
	return nil
}

// jsconfigPaths is the URL → file mapping the SDK serves. Wrapped
// in a function so tests + the writer share one source of truth.
func jsconfigPaths(out, jsconfig string) (map[string][]string, error) {
	// Mappings live inside the jsconfig file's directory; compute
	// the relative path from there to the SDK dump dir.
	jsconfigDir := filepath.Dir(jsconfig)
	rel, err := filepath.Rel(jsconfigDir, out)
	if err != nil {
		return nil, err
	}
	// jsconfig path values use forward slashes regardless of OS.
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." {
		rel = "."
	}
	return map[string][]string{
		"/__nexus/client/client.js": {rel + "/client.js"},
		"/__nexus/client/vue.js":    {rel + "/vue.js"},
	}, nil
}

// writeJSConfig emits or merges a jsconfig.json at the path so an
// IDE (JetBrains, VS Code) can resolve URL-style imports like
// '/__nexus/client/client.js' to the locally-dumped files. Without
// it, "Cannot find declaration to go to" warns on every import.
//
// Merge semantics: existing jsconfig.json's compilerOptions.paths
// is preserved entry-for-entry; the SDK URL keys are added or
// overwritten. include/exclude/baseUrl/other top-level fields are
// preserved as-is. baseUrl defaults to "." when missing — required
// for paths to resolve.
func writeJSConfig(opts clientCmdOptions, stdout io.Writer) error {
	if opts.JSConfig == "" {
		return nil
	}
	mappings, err := jsconfigPaths(opts.Out, opts.JSConfig)
	if err != nil {
		return fmt.Errorf("nexus client: compute jsconfig paths: %w", err)
	}

	// Read existing config when present. A bare-bones top-level map
	// keeps preserved fields untouched even when the user has knobs
	// the CLI doesn't know about.
	var doc map[string]any
	if existing, err := os.ReadFile(opts.JSConfig); err == nil {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return fmt.Errorf("nexus client: parse existing %s: %w", opts.JSConfig, err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}

	co, _ := doc["compilerOptions"].(map[string]any)
	if co == nil {
		co = map[string]any{}
		doc["compilerOptions"] = co
	}
	if _, ok := co["baseUrl"]; !ok {
		co["baseUrl"] = "."
	}
	paths, _ := co["paths"].(map[string]any)
	if paths == nil {
		paths = map[string]any{}
		co["paths"] = paths
	}
	for k, v := range mappings {
		paths[k] = v
	}

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.JSConfig), 0o755); err != nil {
		return fmt.Errorf("nexus client: mkdir %s: %w", filepath.Dir(opts.JSConfig), err)
	}
	return writeIfChanged(opts.JSConfig, body, stdout)
}

// loadClientManifest reads the manifest JSON from either a file
// (--manifest) or a running app (--url). Returns (nil, nil) when
// neither is set so the caller can skip the manifest-dependent
// outputs without erroring.
func loadClientManifest(opts clientCmdOptions, stderr io.Writer) ([]byte, error) {
	if opts.Manifest != "" {
		if opts.Manifest == "-" {
			return io.ReadAll(os.Stdin)
		}
		return os.ReadFile(opts.Manifest)
	}
	if opts.URL != "" {
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
	return nil, nil
}

// writeIfChanged writes body to path only when the file is missing
// or its current contents differ from body. Logs "wrote" with the
// byte count on a real write, "unchanged" when the disk copy
// already matched. Skipping the no-op write preserves mtime —
// file watchers (vite, webpack-dev-server, JetBrains' indexer)
// don't re-trigger builds when re-running `nexus client --out`
// against an already-up-to-date target. Side benefit: a CI step
// that runs the CLI on every build sees clean diffs only when the
// SDK actually changed.
//
// Bytes-equal comparison rather than hash because the files are
// small (tens of KB each) and the explicit byte-slice equality is
// allocation-free for the common no-change case.
func writeIfChanged(path string, body []byte, stdout io.Writer) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, body) {
		fmt.Fprintf(stdout, "unchanged %s (%d bytes)\n", path, len(body))
		return nil
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(stdout, "wrote %s (%d bytes)\n", path, len(body))
	return nil
}
