package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
)

// dockerfileOptions carries the resolved knobs the template needs.
// Defaults match what `nexus dev --split` and the microsplit example
// already use, so a freshly-generated Dockerfile boots without
// per-environment tweaks.
type dockerfileOptions struct {
	// Deployment names the unit to build (must match a key in
	// nexus.deploy.yaml's `deployments:` block).
	Deployment string
	// ManifestPath is the path to nexus.deploy.yaml (defaults to
	// "nexus.deploy.yaml" in the project root).
	ManifestPath string
	// OutputPath writes the rendered Dockerfile here. "-" or empty
	// streams to stdout. Defaults to ./Dockerfile.<deployment>
	// when empty AND not piped — keeps multiple deployments side by
	// side without colliding.
	OutputPath string
	// GoVersion is the golang base-image tag for the build stage.
	GoVersion string
	// RuntimeImage is the base image for the runtime stage. Default
	// alpine — small, has wget for the HEALTHCHECK probe.
	RuntimeImage string
	// AdminPortOffset matches microsplit's main.go: admin listener
	// binds at public+offset. Set to 0 to skip the EXPOSE for it
	// (single-listener deployments).
	AdminPortOffset int
	// NexusVersion pins the CLI installed in the build stage. "latest"
	// is the convenient default; pin to a tag for reproducible
	// builds.
	NexusVersion string
}

func newGenerateDockerfileCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := dockerfileOptions{
		ManifestPath:    "nexus.deploy.yaml",
		GoVersion:       "1.25",
		RuntimeImage:    "alpine:3.20",
		AdminPortOffset: 1000,
		NexusVersion:    "latest",
	}
	cmd := &cobra.Command{
		Use:   "dockerfile --deployment <name>",
		Short: "Generate a multi-stage Dockerfile for one deployment unit",
		Long: `Generate a multi-stage Dockerfile that:

  1. Installs the nexus CLI in a golang builder image
  2. Runs nexus build --deployment <name> against your source
  3. Copies the resulting binary into a small runtime image
  4. EXPOSEs the manifest's port (and the admin port at public+1000)
  5. Wires HEALTHCHECK to /__nexus/health

The Dockerfile lives at the project root by default (Dockerfile.<deployment>)
so multiple deployments can be built side-by-side without colliding. Pipe
to stdout with -o -.

Examples:
    nexus generate dockerfile --deployment users-svc
    nexus generate dockerfile --deployment checkout-svc -o Dockerfile.checkout
    nexus generate dockerfile --deployment monolith -o -    # stream to stdout`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if opts.Deployment == "" {
				return fmt.Errorf("nexus generate dockerfile: --deployment is required")
			}
			return runGenerateDockerfile(opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.Deployment, "deployment", "", "deployment unit to build (must match a key in nexus.deploy.yaml)")
	cmd.Flags().StringVar(&opts.ManifestPath, "manifest", opts.ManifestPath, "path to the deploy manifest")
	cmd.Flags().StringVarP(&opts.OutputPath, "output", "o", "", "output path; '-' for stdout; default Dockerfile.<deployment>")
	cmd.Flags().StringVar(&opts.GoVersion, "go-version", opts.GoVersion, "golang image tag for the build stage")
	cmd.Flags().StringVar(&opts.RuntimeImage, "runtime-image", opts.RuntimeImage, "base image for the runtime stage (must include wget or curl for the healthcheck)")
	cmd.Flags().IntVar(&opts.AdminPortOffset, "admin-port-offset", opts.AdminPortOffset, "offset added to the manifest port for the admin listener (0 to skip)")
	cmd.Flags().StringVar(&opts.NexusVersion, "nexus-version", opts.NexusVersion, "version of the nexus CLI to install in the build stage")
	return cmd
}

// dockerfileTemplateData is the shape passed to the template. Carved
// out of dockerfileOptions so the template can reference computed
// fields (PublicPort, AdminPort, etc.) without re-deriving them on
// every {{...}} expansion.
type dockerfileTemplateData struct {
	dockerfileOptions
	PublicPort int
	AdminPort  int // 0 → omit EXPOSE
	// ModulePath is the relative path from the build context root
	// (where go.mod lives) to the directory holding the manifest.
	// Empty when the manifest is at the module root (the simple
	// case — go.mod alongside nexus.deploy.yaml). Non-empty for
	// in-tree examples like examples/microsplit, where the build
	// context must be the repo root and `nexus build` runs from
	// the subdir.
	ModulePath string
	// BuildContextHint is the docker build command the user should
	// run, embedded in the generated comments so they don't have to
	// reverse-engineer the right -f / context-dir combination.
	BuildContextHint string
	// HasGoSum is true when go.sum exists at the module root. The
	// COPY line uses a glob when it might not (fresh modules without
	// any external deps), but the deterministic path is cleaner
	// when go.sum is known to be there.
	HasGoSum bool
}

