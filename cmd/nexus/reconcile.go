package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// reconcileOptions carries the flags for `nexus reconcile`. Three
// input shapes, picked by what the caller has on hand:
//
//   - --manifest-json <path|->: read the print-mode JSON from a file
//     (or stdin). Cheapest. The orchestration server's builder
//     already extracts the manifest as a separate build step and
//     pipes the bytes here.
//   - --binary <path>: exec the built binary with
//     NEXUS_PRINT_MANIFEST=1 set and capture stdout. No container.
//     Use this after `go build` (or `nexus build`).
//   - --source <dir>: run `go run` against the project's main package
//     with NEXUS_PRINT_MANIFEST=1 set. Use when no binary is built
//     yet — typical local-dev workflow.
//
// Resolution order when multiple are set: --manifest-json > --binary
// > --source. Cheaper sources win so a CI step that pipes JSON in
// doesn't accidentally re-build because someone left --source on.
type reconcileOptions struct {
	ManifestJSON string
	Binary       string
	Source       string
	YAMLPath     string
	Out          string
	DryRun       bool
}

func newReconcileCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := reconcileOptions{
		YAMLPath: "nexus.deploy.yaml",
	}
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Sync nexus.deploy.yaml's declaration sections from a built binary's manifest",
		Long: `Read the runtime manifest produced by NEXUS_PRINT_MANIFEST=1 and write
its services/env/startup_tasks blocks back into nexus.deploy.yaml.

The deployments + peers sections are operator-authored and left
alone. Only the auto-generated declaration sections (services,
env, startup_tasks) are overwritten — any hand edits there will be
clobbered, by design. Source declarations (nexus.DeclareService,
DeclareEnv, AddStartupTask) are the canonical way to change them.

Three ways to feed the manifest, no docker required:

    nexus reconcile --manifest-json <file|->   # JSON already extracted
    nexus reconcile --binary ./bin/myapp       # exec built binary
    nexus reconcile --source .                 # go run the main pkg

All three write back to nexus.deploy.yaml in place. --out writes
elsewhere without touching the original (useful for diffs in CI).
--dry-run prints the merged YAML to stdout without writing anywhere.

Examples:
    nexus reconcile --binary ./bin/myapp
    nexus reconcile --source ./cmd/myapp
    nexus reconcile --manifest-json - < manifest.json
    nexus reconcile --binary ./bin/myapp --dry-run | diff nexus.deploy.yaml -
`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if opts.ManifestJSON == "" && opts.Binary == "" && opts.Source == "" {
				return fmt.Errorf("nexus reconcile: one of --manifest-json, --binary, or --source is required")
			}
			return runReconcile(opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.ManifestJSON, "manifest-json", "", "path to a pre-extracted manifest JSON file (- for stdin)")
	cmd.Flags().StringVar(&opts.Binary, "binary", "", "path to a built nexus binary; will be exec'd with NEXUS_PRINT_MANIFEST=1")
	cmd.Flags().StringVar(&opts.Source, "source", "", "Go main package (dir or import path) to `go run` with NEXUS_PRINT_MANIFEST=1 set")
	cmd.Flags().StringVar(&opts.YAMLPath, "yaml", opts.YAMLPath, "path to nexus.deploy.yaml")
	cmd.Flags().StringVar(&opts.Out, "out", "", "write merged YAML here instead of in-place (default: rewrite --yaml)")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "print merged YAML to stdout, write nothing")
	return cmd
}

// printedManifest is the JSON shape produced by NEXUS_PRINT_MANIFEST=1.
// Mirrors manifest.Manifest in the framework, but read here without
// importing the framework package (cmd/nexus is consciously
// dependency-light — operators install it via go install and we
// don't drag in fx etc.).
type printedManifest struct {
	Name         string                  `json:"name"`
	Version      string                  `json:"version,omitempty"`
	Services     []printedService        `json:"services,omitempty"`
	Env          []printedEnv            `json:"env,omitempty"`
	StartupTasks []printedTask           `json:"startupTasks,omitempty"`
	Volumes      []map[string]any        `json:"volumes,omitempty"` // platform-managed; ignored here
}

