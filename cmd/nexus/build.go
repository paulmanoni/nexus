package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/tools/go/packages"
)

// newBuildCmd builds `nexus build --deployment <name> [package]`.
//
// Reads nexus.deploy.yaml in the project root (or --manifest) to
// learn which modules each deployment owns. For every module the
// target deployment doesn't own, generates a shadow source file
// under .nexus/build/<deployment>/ that redefines the module's
// public Service as an HTTP-backed stub. Writes an overlay.json and
// invokes `go build -overlay=...` so the compiler substitutes the
// shadow files for the originals — same source tree, different
// binary per deployment, no build tags or per-deployment files in
// the user's repo.
func newBuildCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		deployment   string
		manifestPath string
		outputPath   string
		mainPkg      string
	)
	cmd := &cobra.Command{
		Use:   "build [main-package]",
		Short: "Build a deployment binary using overlay-driven shadow code",
		Long: `Build one deployment unit's binary from the shared source tree.

The framework reads nexus.deploy.yaml to learn which modules each
deployment owns locally. Modules not owned in this deployment are
replaced by HTTP-stub source files (generated under .nexus/build/)
via go build -overlay so consumer code's "users *users.Service"
field resolves to the right transport per binary — without any
build tags or per-deployment files in your repo.

Examples:
    nexus build --deployment monolith                          # main pkg = "."
    nexus build --deployment users-svc -o ./bin/users-svc
    nexus build --deployment checkout-svc ./cmd/checkout-main`,
		RunE: func(_ *cobra.Command, args []string) error {
			pkg := "."
			if mainPkg != "" {
				pkg = mainPkg
			} else if len(args) > 0 {
				pkg = args[0]
			}
			if deployment == "" {
				return fmt.Errorf("nexus build: --deployment is required")
			}
			return runBuild(buildOptions{
				Deployment:   deployment,
				ManifestPath: manifestPath,
				Output:       outputPath,
				MainPackage:  pkg,
				Stdout:       stdout,
				Stderr:       stderr,
			})
		},
	}
	cmd.Flags().StringVar(&deployment, "deployment", "", "deployment unit to build (must match a key in nexus.deploy.yaml)")
	cmd.Flags().StringVar(&manifestPath, "manifest", "nexus.deploy.yaml", "path to the deploy manifest")
	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "output binary path; defaults to ./bin/<deployment>")
	cmd.Flags().StringVar(&mainPkg, "package", "", "Go main package to build (defaults to '.')")
	return cmd
}

type buildOptions struct {
	Deployment   string
	ManifestPath string
	Output       string
	MainPackage  string // single main package to compile
	Stdout       io.Writer
	Stderr       io.Writer
}

