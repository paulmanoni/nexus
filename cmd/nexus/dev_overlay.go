package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// prepareDevOverlay generates a minimal go-build overlay for `nexus
// dev` so the framework's deploy-init (port, listeners, topology,
// auth, etc.) flows from nexus.deploy.yaml into the running binary.
// Without this `nexus dev` ran plain `go run .` and the manifest
// was ignored — the framework fell back to :8080 / single-listener
// defaults.
//
// Returns:
//   - overlayPath: the overlay.json to pass via `go run -overlay=...`.
//     Empty when no manifest exists in the project.
//   - deployment: the manifest deployment we used (typically
//     "monolith"). Empty when no manifest. Reserved for future use
//     in the dev banner.
//   - err: only when a manifest exists but is malformed. Empty
//     manifest = nil err + empty paths so the caller falls back to
//     plain `go run`.
//
// Cross-module dep registrations are intentionally skipped — they
// require a packages.Load on the project which adds 3-5s to every
// dev restart. The dashboard's "service depends on service" edges
// won't draw in dev mode without that scan; everything else (port,
// listeners, peer table) lands fine.
func prepareDevOverlay(target string) (overlayPath, deployment string, err error) {
	manifestPath := findManifest(target)
	if manifestPath == "" {
		return "", "", nil
	}
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return "", "", fmt.Errorf("load %s: %w", manifestPath, err)
	}
	deployment = pickDevDeployment(manifest)
	if deployment == "" {
		return "", "", fmt.Errorf("manifest declares no deployments")
	}

	projectRoot, err := filepath.Abs(filepath.Dir(manifestPath))
	if err != nil {
		return "", "", err
	}
	shadowDir := filepath.Join(projectRoot, ".nexus", "build", "_dev_"+deployment)
	if err := os.MkdirAll(shadowDir, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir shadow dir: %w", err)
	}

	// writeDeployInitFile codegens zz_deploy_gen.go with the
	// manifest's port + listeners + topology baked into a
	// SetDeploymentDefaults call. Empty mods = skip cross-module
	// dep registration (slow; not needed for dev port/listener
	// resolution).
	depFile, err := writeDeployInitFile(deployment, manifest, projectRoot, target, shadowDir, nil)
	if err != nil {
		return "", "", fmt.Errorf("codegen deploy-init: %w", err)
	}
	if depFile == "" {
		// No port + no peers + no listeners → nothing to overlay.
		return "", deployment, nil
	}

	mainPkgDir, err := devMainPackageDir(projectRoot, target)
	if err != nil {
		return "", "", fmt.Errorf("resolve main pkg dir: %w", err)
	}
	logicalPath := filepath.Join(mainPkgDir, "zz_deploy_gen.go")
	overlay := overlayJSON{Replace: map[string]string{logicalPath: depFile}}
	overlayPath = filepath.Join(shadowDir, "overlay.json")
	data, err := json.MarshalIndent(overlay, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("encode overlay: %w", err)
	}
	if err := os.WriteFile(overlayPath, data, 0o644); err != nil {
		return "", "", fmt.Errorf("write overlay: %w", err)
	}
	return overlayPath, deployment, nil
}

// pickDevDeployment chooses which deployment row in the manifest to
// codegen for `nexus dev` (single-process). Preference order:
//
//  1. "monolith" if it exists (the conventional name for a
//     deployment that owns every module).
//  2. Any deployment with empty Owns (the implicit-monolith case
//     even when its name isn't literally "monolith").
//  3. The lexically first deployment as a last resort.
//
// nexus dev --split goes through a different code path that picks
// per-deployment binaries; this helper is single-process only.
func pickDevDeployment(m *DeployManifest) string {
	if _, ok := m.Deployments["monolith"]; ok {
		return "monolith"
	}
	names := m.Names() // sorted lexically
	for _, n := range names {
		if len(m.Deployments[n].Owns) == 0 {
			return n
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// devMainPackageDir resolves the directory of the main package
// `target` relative to projectRoot. Cheap path: treat target as a
// filesystem path and absolutize. We deliberately avoid
// packages.Load here — it adds seconds of latency to every dev
// restart for one piece of metadata we can almost always derive
// from the path alone.
func devMainPackageDir(projectRoot, target string) (string, error) {
	if target == "" || target == "." {
		return projectRoot, nil
	}
	if filepath.IsAbs(target) {
		return target, nil
	}
	return filepath.Abs(filepath.Join(projectRoot, target))
}
