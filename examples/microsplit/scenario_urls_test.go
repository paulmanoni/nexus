package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMicrosplit_ManifestURLsCodegen drives the manifest → codegen
// chain end-to-end. Writes a temp manifest with `urls:` next to the
// real one (so projectRoot resolution still finds microsplit's
// source), runs `nexus build` against it, and inspects the generated
// zz_deploy_gen.go to confirm the URLs slice was emitted with the
// right env-interpolation shape.
//
// Slow: `go run ../../cmd/nexus build` recompiles the CLI and runs
// packages.Load on microsplit's source. Skipped under -short so
// quick local runs don't pay the cost.
func TestMicrosplit_ManifestURLsCodegen(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess build is slow; run without -short to exercise the manifest → codegen chain")
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	// Custom manifest with `urls:` for users-svc — mixed literals and
	// ${VAR} entries to exercise both codegen branches in one pass.
	manifest := filepath.Join(cwd, "nexus.deploy.urls-test.yaml")
	body := `deployments:
  monolith:
    port: 8080
  checkout-svc:
    owns: [checkout]
    port: 8080
  users-svc:
    owns: [users]
    port: 8081

peers:
  users-svc:
    timeout: 2s
    urls:
      - http://users-1.cluster.local:8080
      - http://users-2.cluster.local:8080
      - ${USERS_SVC_REPLICA_3_URL}
  checkout-svc:
    timeout: 2s
`
	if err := os.WriteFile(manifest, []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(manifest) })

	// Run the build into a tmp dir so we don't clobber microsplit/bin/.
	binOut := filepath.Join(t.TempDir(), "checkout-svc")
	cliPath := filepath.Join("..", "..", "cmd", "nexus")
	cmd := exec.Command("go", "run", cliPath,
		"build",
		"--deployment", "checkout-svc",
		"--manifest", manifest,
		"-o", binOut,
	)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nexus build failed: %v\n%s", err, out)
	}

	// Read the generated init file. The shadow path lives under the
	// project root (microsplit/) regardless of -o.
	initFile := filepath.Join(cwd, ".nexus", "build", "checkout-svc", "zz_deploy_gen.go")
	t.Cleanup(func() {
		// The whole .nexus/build/checkout-svc dir is fair game to
		// clean — gitignored, regenerated on every build.
		_ = os.RemoveAll(filepath.Join(cwd, ".nexus", "build", "checkout-svc"))
	})

	src, err := os.ReadFile(initFile)
	if err != nil {
		t.Fatalf("read %s: %v", initFile, err)
	}
	body2 := string(src)

	mustContainURLs(t, body2, `URLs: []string{`)
	mustContainURLs(t, body2, `"http://users-1.cluster.local:8080"`)
	mustContainURLs(t, body2, `"http://users-2.cluster.local:8080"`)
	mustContainURLs(t, body2, `os.Getenv("USERS_SVC_REPLICA_3_URL")`)

	// Sanity: the singular URL emit path should NOT appear for
	// users-svc when URLs is set. Looking for "URL: " (with the
	// space delimits it from "URLs:" without a space).
	if usersBlock := extractPeerBlock(body2, "users-svc"); usersBlock != "" {
		if strings.Contains(usersBlock, "URL: ") {
			t.Errorf("users-svc block should not emit singular URL when URLs is set:\n%s", usersBlock)
		}
	}
}

// extractPeerBlock returns the substring of src corresponding to one
// peer's entry in the generated map. Used by the assertion above to
// scope the URL: vs URLs: check to the right peer.
func extractPeerBlock(src, peer string) string {
	marker := `"` + peer + `": {`
	start := strings.Index(src, marker)
	if start < 0 {
		return ""
	}
	rest := src[start:]
	end := strings.Index(rest, "},")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func mustContainURLs(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("expected generated init to contain %q; got:\n%s", want, body)
	}
}