// runGenerateDockerfile loads the manifest, derives ports, renders
// the template, and writes the output. Separated from the cobra glue
// so tests can drive it in-process without spawning a subprocess.
func runGenerateDockerfile(opts dockerfileOptions, stdout, stderr io.Writer) error {
	manifest, err := LoadManifest(opts.ManifestPath)
	if err != nil {
		return err
	}
	spec, ok := manifest.Deployments[opts.Deployment]
	if !ok {
		return fmt.Errorf("nexus generate dockerfile: deployment %q not in %s — declared: %v",
			opts.Deployment, opts.ManifestPath, manifest.Names())
	}
	publicPort := spec.Port
	if publicPort == 0 {
		// Match the framework's runtime fallback so the EXPOSE line
		// matches what the binary will actually bind to.
		publicPort = 8080
	}
	adminPort := 0
	if opts.AdminPortOffset > 0 {
		adminPort = publicPort + opts.AdminPortOffset
	}

	// Resolve the module root (where go.mod lives). For a standalone
	// project this is the manifest dir; for in-tree examples like
	// nexus's own examples/microsplit it's a parent — the build
	// context has to be the module root because that's where go.mod
	// + go.sum live.
	manifestAbs, err := filepath.Abs(opts.ManifestPath)
	if err != nil {
		return fmt.Errorf("resolve manifest path: %w", err)
	}
	manifestDir := filepath.Dir(manifestAbs)
	moduleRoot, err := findModuleRoot(manifestDir)
	if err != nil {
		return err
	}
	relPath, err := filepath.Rel(moduleRoot, manifestDir)
	if err != nil {
		return fmt.Errorf("compute module-relative path: %w", err)
	}
	if relPath == "." {
		relPath = ""
	}
	hasGoSum := false
	if _, err := os.Stat(filepath.Join(moduleRoot, "go.sum")); err == nil {
		hasGoSum = true
	}
	hint := buildContextHint(relPath, opts.Deployment, opts.OutputPath)

	data := dockerfileTemplateData{
		dockerfileOptions: opts,
		PublicPort:        publicPort,
		AdminPort:         adminPort,
		ModulePath:        relPath,
		BuildContextHint:  hint,
		HasGoSum:          hasGoSum,
	}

	var buf bytes.Buffer
	if err := dockerfileTpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("render dockerfile: %w", err)
	}

	output := opts.OutputPath
	if output == "-" {
		_, err := io.Copy(stdout, &buf)
		return err
	}
	if output == "" {
		output = "Dockerfile." + opts.Deployment
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("mkdir output dir: %w", err)
	}
	if err := os.WriteFile(output, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}
	fmt.Fprintf(stdout, "wrote %s\n", output)
	// When the manifest is in a subdir of the module, `docker build .`
	// from the manifest dir fails (no go.mod in the build context).
	// Print the exact command + cd hint so the next thing the user
	// types just works.
	if data.ModulePath != "" {
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Build context must be the module root (where go.mod lives).\n")
		fmt.Fprintf(stdout, "From module root, run:\n")
		fmt.Fprintf(stdout, "    %s\n", data.BuildContextHint)
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Or from this directory (%s/):\n", data.ModulePath)
		fmt.Fprintf(stdout, "    cd %s && %s\n", relUp(data.ModulePath), data.BuildContextHint)
	}
	return nil
}

// relUp turns "examples/microsplit" into "../.." — the cd hint to
// climb out of the subdir and up to the module root. Lets the stdout
// message offer a working command that runs in one shell line from
// the manifest dir, not just from the module root.
func relUp(rel string) string {
	if rel == "" || rel == "." {
		return "."
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 0 {
		return "."
	}
	out := make([]string, len(parts))
	for i := range parts {
		out[i] = ".."
	}
	return strings.Join(out, "/")
}

// findModuleRoot walks up from start looking for go.mod. Returns the
// first directory that contains it. Errors when no go.mod is reachable
// (the user pointed at a manifest outside any Go module — would fail
// `nexus build` later anyway, but failing here gives a clearer error).
func findModuleRoot(start string) (string, error) {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("nexus generate dockerfile: no go.mod found at or above %s — the build context must be inside a Go module", start)
		}
		dir = parent
	}
}

// buildContextHint builds the docker build command line the user
// should run, baked into the generated Dockerfile's header. Tells
// the user explicitly that the build context = module root and the
// -f flag points at this Dockerfile, which is the most common
// confusion when the manifest lives in a subdir.
func buildContextHint(relPath, deployment, outputPath string) string {
	dockerfile := outputPath
	if dockerfile == "" || dockerfile == "-" {
		dockerfile = "Dockerfile." + deployment
	}
	if relPath == "" {
		return "docker build -f " + dockerfile + " ."
	}
	// Manifest in a subdir: -f must point at the Dockerfile that the
	// generator wrote (typically next to the manifest), context = ".".
	return "docker build -f " + filepath.Join(relPath, dockerfile) + " ."
}

