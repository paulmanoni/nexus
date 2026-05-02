package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/template"

	"github.com/spf13/cobra"
)

// bakeOptions configures the docker-bake.hcl generator. Defaults
// match the dockerfile subcommand: same per-deployment Dockerfile
// naming, same module-root assumption for the build context.
type bakeOptions struct {
	// ManifestPath points at nexus.deploy.yaml; every deployment in
	// it becomes a bake target.
	ManifestPath string
	// OutputPath writes the rendered HCL here. "-" streams to stdout.
	// Defaults to docker-bake.hcl alongside the manifest, since
	// `docker buildx bake` auto-discovers that filename.
	OutputPath string
	// TagPrefix is prepended to each target's image tag (so
	// "microsplit" yields "microsplit-monolith:${TAG}"). Defaults to
	// the manifest directory's basename — a sensible identifier when
	// the user hasn't picked one.
	TagPrefix string
	// DockerfileName is the per-deployment Dockerfile basename
	// pattern. The literal "<deployment>" placeholder is substituted.
	// Defaults to "Dockerfile.<deployment>" matching what
	// `nexus generate dockerfile` writes.
	DockerfileName string
}

func newGenerateBakeCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := bakeOptions{
		ManifestPath:   "nexus.deploy.yaml",
		DockerfileName: "Dockerfile.<deployment>",
	}
	cmd := &cobra.Command{
		Use:   "bake",
		Short: "Generate a docker-bake.hcl that builds every deployment in one BuildKit run",
		Long: `Generate a docker-bake.hcl describing every deployment in nexus.deploy.yaml
as a parallel buildx target.

Why bake instead of N separate docker builds:

  - One BuildKit invocation, one cache-mount space — the Go module
    + build caches warm up once and every target reuses them. With N
    separate ` + "`docker build`" + ` commands, BuildKit either spins up N
    daemons or serializes them; either way each target re-warms its
    cache from scratch.
  - Parallel by default. ` + "`docker buildx bake`" + ` runs every target in
    the default group concurrently, capped by BuildKit's worker pool.
  - One file in version control captures the whole topology;
    operators don't have to remember which Dockerfile to point at.

Run with:
    docker buildx bake                      # build everything
    docker buildx bake monolith             # build one target
    TAG=v1.2.3 docker buildx bake --push    # tag + push to registry

Variables (override via env or --set):
    REGISTRY  prefix for image refs (default empty → local image)
    TAG       image tag (default "latest")

The generated file references one Dockerfile per deployment; run
` + "`nexus generate dockerfile --deployment <name>`" + ` for each before baking.

Examples:
    nexus generate bake
    nexus generate bake --tag-prefix mycorp/microsplit
    nexus generate bake -o -                # stream to stdout`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runGenerateBake(opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.ManifestPath, "manifest", opts.ManifestPath, "path to the deploy manifest")
	cmd.Flags().StringVarP(&opts.OutputPath, "output", "o", "", "output path; '-' for stdout; default docker-bake.hcl alongside the manifest")
	cmd.Flags().StringVar(&opts.TagPrefix, "tag-prefix", "", "image tag prefix (default: manifest dir basename)")
	cmd.Flags().StringVar(&opts.DockerfileName, "dockerfile-name", opts.DockerfileName, "Dockerfile basename pattern; '<deployment>' is substituted")
	return cmd
}

// bakeTargetData is one rendered target. The template iterates over
// these in the manifest's lexical order so the output is deterministic
// across runs (no map-iteration churn in the generated file).
type bakeTargetData struct {
	Name       string
	Dockerfile string // path relative to the build context (module root)
	Image      string // base image name (TagPrefix + "-" + Name)
}

type bakeTemplateData struct {
	Targets []bakeTargetData
	// TargetNames is the sorted list of target keys, used by the
	// "default" group's targets array. Pre-computed so the template
	// doesn't have to iterate Targets a second time.
	TargetNames []string
}

