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

Examples:

    nexus client --out ./web/src/sdk
    nexus client --out ./web/src/sdk --url http://localhost:8080
    nexus client --out ./web/src/sdk --manifest ./manifest.json
    nexus client --out ./web/src/sdk --manifest -            # JSON on stdin
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
	return cmd
}

func runClientCmd(opts clientCmdOptions, stdout, stderr io.Writer) error {
	if err := os.MkdirAll(opts.Out, 0o755); err != nil {
		return fmt.Errorf("nexus client: mkdir %s: %w", opts.Out, err)
	}

	// Always write the static runtime files. The embedded contents
	// come from client/ui via the same //go:embed directives the
	// HTTP path uses, so there's exactly one source of truth across
	// the CLI dump and the live route.
	if err := writeFile(filepath.Join(opts.Out, "client.js"), client.RuntimeJS()); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(opts.Out, "vue.js"), client.VueJS()); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s/client.js (%d bytes)\n", opts.Out, len(client.RuntimeJS()))
	fmt.Fprintf(stdout, "wrote %s/vue.js (%d bytes)\n", opts.Out, len(client.VueJS()))

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

	if err := writeFile(filepath.Join(opts.Out, "manifest.json"), manifestBytes); err != nil {
		return err
	}
	dts := client.GenerateDTS(m)
	if err := writeFile(filepath.Join(opts.Out, "client.d.ts"), []byte(dts)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s/manifest.json (%d bytes)\n", opts.Out, len(manifestBytes))
	fmt.Fprintf(stdout, "wrote %s/client.d.ts (%d bytes, %d endpoints)\n",
		opts.Out, len(dts), len(m.Endpoints))
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

func writeFile(path string, body []byte) error {
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