// dockerfileTpl is the rendered output. Kept compact and operator-
// readable: every line that isn't a Dockerfile instruction is a
// single-sentence comment explaining the why. Operators routinely
// edit Dockerfiles by hand; the comments tell them which lines are
// safe to change vs which encode framework contracts (the
// HEALTHCHECK target, the NEXUS_DEPLOYMENT env, etc.).
//
// Indentation uses spaces — Dockerfiles tolerate tabs but tooling
// (linters, syntax highlighters) is happiest with spaces.
var dockerfileTpl = template.Must(template.New("dockerfile").
	Funcs(template.FuncMap{"join": strings.Join}).
	Parse(`# Generated by nexus generate dockerfile.
# Regenerate after changes to nexus.deploy.yaml; safe to commit but the
# build stage's nexus CLI version + the EXPOSE/HEALTHCHECK lines below
# encode framework contracts and should not be edited by hand.
# syntax=docker/dockerfile:1.7
#
# Build with:
#     {{.BuildContextHint}}{{if .ModulePath}}
#
# Context = repo root (where go.mod lives). The Dockerfile lives in
# {{.ModulePath}}/ alongside nexus.deploy.yaml; -f points at it.{{end}}

# --- Build stage --------------------------------------------------------
# golang base does the overlay-driven shadow build. The nexus CLI emits
# .nexus/build/{{.Deployment}}/ shadow files and runs go build with -overlay
# so this stage produces a single static binary at /out/{{.Deployment}}.
#
# BuildKit cache mounts on /root/.cache/go-build and /go/pkg/mod
# persist Go's compile cache + module download cache across builds.
# Without them, every docker build re-downloads modules and
# recompiles every transitive dep from scratch — these mounts turn
# repeat builds into incremental compiles. Requires BuildKit (default
# in Docker 23+; otherwise prefix the build with DOCKER_BUILDKIT=1).
FROM golang:{{.GoVersion}}-alpine AS builder
WORKDIR /src
RUN apk add --no-cache git ca-certificates
# Pinning NexusVersion (default "latest") to a tag makes this layer
# reproducible — and even with "latest", the cache mounts below keep
# the install fast on re-runs by reusing the module + build cache.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go install github.com/paulmanoni/nexus/cmd/nexus@{{.NexusVersion}}

# Module cache layer: copying go.mod{{if .HasGoSum}}/go.sum{{end}} first means dep changes
# invalidate the cache here, but source-only changes reuse it. The
# mount keeps the actual download cached even when this layer
# re-runs (e.g. a single-line go.mod tweak).
COPY go.mod {{if .HasGoSum}}go.sum {{end}}./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .{{if .ModulePath}}
# nexus.deploy.yaml lives in {{.ModulePath}}/; switch to that subdir
# so the manifest, packages.Load, and overlay paths all resolve.
WORKDIR /src/{{.ModulePath}}{{end}}
# nexus build shells out to go build internally, which honors the
# same cache mounts — incremental rebuilds collapse to seconds.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    nexus build --deployment {{.Deployment}} -o /out/{{.Deployment}}

# --- Runtime stage ------------------------------------------------------
# Small image carries only the binary + ca-certs (for any outbound TLS
# the deployment makes to peers). Switch RuntimeImage to a distroless
# variant if you don't need a shell — but mind the HEALTHCHECK target,
# which assumes wget is on PATH.
FROM {{.RuntimeImage}} AS runtime
RUN apk add --no-cache ca-certificates wget || true
WORKDIR /app
COPY --from=builder /out/{{.Deployment}} /app/{{.Deployment}}

# NEXUS_DEPLOYMENT is the secondary source for the active unit name
# (after Config.Deployment / SetDeploymentDefaults). Setting it here
# lets the same image be reused across environments by overriding via
# docker run -e NEXUS_DEPLOYMENT=... if needed.
ENV NEXUS_DEPLOYMENT={{.Deployment}}

# Public port carries REST + GraphQL + WebSocket + GraphQL playground.{{if .AdminPort}}
# Admin port carries the /__nexus dashboard surface. Keep it off the
# public network in production (k8s NetworkPolicy / cloud SG).{{end}}
EXPOSE {{.PublicPort}}{{if .AdminPort}} {{.AdminPort}}{{end}}

# Liveness probe: 200 once fx Start completes. Readiness lives at
# /__nexus/ready and is more useful in k8s where the orchestrator can
# split traffic gating from process liveness.{{if .AdminPort}}
# Probe targets the admin port because the public listener's scope
# filter hides /__nexus/* — health/ready are dashboard-namespaced.{{end}}
HEALTHCHECK --interval=10s --timeout=2s --start-period=10s --retries=3 \
  CMD wget -q --spider http://localhost:{{if .AdminPort}}{{.AdminPort}}{{else}}{{.PublicPort}}{{end}}/__nexus/health || exit 1

ENTRYPOINT ["/app/{{.Deployment}}"]
`))