type printedService struct {
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	Version  string            `json:"version,omitempty"`
	Optional bool              `json:"optional,omitempty"`
	ExposeAs map[string]string `json:"exposeAs,omitempty"`
}

type printedEnv struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
	Default     string `json:"default,omitempty"`
	BoundTo     string `json:"boundTo,omitempty"`
}

type printedTask struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Phase       string `json:"phase,omitempty"`
}

func runReconcile(opts reconcileOptions, stdout, stderr io.Writer) error {
	jsonBytes, err := loadManifestJSON(opts, stderr)
	if err != nil {
		return err
	}
	var pm printedManifest
	if err := json.Unmarshal(jsonBytes, &pm); err != nil {
		return fmt.Errorf("nexus reconcile: parse manifest JSON: %w", err)
	}

	// Read existing YAML. Missing file is allowed in --out mode
	// (we'll create one); without --out, refuse to fabricate a
	// manifest from scratch — that's `nexus init`'s job.
	var dm DeployManifest
	yamlBytes, readErr := os.ReadFile(opts.YAMLPath)
	switch {
	case readErr == nil:
		if err := yaml.Unmarshal(yamlBytes, &dm); err != nil {
			return fmt.Errorf("nexus reconcile: parse %s: %w", opts.YAMLPath, err)
		}
	case os.IsNotExist(readErr):
		if opts.Out == "" && !opts.DryRun {
			return fmt.Errorf("nexus reconcile: %s does not exist (run `nexus init` first or pass --out to create new file)", opts.YAMLPath)
		}
		dm = DeployManifest{Deployments: map[string]DeploymentSpec{
			// Sensible default for first-time reconcile — operators
			// edit/rename after.
			"monolith": {Port: 8080},
		}}
	default:
		return fmt.Errorf("nexus reconcile: read %s: %w", opts.YAMLPath, readErr)
	}

	// Merge: replace declaration sections with print-mode output.
	// Operator-authored sections (deployments, peers) untouched.
	dm.Services = mergeServices(pm.Services)
	dm.Env = mergeEnv(pm.Env)
	dm.StartupTasks = mergeTasks(pm.StartupTasks)

	// Render. Use 2-space indent + a small header comment so a diff
	// tells the operator where the file came from. The yaml.v3 Encoder
	// preserves comments on already-parsed nodes, but we re-render
	// from the typed struct here — the header is attached at the top
	// in a fresh write.
	var buf bytes.Buffer
	buf.WriteString("# nexus.deploy.yaml — services/env/startup_tasks regenerated by `nexus reconcile`.\n")
	buf.WriteString("# Edit deployments + peers freely; the auto-generated sections come from\n")
	buf.WriteString("# nexus.DeclareService / DeclareEnv / AddStartupTask in your code.\n\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&dm); err != nil {
		_ = enc.Close()
		return fmt.Errorf("nexus reconcile: encode: %w", err)
	}
	_ = enc.Close()

	if opts.DryRun {
		_, _ = io.Copy(stdout, &buf)
		return nil
	}
	target := opts.Out
	if target == "" {
		target = opts.YAMLPath
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("nexus reconcile: mkdir for %s: %w", target, err)
	}
	if err := os.WriteFile(target, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("nexus reconcile: write %s: %w", target, err)
	}
	fmt.Fprintf(stdout, "wrote %s (%d services, %d env, %d startup_tasks)\n",
		target, len(dm.Services), len(dm.Env), len(dm.StartupTasks))
	return nil
}

