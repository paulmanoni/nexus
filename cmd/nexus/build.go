package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/tools/go/packages"

	nexusmanifest "github.com/paulmanoni/nexus/manifest"
)

// manifestEmitAuto is the sentinel cobra fills in when --emit-manifest
// is passed bare (no `=value`). runBuild sees it and resolves the real
// destination path from the build's output binary location.
const manifestEmitAuto = "<auto>"

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
		emitManifest string
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
    nexus build --deployment checkout-svc ./cmd/checkout-main
    nexus build --deployment monolith --emit-manifest          # ./bin/monolith.manifest.json
    nexus build --deployment monolith --emit-manifest=/tmp/m.json

--emit-manifest extracts the just-built binary's NEXUS_PRINT_MANIFEST
output to a JSON file, enriched with the topology declared in
nexus.deploy.yaml (Deployments + Owns + Peers — fields print mode
alone cannot see). The orchestration platform reads this file to
plan the deploy without booting the binary first.`,
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
				EmitManifest: emitManifest,
				Stdout:       stdout,
				Stderr:       stderr,
			})
		},
	}
	cmd.Flags().StringVar(&deployment, "deployment", "", "deployment unit to build (must match a key in nexus.deploy.yaml)")
	cmd.Flags().StringVar(&manifestPath, "manifest", "nexus.deploy.yaml", "path to the deploy manifest")
	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "output binary path; defaults to ./bin/<deployment>")
	cmd.Flags().StringVar(&mainPkg, "package", "", "Go main package to build (defaults to '.')")
	cmd.Flags().StringVar(&emitManifest, "emit-manifest", "", "extract the binary's manifest JSON to this path (bare flag → <output>.manifest.json)")
	// NoOptDefVal makes `--emit-manifest` (no value) valid: cobra fills
	// in the sentinel below, runBuild resolves it to <output>.manifest.json.
	// Without this, bare --emit-manifest errors with "needs an argument".
	cmd.Flag("emit-manifest").NoOptDefVal = manifestEmitAuto
	return cmd
}

type buildOptions struct {
	Deployment   string
	ManifestPath string
	Output       string
	MainPackage  string // single main package to compile
	// EmitManifest is the destination for the build's manifest JSON.
	// Empty: skip extraction. manifestEmitAuto sentinel: derive from
	// Output path (<binary>.manifest.json). Any other value: literal
	// path to write to.
	EmitManifest string
	Stdout       io.Writer
	Stderr       io.Writer

	// Preloaded lets a caller (typically nexus dev --split, which
	// builds N deployments in a row) share a single packages.Load +
	// manifest read across all builds. When nil, runBuild does its
	// own load. When set, the load is skipped and the cached scan
	// reused, which dominates wall-clock for cold builds on real
	// projects.
	Preloaded *buildPreload
}

// buildPreload bundles the expensive shared inputs (manifest + AST
// scan results) so multiple deployments can be built without
// re-paying the load cost each time. Produced by preloadBuild and
// passed via buildOptions.Preloaded.
type buildPreload struct {
	Manifest    *DeployManifest
	ProjectRoot string
	Mods        []modInfo
}

// loadManifestForBuild reads the manifest and resolves the project
// root without doing any source scanning. The monolith fast path in
// runBuild uses it to decide whether the expensive packages.Load is
// necessary, and to feed the deploy-init generator (which only needs
// manifest data when no shadows are involved).
func loadManifestForBuild(manifestPath string) (*DeployManifest, string, error) {
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, "", err
	}
	projectRoot, err := filepath.Abs(filepath.Dir(manifestPath))
	if err != nil {
		return nil, "", fmt.Errorf("resolve project root: %w", err)
	}
	return manifest, projectRoot, nil
}

// preloadBuild does the manifest read + recursive packages.Load +
// scanModules + cross-validation once, returning a result that
// can be threaded through several runBuild calls. Splitter mode
// uses this to avoid running the slow type-check N times for N
// deployments. Direct nexus build callers don't need it; runBuild
// falls back to its own load when Preloaded is nil.
func preloadBuild(manifestPath string, stderr io.Writer) (*buildPreload, error) {
	manifest, projectRoot, err := loadManifestForBuild(manifestPath)
	if err != nil {
		return nil, err
	}
	// Mode is the minimum set the scan needs:
	//   - NeedName/NeedFiles: PkgPath, Name, GoFiles → PackageDir.
	//   - NeedSyntax: AST walk in scanModules.
	//   - NeedTypes/NeedTypesInfo: TypesInfo.ObjectOf for handler
	//     resolution; types.Type walking for ArgsType/Return strings
	//     and cross-module dep struct-field detection.
	//
	// NeedDeps and NeedImports were dropped: nothing reads
	// pkg.Imports here, and dependent packages' types load via
	// export data (sufficient for paramsTypeArg + collectPackagePaths
	// to resolve obj.Pkg().Path()). Skipping NeedDeps avoids a full
	// transitive type-check of every dep tree leaf, which dominates
	// cold-load time on real projects.
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo,
		Tests: false,
		Dir:   projectRoot,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	if hasErrors(pkgs) {
		for _, p := range pkgs {
			for _, e := range p.Errors {
				fmt.Fprintf(stderr, "warn: %s\n", e)
			}
		}
	}
	mods := scanModules(pkgs)
	if len(mods) == 0 {
		return nil, fmt.Errorf("nexus build: no nexus.Module(...) declarations found under %s", projectRoot)
	}
	if err := validateManifestSourceTags(mods, manifest, stderr); err != nil {
		return nil, err
	}
	return &buildPreload{
		Manifest:    manifest,
		ProjectRoot: projectRoot,
		Mods:        mods,
	}, nil
}

// runBuild orchestrates manifest read → module scan → shadow
// generation → overlay write → go build invocation. Separated from
// the cobra glue so tests can drive it in-process.
//
// When opts.Preloaded is non-nil, the manifest + packages.Load
// results are reused from the caller's earlier preloadBuild call.
// nexus dev --split uses this so the slow type-check runs once
// across all deployments instead of N times.
func runBuild(opts buildOptions) error {
	var (
		manifest    *DeployManifest
		projectRoot string
		mods        []modInfo
	)
	if opts.Preloaded != nil {
		manifest = opts.Preloaded.Manifest
		projectRoot = opts.Preloaded.ProjectRoot
		mods = opts.Preloaded.Mods
	} else {
		// Read the manifest first (cheap) so we can decide whether
		// the heavyweight packages.Load is even necessary. A monolith
		// deployment (omitted `owns:` → DeploymentSpec.Owns == nil
		// → manifest.Owns returns true for every module) generates
		// no shadows by construction, so we can skip the recursive
		// type-check and shell straight to go build below.
		m, root, err := loadManifestForBuild(opts.ManifestPath)
		if err != nil {
			return err
		}
		manifest = m
		projectRoot = root
		spec, ok := manifest.Deployments[opts.Deployment]
		if !ok {
			return fmt.Errorf("nexus build: deployment %q not in %s — declared: %v",
				opts.Deployment, opts.ManifestPath, manifest.Names())
		}
		// Only run the full scan when this deployment may need
		// shadows. OwnsAll → "owns everything" (monolith — no
		// shadows); explicit empty → "owns nothing" (frontend-only,
		// needs shadows for every module); listed → split unit
		// (needs shadows for non-listed modules). The latter two
		// require source scan, signaled by spec.Owns being non-nil.
		//
		// Trade-off for the fast path: the static cross-module dep
		// arrows the dashboard draws from struct-field references
		// (RegisterCrossModuleDep, emitted by buildCrossModuleDepRegistrations)
		// are skipped. Runtime reflection in automount still picks up
		// peers injected as handler deps — only field-only references
		// (svc.users used internally without appearing in any handler
		// signature) lose their dashboard edge in monolith builds.
		if spec.Owns != nil {
			pre, err := preloadBuild(opts.ManifestPath, opts.Stderr)
			if err != nil {
				return err
			}
			manifest = pre.Manifest
			projectRoot = pre.ProjectRoot
			mods = pre.Mods
		}
	}
	if _, ok := manifest.Deployments[opts.Deployment]; !ok {
		return fmt.Errorf("nexus build: deployment %q not in %s — declared: %v",
			opts.Deployment, opts.ManifestPath, manifest.Names())
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

	if opts.EmitManifest != "" {
		emitPath := opts.EmitManifest
		if emitPath == manifestEmitAuto {
			emitPath = output + ".manifest.json"
		} else if !filepath.IsAbs(emitPath) {
			// Resolve relative paths against the project root, not the
			// process cwd, so `nexus build --emit-manifest=foo.json`
			// from a subdir lands the file in the same place as the
			// other build artifacts.
			emitPath = filepath.Join(projectRoot, emitPath)
		}
		if err := emitBuildManifest(output, manifest, emitPath, opts.Stdout); err != nil {
			return fmt.Errorf("emit-manifest: %w", err)
		}
		fmt.Fprintf(opts.Stdout, "manifest %s\n", relTo(projectRoot, emitPath))
	}
	return nil
}

// emitBuildManifest runs the just-built binary in print mode, parses
// the JSON it produces, augments it with build-time-known topology
// (Deployments + Owns + Peers from nexus.deploy.yaml — fields the
// running binary cannot infer because it doesn't read the YAML),
// recomputes ManifestHash so consumers diffing on the hash see the
// enriched shape, and writes a pretty-printed copy to outPath.
//
// Hard-fail on extraction errors: the user explicitly asked for the
// manifest, so a non-nexus binary or a print-mode failure is a real
// problem, not a "best effort" miss.
func emitBuildManifest(binaryPath string, dm *DeployManifest, outPath string, stdout io.Writer) error {
	jsonBytes, err := runBinaryPrintMode(binaryPath)
	if err != nil {
		return err
	}
	// Strip stdout pollution between begin/end markers; v0 binaries
	// without markers pass through as-is.
	jsonBytes, err = nexusmanifest.Extract(jsonBytes)
	if err != nil {
		return fmt.Errorf("extract manifest: %w", err)
	}
	var m nexusmanifest.Manifest
	if err := json.Unmarshal(jsonBytes, &m); err != nil {
		// Surface a snippet so the user sees what came out — useful when
		// a non-nexus binary printed something unexpected.
		snippet := strings.TrimSpace(string(jsonBytes))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		return fmt.Errorf("parse print-mode JSON: %w (output starts: %q)", err, snippet)
	}
	if dep := topologyFromDeployYAML(dm); len(dep) > 0 {
		m.Deployments = dep
		// Hash must be recomputed: the binary's own ComputeHash didn't
		// see the topology we just merged in, so the hash it carried
		// no longer matches the document. ComputeHash excludes
		// App.GeneratedAt, so this stays deterministic across re-emits.
		m.ManifestHash = nexusmanifest.ComputeHash(m)
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode merged manifest: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", outPath, err)
	}
	// Trailing newline so the file plays nice with line-oriented tools
	// (cat, diff, git) that flag missing-final-newline.
	if err := os.WriteFile(outPath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	return nil
}

// runBinaryPrintMode execs the binary with NEXUS_PRINT_MANIFEST=1 and
// returns its stdout. The binary is expected to print its manifest
// JSON and exit 0 within a few seconds; the 30s ceiling protects
// against a non-nexus binary that ignores the env var and starts a
// long-running server.
func runBinaryPrintMode(binaryPath string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binaryPath)
	// Inherit the parent env so the binary's config-load can resolve
	// any vars it consults during print mode (provider declarations
	// should be side-effect free, but constructors that read env to
	// decide *whether* to register are common). Then append the
	// print-mode trigger so it always wins on a duplicate key.
	cmd.Env = append(os.Environ(), nexusmanifest.EnvVarPrintAndExit+"=1")
	out, err := cmd.Output()
	if err != nil {
		stderrStr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderrStr = strings.TrimSpace(string(exitErr.Stderr))
		}
		if stderrStr != "" {
			return nil, fmt.Errorf("run %s in print mode: %w: %s", binaryPath, err, stderrStr)
		}
		return nil, fmt.Errorf("run %s in print mode: %w", binaryPath, err)
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, fmt.Errorf("binary %s produced no output in print mode — does it call nexus.Run with the framework version that emits manifests?", binaryPath)
	}
	return out, nil
}

// topologyFromDeployYAML projects nexus.deploy.yaml's deployments +
// peers map onto the runtime manifest's Deployment slice. Print mode
// can't produce these fields (the binary doesn't read the YAML) — the
// build tool fills them in once, here, so the orchestration platform
// gets the full topology in a single document.
//
// Peers are derived as "every other declared deployment that also
// appears in the global peers: map." This may overcount when an
// operator wants per-deployment peer subsets; refine when DeploymentSpec
// gains an explicit peers field. Until then, listing every cross-talk
// candidate is closer to right than listing none.
func topologyFromDeployYAML(dm *DeployManifest) []nexusmanifest.Deployment {
	if dm == nil || len(dm.Deployments) == 0 {
		return nil
	}
	declaredPeers := make(map[string]struct{}, len(dm.Peers))
	for name := range dm.Peers {
		declaredPeers[name] = struct{}{}
	}
	out := make([]nexusmanifest.Deployment, 0, len(dm.Deployments))
	for name, spec := range dm.Deployments {
		var peers []string
		for peerName := range declaredPeers {
			if peerName != name {
				peers = append(peers, peerName)
			}
		}
		// Inner slices are sorted so two emissions of the same yaml
		// produce identical bytes. The outer Deployments slice is
		// sorted below for the same reason.
		sort.Strings(peers)
		// OwnsList yields nil for monolith-shape specs (Owns omitted),
		// which projects to no `owns:` field on the manifest's
		// Deployment — matching the source YAML's intent. An explicit
		// empty list (web-svc shape) projects to an empty slice,
		// preserved through omitempty/omitzero on the manifest type.
		owns := append([]string(nil), spec.OwnsList()...)
		sort.Strings(owns)
		out = append(out, nexusmanifest.Deployment{
			Name:  name,
			Port:  spec.Port,
			Owns:  owns,
			Peers: peers,
		})
	}
	// Map iteration is randomized; sort by name so ManifestHash is
	// stable across repeated builds of the same source.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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