// runBuild orchestrates manifest read → module scan → shadow
// generation → overlay write → go build invocation. Separated from
// the cobra glue so tests can drive it in-process.
func runBuild(opts buildOptions) error {
	manifest, err := LoadManifest(opts.ManifestPath)
	if err != nil {
		return err
	}
	if _, ok := manifest.Deployments[opts.Deployment]; !ok {
		return fmt.Errorf("nexus build: deployment %q not in %s — declared: %v",
			opts.Deployment, opts.ManifestPath, manifest.Names())
	}

	projectRoot, err := filepath.Abs(filepath.Dir(opts.ManifestPath))
	if err != nil {
		return fmt.Errorf("resolve project root: %w", err)
	}

	// Scan every nexus.Module(...) declaration so we know each
	// module's name, DeployAs tag, source file path, and endpoints.
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
			packages.NeedImports,
		Tests: false,
	}
	// Scan recursively from the project root regardless of the
	// build target — modules can live in any subpackage; the build
	// target is just one main package among many.
	scanCfg := *cfg
	scanCfg.Dir = projectRoot
	pkgs, err := packages.Load(&scanCfg, "./...")
	if err != nil {
		return fmt.Errorf("load packages: %w", err)
	}
	if hasErrors(pkgs) {
		for _, p := range pkgs {
			for _, e := range p.Errors {
				fmt.Fprintf(opts.Stderr, "warn: %s\n", e)
			}
		}
	}

	mods := scanModules(pkgs)
	if len(mods) == 0 {
		return fmt.Errorf("nexus build: no nexus.Module(...) declarations found under %s", projectRoot)
	}

	// Cross-validate manifest ↔ source. A common silent bug: a module's
	// nexus.DeployAs("foo-srv") doesn't match any manifest deployment
	// name (manifest says "foo-svc"), so under nexus dev --split the
	// runtime filter skips the local module wholesale and the dashboard
	// shows nothing for it. Surface the mismatch as a build error rather
	// than producing a half-broken binary.
	if err := validateManifestSourceTags(mods, manifest, opts.Stderr); err != nil {
		return err
	}

	// Generate shadows for every module the target deployment doesn't
	// own. A module without a DeployAs tag has no peer URL to resolve
	// — those stay local in every deployment by construction (the
	// existing options.go module-filter rule).
	shadowDir := filepath.Join(projectRoot, ".nexus", "build", opts.Deployment)
	if err := os.RemoveAll(shadowDir); err != nil {
		return fmt.Errorf("clean shadow dir: %w", err)
	}
	if err := os.MkdirAll(shadowDir, 0o755); err != nil {
		return fmt.Errorf("create shadow dir: %w", err)
	}

	overlay := overlayJSON{Replace: map[string]string{}}
	for _, m := range mods {
		// Source-side DeployAs wins; otherwise infer from manifest's
		// owns mapping (auto-inject path — modules without explicit
		// DeployAs in source still need a tag for shadow generation).
		if m.Tag == "" {
			m.Tag = manifest.DeploymentOf(m.Name)
		}
		if m.Tag == "" {
			continue // truly untagged module — never shadowed
		}
		if manifest.Owns(opts.Deployment, m.Name) {
			continue // local in this deployment
		}
		// Module is remote in the target deployment — generate the
		// per-package shadow set: every .go file gets a stripped
		// variant (preserved types only), and one synthesized file
		// (zz_shadow_gen.go) holds the stub Service + methods +
		// Module + init. Works for single-file and multi-file
		// modules uniformly.
		files, err := renderShadowPackage(m, projectRoot, opts.Deployment)
		if err != nil {
			return fmt.Errorf("shadow %q: %w", m.Name, err)
		}
		for _, sf := range files {
			overlay.Replace[sf.Original] = sf.Generated
			fmt.Fprintf(opts.Stdout, "shadow %s → %s\n", relTo(projectRoot, sf.Original), relTo(projectRoot, sf.Generated))
		}
		// Surface any endpoints we had to drop from the shadow because
		// their signatures reference unexported types — the user sees
		// what cross-module surface they're losing.
		for _, ep := range m.Endpoints {
			if ep.Skip {
				fmt.Fprintf(opts.Stderr, "  skip %s.%s: %s\n", m.Name, ep.OpName, ep.SkipReason)
			}
		}
	}

	// Generate the deploy-defaults init file for the active
	// deployment. The file's init() block calls
	// nexus.SetDeploymentDefaults(...) so the binary boots with
	// the right port and peer table without main.go declaring
	// anything. Overlay-added (logical path doesn't exist on disk)
	// so it doesn't pollute the source tree.
	if depFile, err := writeDeployInitFile(opts.Deployment, manifest, projectRoot, opts.MainPackage, shadowDir, mods); err != nil {
		return err
	} else if depFile != "" {
		// Overlay an additional file by mapping a logical
		// (non-existing) path under the main package directory to
		// the generated source. go build accepts this; the source
		// file is treated as if it were on disk.
		mainPkgDir, err := mainPackageDir(projectRoot, opts.MainPackage)
		if err != nil {
			return err
		}
		logicalPath := filepath.Join(mainPkgDir, "zz_deploy_gen.go")
		overlay.Replace[logicalPath] = depFile
		fmt.Fprintf(opts.Stdout, "deploy-init %s → %s\n", relTo(projectRoot, logicalPath), relTo(projectRoot, depFile))
	}

	overlayPath := filepath.Join(shadowDir, "overlay.json")
	if err := writeOverlay(overlayPath, overlay); err != nil {
		return err
	}

	// Resolve output path: ./bin/<deployment> by default. Mkdir
	// the parent so go build doesn't fail on a missing dir.
	output := opts.Output
	if output == "" {
		output = filepath.Join(projectRoot, "bin", opts.Deployment)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("mkdir bin dir: %w", err)
	}

	// Invoke go build with the overlay. Stdout/stderr stream through
	// so the user sees gc errors live.
	args := []string{"build", "-o", output}
	if len(overlay.Replace) > 0 {
		args = append(args, "-overlay="+overlayPath)
	}
	args = append(args, opts.MainPackage)
	cmd := exec.Command("go", args...)
	cmd.Dir = projectRoot
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	fmt.Fprintf(opts.Stdout, "go %s\n", joinArgs(args))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build failed: %w", err)
	}
	fmt.Fprintf(opts.Stdout, "built %s\n", relTo(projectRoot, output))
	return nil
}