// loadManifestJSON resolves the JSON bytes from whichever input the
// operator chose, in cheapest-first order: --manifest-json > --binary
// > --source. Cheaper sources win so a CI step that pipes JSON in
// doesn't accidentally re-run a full `go run` because someone left
// --source on. The stderr writer is reserved for future hints; today
// the underlying helpers report errors directly.
//
// All three paths produce the same byte stream — print-mode JSON from
// the canonical manifest package — so the rest of runReconcile is
// source-agnostic.
func loadManifestJSON(opts reconcileOptions, _ io.Writer) ([]byte, error) {
	if opts.ManifestJSON != "" {
		if opts.ManifestJSON == "-" {
			return io.ReadAll(os.Stdin)
		}
		return os.ReadFile(opts.ManifestJSON)
	}
	if opts.Binary != "" {
		// Reuses the same exec helper `nexus build --emit-manifest`
		// uses, so behavior (timeout, env passthrough, empty-output
		// detection) stays identical between the two commands.
		return runBinaryPrintMode(opts.Binary)
	}
	if opts.Source != "" {
		return runSourcePrintMode(opts.Source)
	}
	// Caller-side guard in newReconcileCmd already rejects this case;
	// duplicate the check so the helper is safe to use stand-alone.
	return nil, fmt.Errorf("nexus reconcile: no input source configured")
}

// runSourcePrintMode runs `go run <pkg>` with NEXUS_PRINT_MANIFEST=1
// set, capturing stdout. The 60s ceiling is more generous than the
// binary path because `go run` adds a compile step on every
// invocation; on a cold build of a real project that's the dominant
// cost. For repeated reconciles, prefer --binary against a
// pre-built artifact.
//
// Empty stdout → likely a non-nexus main package, or one that was
// built against an older nexus that lacks print-mode. Surface a
// pointed error so the user knows where to look, not a generic
// "exit 0 with nothing".
func runSourcePrintMode(pkg string) ([]byte, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return nil, fmt.Errorf("nexus reconcile: `go` toolchain not found on PATH (needed for --source; pass --binary against a pre-built artifact instead)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", pkg)
	// Inherit env so go's GOPATH/GOMODCACHE/etc. resolve normally,
	// then append the print-mode trigger so it always wins on a
	// duplicate key. Same shape as runBinaryPrintMode in build.go.
	cmd.Env = append(os.Environ(), "NEXUS_PRINT_MANIFEST=1")
	out, err := cmd.Output()
	if err != nil {
		stderrStr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderrStr = strings.TrimSpace(string(exitErr.Stderr))
		}
		if stderrStr != "" {
			return nil, fmt.Errorf("nexus reconcile: go run %s: %w: %s", pkg, err, stderrStr)
		}
		return nil, fmt.Errorf("nexus reconcile: go run %s: %w", pkg, err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, fmt.Errorf("nexus reconcile: %s produced empty manifest — is this a nexus main package built against a print-mode-aware framework version?", pkg)
	}
	return out, nil
}

func mergeServices(in []printedService) map[string]ServiceDeclSpec {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]ServiceDeclSpec, len(in))
	for _, s := range in {
		if s.Name == "" {
			continue
		}
		out[s.Name] = ServiceDeclSpec{
			Kind:     s.Kind,
			Version:  s.Version,
			Optional: s.Optional,
			ExposeAs: s.ExposeAs,
		}
	}
	return out
}

func mergeEnv(in []printedEnv) map[string]EnvDeclSpec {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]EnvDeclSpec, len(in))
	for _, e := range in {
		if e.Name == "" {
			continue
		}
		out[e.Name] = EnvDeclSpec{
			Description: e.Description,
			Required:    e.Required,
			Secret:      e.Secret,
			Default:     e.Default,
			BoundTo:     e.BoundTo,
		}
	}
	return out
}

func mergeTasks(in []printedTask) []StartupTaskDeclSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]StartupTaskDeclSpec, 0, len(in))
	for _, t := range in {
		if t.Name == "" {
			continue
		}
		out = append(out, StartupTaskDeclSpec{
			Name:        t.Name,
			Description: t.Description,
			Phase:       t.Phase,
		})
	}
	return out
}