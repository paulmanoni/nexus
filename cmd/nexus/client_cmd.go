package main

import (
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
	if err := os.MkdirAll(opts.Out, 0750); err != nil {
		return fmt.Errorf("nexus client: mkdir %s: %w", opts.Out, err)
	}

	// Always write the static runtime files. The embedded contents
	// come from client/ui via the same //go:embed directives the
	// HTTP path uses, so there's exactly one source of truth
	// across the CLI dump and the live route.
	// client.WriteIfChanged skips the disk write when bytes match
	// — avoids touching mtime and triggering file-watch /
	// hot-reload churn on no-op runs.
	if err := client.WriteIfChanged(filepath.Join(opts.Out, "client.js"), client.RuntimeJS(), stdout); err != nil {
		return err
	}
	if err := client.WriteIfChanged(filepath.Join(opts.Out, "vue.js"), client.VueJS(), stdout); err != nil {
		return err
	}

	// jsconfig/tsconfig path mapping — wired before the manifest
	// step so a static-only dump (no --url / --manifest) still
	// gets the path mappings. The IDE benefit doesn't depend on
	// the .d.ts being present.
	if opts.JSConfig != "" {
		if err := client.MergePathsConfig(opts.JSConfig, opts.Out, stdout); err != nil {
			return err
		}
	}

	// Manifest source — file or URL. Skip when neither is set; the
	// dump-of-static-files-only mode is a valid use case.
	manifestBytes, err := loadClientManifest(opts, stderr)
	if err != nil {
		// Non-fatal: warn + continue with static-only output. A
		// developer running this against a non-running app should
		// still get a useful dump, not a failed exit.
		fmt.Fprintf(stderr, "warn: %v\n", err)
		fmt.Fprintln(stderr, "warn: skipped manifest.json + client.d.ts + vue.d.ts (no manifest source)")
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

	if err := client.WriteIfChanged(filepath.Join(opts.Out, "manifest.json"), manifestBytes, stdout); err != nil {
		return err
	}
	clientDTS := client.GenerateClientDTS(m)
	if err := client.WriteIfChanged(filepath.Join(opts.Out, "client.d.ts"), []byte(clientDTS), stdout); err != nil {
		return err
	}
	vueDTS := client.GenerateVueDTS(m)
	if err := client.WriteIfChanged(filepath.Join(opts.Out, "vue.d.ts"), []byte(vueDTS), stdout); err != nil {
		return err
	}
	// nexus.ts is the wiring scaffold — write-once so re-running
	// the CLI never clobbers the developer's edits.
	nexusTS := client.GenerateNexusTS(m)
	if err := client.WriteIfMissing(filepath.Join(opts.Out, "nexus.ts"), []byte(nexusTS), stdout); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "  manifest endpoints: %d\n", len(m.Endpoints))
	return nil
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

// (writeIfChanged + jsconfig merge logic now lives in client/dump.go
// as exported client.WriteIfChanged + client.MergePathsConfig — same
// helpers serve the in-process Config.Client.OutDir auto-dump and
// the offline `nexus client --out` flow, single source of truth.)
