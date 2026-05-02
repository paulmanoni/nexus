package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenerateBake_RendersAllTargets verifies the bake file lists
// every deployment from the manifest as a target and groups them
// under "default". Substring assertions, not a byte-for-byte golden,
// so adding new comments to the template doesn't churn the test.
func TestGenerateBake_RendersAllTargets(t *testing.T) {
	_, manifestPath := fixtureManifest(t)
	var stdout, stderr bytes.Buffer
	err := runGenerateBake(bakeOptions{
		ManifestPath:   manifestPath,
		OutputPath:     "-",
		DockerfileName: "Dockerfile.<deployment>",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runGenerateBake: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	// Every manifest deployment must appear as a target block.
	mustContain(t, out, `target "monolith"`)
	mustContain(t, out, `target "users-svc"`)
	mustContain(t, out, `target "checkout-svc"`)
	// Default group enumerates the targets so `bake` with no args
	// builds everything.
	mustContain(t, out, `group "default"`)
	mustContain(t, out, `"monolith"`)
	// Variables expose registry + tag for CI override without editing.
	mustContain(t, out, `variable "REGISTRY"`)
	mustContain(t, out, `variable "TAG"`)
	// Dockerfile path uses the deployment-named pattern.
	mustContain(t, out, `dockerfile = "Dockerfile.monolith"`)
	mustContain(t, out, `dockerfile = "Dockerfile.users-svc"`)
}

// TestGenerateBake_TagPrefixDefaultsToManifestDir confirms the
// auto-derived prefix (basename of the manifest dir) is what lands
// in the rendered tag — saves the user from picking a name when the
// project layout already implies one.
func TestGenerateBake_TagPrefixDefaultsToManifestDir(t *testing.T) {
	dir, manifestPath := fixtureManifest(t)
	var stdout, stderr bytes.Buffer
	err := runGenerateBake(bakeOptions{
		ManifestPath:   manifestPath,
		OutputPath:     "-",
		DockerfileName: "Dockerfile.<deployment>",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runGenerateBake: %v", err)
	}

	prefix := filepath.Base(dir)
	mustContain(t, stdout.String(), `"${REGISTRY}`+prefix+`-monolith:${TAG}"`)
}

// TestGenerateBake_TagPrefixOverride verifies --tag-prefix wins
// over the auto-derived default — required for projects where the
// manifest dir name doesn't match the desired image namespace.
func TestGenerateBake_TagPrefixOverride(t *testing.T) {
	_, manifestPath := fixtureManifest(t)
	var stdout, stderr bytes.Buffer
	err := runGenerateBake(bakeOptions{
		ManifestPath:   manifestPath,
		OutputPath:     "-",
		TagPrefix:      "mycorp/microsplit",
		DockerfileName: "Dockerfile.<deployment>",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runGenerateBake: %v", err)
	}
	mustContain(t, stdout.String(), `"${REGISTRY}mycorp/microsplit-monolith:${TAG}"`)
}

// TestGenerateBake_ManifestInSubdir verifies the bake file's
// dockerfile paths are module-root-relative when the manifest sits in
// a subdir — the build context has to be the module root (where
// go.mod lives), so target paths must reach into the subdir.
func TestGenerateBake_ManifestInSubdir(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/test\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(root, "examples", "microsplit")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(subdir, "nexus.deploy.yaml")
	body := `deployments:
  monolith:
    port: 8080
  users-svc:
    owns: [users]
    port: 8081
`
	if err := os.WriteFile(manifestPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := runGenerateBake(bakeOptions{
		ManifestPath:   manifestPath,
		OutputPath:     "-",
		DockerfileName: "Dockerfile.<deployment>",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runGenerateBake: %v", err)
	}
	out := stdout.String()
	mustContain(t, out, `dockerfile = "examples/microsplit/Dockerfile.monolith"`)
	mustContain(t, out, `dockerfile = "examples/microsplit/Dockerfile.users-svc"`)
}

// TestGenerateBake_FileOutput writes to disk and confirms the
// default filename (docker-bake.hcl beside the manifest) — that's
// the name `docker buildx bake` auto-discovers, so users don't need
// to pass -f when running it from the manifest dir.
func TestGenerateBake_FileOutput(t *testing.T) {
	dir, manifestPath := fixtureManifest(t)
	var stdout, stderr bytes.Buffer
	err := runGenerateBake(bakeOptions{
		ManifestPath:   manifestPath,
		DockerfileName: "Dockerfile.<deployment>",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runGenerateBake: %v", err)
	}
	expected := filepath.Join(dir, "docker-bake.hcl")
	if _, err := os.Stat(expected); err != nil {
		t.Fatalf("expected output file at %s: %v", expected, err)
	}
	if !strings.Contains(stdout.String(), "docker-bake.hcl") {
		t.Errorf("stdout should announce the output path; got: %s", stdout.String())
	}
}

// TestGenerateBake_DockerfilePatternSubstitution lets users pick a
// non-default Dockerfile naming scheme (e.g. one Dockerfile per
// directory). The "<deployment>" token must be replaced everywhere
// it appears.
func TestGenerateBake_DockerfilePatternSubstitution(t *testing.T) {
	_, manifestPath := fixtureManifest(t)
	var stdout, stderr bytes.Buffer
	err := runGenerateBake(bakeOptions{
		ManifestPath:   manifestPath,
		OutputPath:     "-",
		DockerfileName: "build/<deployment>/Dockerfile",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runGenerateBake: %v", err)
	}
	out := stdout.String()
	mustContain(t, out, `dockerfile = "build/monolith/Dockerfile"`)
	mustContain(t, out, `dockerfile = "build/users-svc/Dockerfile"`)
}