func runGenerateBake(opts bakeOptions, stdout, stderr io.Writer) error {
	manifest, err := LoadManifest(opts.ManifestPath)
	if err != nil {
		return err
	}

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

	tagPrefix := opts.TagPrefix
	if tagPrefix == "" {
		tagPrefix = filepath.Base(manifestDir)
	}

	dockerfilePattern := opts.DockerfileName
	if dockerfilePattern == "" {
		dockerfilePattern = "Dockerfile.<deployment>"
	}

	names := manifest.Names() // sorted
	targets := make([]bakeTargetData, 0, len(names))
	for _, name := range names {
		dockerfile := substituteDeployment(dockerfilePattern, name)
		// Dockerfile path is relative to the build context (module
		// root). When the manifest sits in a subdir, prepend it.
		if relPath != "" {
			dockerfile = filepath.ToSlash(filepath.Join(relPath, dockerfile))
		}
		targets = append(targets, bakeTargetData{
			Name:       name,
			Dockerfile: dockerfile,
			Image:      tagPrefix + "-" + name,
		})
	}

	data := bakeTemplateData{
		Targets:     targets,
		TargetNames: append([]string(nil), names...),
	}
	sort.Strings(data.TargetNames)

	var buf bytes.Buffer
	if err := bakeTpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("render bake file: %w", err)
	}

	output := opts.OutputPath
	if output == "-" {
		_, err := io.Copy(stdout, &buf)
		return err
	}
	if output == "" {
		output = filepath.Join(manifestDir, "docker-bake.hcl")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("mkdir output dir: %w", err)
	}
	if err := os.WriteFile(output, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}
	fmt.Fprintf(stdout, "wrote %s\n", output)
	if relPath != "" {
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Run from module root (where go.mod lives):\n")
		fmt.Fprintf(stdout, "    docker buildx bake -f %s\n", filepath.ToSlash(filepath.Join(relPath, filepath.Base(output))))
	}
	return nil
}

// substituteDeployment replaces the literal "<deployment>" token with
// the deployment name. Used so users can override the basename pattern
// (e.g. "Dockerfile-<deployment>" or just "Dockerfile" for monorepos
// that segregate Dockerfiles by directory).
func substituteDeployment(pattern, name string) string {
	out := pattern
	const tok = "<deployment>"
	for i := 0; ; {
		j := indexFrom(out, tok, i)
		if j < 0 {
			return out
		}
		out = out[:j] + name + out[j+len(tok):]
		i = j + len(name)
	}
}

func indexFrom(s, sub string, from int) int {
	if from < 0 {
		from = 0
	}
	if from >= len(s) {
		return -1
	}
	for i := from; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// bakeTpl renders the docker-bake.hcl. Comments inline explain the
// load-bearing pieces so operators editing it know what's safe to
// change. The variables block exposes the two knobs CI typically
// overrides (registry + tag) so a single file works locally and in
// production without per-env editing.
var bakeTpl = template.Must(template.New("bake").Parse(`# Generated by nexus generate bake.
# Regenerate after adding/removing deployments in nexus.deploy.yaml.
# Safe to commit — operators override REGISTRY and TAG via env vars
# or ` + "`--set`" + ` on the bake command line.
#
# Build everything in parallel with shared BuildKit cache:
#     docker buildx bake
#
# Build one target:
#     docker buildx bake {{index .TargetNames 0}}
#
# Push to a registry with a versioned tag:
#     REGISTRY=ghcr.io/myorg/ TAG=v1.2.3 docker buildx bake --push

variable "REGISTRY" {
  default = ""
}

variable "TAG" {
  default = "latest"
}

# CACHE_FROM / CACHE_TO let CI wire registry-backed remote caching
# (e.g. type=registry,ref=ghcr.io/myorg/buildcache). Empty defaults
# fall back to the local BuildKit cache, which is what you want for
# dev. Kept as variables (not hardcoded) so the same file works in
# both environments without conditional logic.
variable "CACHE_FROM" {
  default = ""
}

variable "CACHE_TO" {
  default = ""
}

group "default" {
  targets = [{{range $i, $n := .TargetNames}}{{if $i}}, {{end}}{{printf "%q" $n}}{{end}}]
}
{{range .Targets}}
target "{{.Name}}" {
  context    = "."
  dockerfile = "{{.Dockerfile}}"
  tags       = ["${REGISTRY}{{.Image}}:${TAG}"]
  cache-from = CACHE_FROM == "" ? [] : [CACHE_FROM]
  cache-to   = CACHE_TO == "" ? [] : [CACHE_TO]
}
{{end}}`))