// overlayJSON matches the shape `go build -overlay=` expects.
// Documented at https://pkg.go.dev/cmd/go: a JSON file with a single
// "Replace" object mapping logical paths to alternate disk paths.
type overlayJSON struct {
	Replace map[string]string `json:"Replace"`
}

func writeOverlay(path string, ov overlayJSON) error {
	data, err := json.MarshalIndent(ov, "", "  ")
	if err != nil {
		return fmt.Errorf("encode overlay: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write overlay %s: %w", path, err)
	}
	return nil
}

// validateManifestSourceTags fails the build when source-side
// nexus.DeployAs(...) tags reference deployment names that don't
// exist in the manifest. The most common case: a typo like
// DeployAs("foo-srv") in source vs `foo-svc:` in the manifest —
// the runtime module filter then skips the local module entirely
// under nexus dev --split, with no error and no dashboard signal.
//
// Modules without a DeployAs (manifest-driven via owns) skip the
// check; their tag is inferred from the manifest by definition.
//
// Reports every offender at once so the user sees the full picture
// in one build attempt rather than one mismatch per run.
func validateManifestSourceTags(mods []modInfo, manifest *DeployManifest, stderr io.Writer) error {
	if manifest == nil {
		return nil
	}
	deployNames := map[string]bool{}
	for name := range manifest.Deployments {
		deployNames[name] = true
	}
	var problems []string
	for _, m := range mods {
		if m.Tag == "" {
			continue // manifest-driven (auto-inject) — skip
		}
		if !deployNames[m.Tag] {
			// Surface near-misses (Levenshtein distance ≤ 2) as a
			// hint — typical typos like "srv" vs "svc" land here.
			suggestion := nearestDeploymentName(m.Tag, deployNames)
			line := fmt.Sprintf("module %q in source declares nexus.DeployAs(%q), but no deployment named %q exists in the manifest",
				m.Name, m.Tag, m.Tag)
			if suggestion != "" {
				line += fmt.Sprintf(" — did you mean %q?", suggestion)
			}
			problems = append(problems, line)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	msg := "nexus build: manifest / source mismatch detected:"
	for _, p := range problems {
		msg += "\n  - " + p
	}
	msg += "\n\nFix: rename either the source DeployAs tag or the manifest deployment so they match. Without a match, nexus dev --split's module filter silently skips the local module and the dashboard shows nothing for it."
	return fmt.Errorf("%s", msg)
}

// nearestDeploymentName picks the manifest deployment name whose
// edit-distance to want is smallest, or "" when every candidate is
// further than the typo-tolerance threshold. Catches "srv" vs "svc"
// (distance 1) without flagging genuinely-different names.
func nearestDeploymentName(want string, candidates map[string]bool) string {
	best := ""
	bestDist := 3 // ≥3 = treat as unrelated
	for name := range candidates {
		d := levenshtein(want, name)
		if d < bestDist {
			bestDist = d
			best = name
		}
	}
	return best
}

// levenshtein computes the standard edit distance between a and b.
// Used by the typo-suggestion logic; small inputs (deployment names),
// no need for the optimized two-row variant.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	curr := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = minInt(curr[j-1]+1, minInt(prev[j]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func hasGoSuffix(name string) bool {
	return len(name) > 3 && name[len(name)-3:] == ".go"
}

// isGeneratedName returns true for filenames the codegen writes
// (zz_shadow_gen.go, zz_deploy_gen.go, etc.). Skipping them when
// shadowing prevents the shadow input from picking up files that
// don't belong on the shadow side.
func isGeneratedName(name string) bool {
	return strings.HasPrefix(name, "zz_")
}

func relTo(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return rel
}

func joinArgs(args []string) string {
	return strings.Join(args, " ")
}
