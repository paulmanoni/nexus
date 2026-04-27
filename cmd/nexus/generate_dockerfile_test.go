package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureManifest writes a minimal nexus.deploy.yaml plus a stub
// go.mod into a temp dir and returns the path. The go.mod stub is
// required because the generator walks up looking for module-root —
// real users always have one, but the test-only TempDir wouldn't
// without an explicit write.
func fixtureManifest(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte(""), 0o644); err != nil {
		t.Fatalf("write go.sum: %v", err)
	}
	path = filepath.Join(dir, "nexus.deploy.yaml")
	body := `deployments:
  monolith:
    port: 8080
  users-svc:
    owns: [users]
    port: 8081
  checkout-svc:
    owns: [checkout]
    port: 8080

peers:
  users-svc:
    timeout: 2s
  checkout-svc:
    timeout: 2s
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir, path
}

// TestGenerateDockerfile_RendersExpectedDirectives is a content check
// rather than a byte-for-byte golden compare — the Dockerfile gains
// new comments / instructions over time and the test should keep
// passing as long as the load-bearing pieces are right.
func TestGenerateDockerfile_RendersExpectedDirectives(t *testing.T) {
	_, manifestPath := fixtureManifest(t)
	var stdout, stderr bytes.Buffer
	err := runGenerateDockerfile(dockerfileOptions{
		Deployment:      "users-svc",
		ManifestPath:    manifestPath,
		OutputPath:      "-",
		GoVersion:       "1.25",
		RuntimeImage:    "alpine:3.20",
		AdminPortOffset: 1000,
		NexusVersion:    "latest",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runGenerateDockerfile: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	mustContain(t, out, "FROM golang:1.25-alpine AS builder")
	mustContain(t, out, "FROM alpine:3.20 AS runtime")
	mustContain(t, out, "go install github.com/paulmanoni/nexus/cmd/nexus@latest")
	mustContain(t, out, "RUN nexus build --deployment users-svc -o /out/users-svc")
	mustContain(t, out, "ENV NEXUS_DEPLOYMENT=users-svc")
	// Admin offset 1000 → 8081 + 1000 = 9081.
	mustContain(t, out, "EXPOSE 8081 9081")
	// Healthcheck must target the admin port because public scope
	// hides /__nexus/*.
	mustContain(t, out, "http://localhost:9081/__nexus/health")
	mustContain(t, out, `ENTRYPOINT ["/app/users-svc"]`)
}

// TestGenerateDockerfile_NoAdminPort verifies the single-listener
// fallback: with admin offset 0, EXPOSE has only the public port and
// the healthcheck hits it directly.
func TestGenerateDockerfile_NoAdminPort(t *testing.T) {
	_, manifestPath := fixtureManifest(t)
	var stdout, stderr bytes.Buffer
	err := runGenerateDockerfile(dockerfileOptions{
		Deployment:      "monolith",
		ManifestPath:    manifestPath,
		OutputPath:      "-",
		GoVersion:       "1.25",
		RuntimeImage:    "alpine:3.20",
		AdminPortOffset: 0,
		NexusVersion:    "latest",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runGenerateDockerfile: %v", err)
	}
	out := stdout.String()
	if strings.Contains(out, "9080") {
		t.Errorf("admin port should not appear when offset is 0; got:\n%s", out)
	}
	mustContain(t, out, "EXPOSE 8080")
	mustContain(t, out, "http://localhost:8080/__nexus/health")
}

// TestGenerateDockerfile_UnknownDeployment rejects a deployment name
// that isn't in the manifest so the user sees the typo before docker
// build runs.
func TestGenerateDockerfile_UnknownDeployment(t *testing.T) {
	_, manifestPath := fixtureManifest(t)
	var stdout, stderr bytes.Buffer
	err := runGenerateDockerfile(dockerfileOptions{
		Deployment:   "users-srv", // typo
		ManifestPath: manifestPath,
		OutputPath:   "-",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for unknown deployment")
	}
	if !strings.Contains(err.Error(), "users-srv") {
		t.Errorf("error should name the bad deployment; got: %v", err)
	}
}

// TestGenerateDockerfile_FileOutput writes to disk and verifies the
// default Dockerfile.<deployment> filename + that the success message
// names the path so users can pipe it through scripts.
func TestGenerateDockerfile_FileOutput(t *testing.T) {
	dir, manifestPath := fixtureManifest(t)
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)

	var stdout, stderr bytes.Buffer
	err := runGenerateDockerfile(dockerfileOptions{
		Deployment:      "users-svc",
		ManifestPath:    manifestPath,
		AdminPortOffset: 1000,
		GoVersion:       "1.25",
		RuntimeImage:    "alpine:3.20",
		NexusVersion:    "latest",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runGenerateDockerfile: %v", err)
	}
	expected := filepath.Join(dir, "Dockerfile.users-svc")
	if _, err := os.Stat(expected); err != nil {
		t.Fatalf("expected output file at %s: %v", expected, err)
	}
	if !strings.Contains(stdout.String(), "Dockerfile.users-svc") {
		t.Errorf("stdout should announce the output path; got: %s", stdout.String())
	}
}

// TestGenerateDockerfile_ManifestInSubdir verifies the generator
// handles the microsplit case: nexus.deploy.yaml lives in a subdir
// of the Go module (e.g. examples/microsplit), so the build context
// must be the module root and the Dockerfile needs WORKDIR set into
// the subdir before calling nexus build.
func TestGenerateDockerfile_ManifestInSubdir(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/test\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.sum"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(root, "examples", "microsplit")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(subdir, "nexus.deploy.yaml")
	if err := os.WriteFile(manifestPath, []byte("deployments:\n  monolith:\n    port: 8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := runGenerateDockerfile(dockerfileOptions{
		Deployment:      "monolith",
		ManifestPath:    manifestPath,
		OutputPath:      "-",
		GoVersion:       "1.25",
		RuntimeImage:    "alpine:3.20",
		AdminPortOffset: 1000,
		NexusVersion:    "latest",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runGenerateDockerfile: %v", err)
	}

	out := stdout.String()
	// Build hint must include the subdir-relative -f path.
	mustContain(t, out, "docker build -f examples/microsplit/Dockerfile.monolith .")
	// WORKDIR switches into the subdir before nexus build runs.
	mustContain(t, out, "WORKDIR /src/examples/microsplit")
	// nexus build still runs from there with the same deployment name.
	mustContain(t, out, "RUN nexus build --deployment monolith -o /out/monolith")
}

// TestGenerateDockerfile_NoGoModRefuses produces a clear error when
// the manifest sits outside any Go module — the nexus build inside
// the container would fail later anyway, but failing here is a
// faster signal.
func TestGenerateDockerfile_NoGoModRefuses(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "nexus.deploy.yaml")
	if err := os.WriteFile(manifestPath, []byte("deployments:\n  monolith:\n    port: 8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := runGenerateDockerfile(dockerfileOptions{
		Deployment:   "monolith",
		ManifestPath: manifestPath,
		OutputPath:   "-",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when no go.mod is reachable")
	}
	if !strings.Contains(err.Error(), "go.mod") {
		t.Errorf("error should mention go.mod; got: %v", err)
	}
}

func mustContain(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("expected output to contain %q; got:\n%s", want, body)
	}
}
