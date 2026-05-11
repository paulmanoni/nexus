package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	nexusmanifest "github.com/paulmanoni/nexus/manifest"
)

// writeManifestFile drops a Manifest as JSON into a temp dir and
// returns the path. Tests use this to drive the file-input path of
// nexus lint without coupling to fixture files.
func writeManifestFile(t *testing.T, m nexusmanifest.Manifest) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func cleanManifest() nexusmanifest.Manifest {
	return nexusmanifest.Manifest{
		Environments: []nexusmanifest.Environment{{Name: "production"}},
		Env: []nexusmanifest.EnvVar{
			{Name: "LOG_LEVEL", Default: "info"},
		},
	}
}

func TestLint_CleanManifest_File_ExitZeroWithSuccessLine(t *testing.T) {
	path := writeManifestFile(t, cleanManifest())
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	err := runLint(stdout, stderr, lintOptions{filePath: path})
	if err != nil {
		t.Fatalf("expected nil error, got %v (stderr=%q)", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "manifest is valid") {
		t.Errorf("expected success line, got:\n%s", stdout.String())
	}
}

func TestLint_BrokenManifest_File_ExitNonZeroWithReport(t *testing.T) {
	m := cleanManifest()
	m.Environments = append(m.Environments, nexusmanifest.Environment{Name: "production"}) // duplicate
	path := writeManifestFile(t, m)

	stdout, _ := new(bytes.Buffer), new(bytes.Buffer)
	err := runLint(stdout, new(bytes.Buffer), lintOptions{filePath: path})
	if !IsLintExitError(err) {
		t.Fatalf("expected lint-exit error, got %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "error") {
		t.Errorf("output missing 'error' marker:\n%s", out)
	}
	if !strings.Contains(out, "environments[1]") {
		t.Errorf("output missing duplicate-env path:\n%s", out)
	}
	if !strings.Contains(out, "manifest is invalid") {
		t.Errorf("output missing failure summary:\n%s", out)
	}
}

func TestLint_JSON_EmitsStructuredOutput(t *testing.T) {
	m := cleanManifest()
	m.Environments = append(m.Environments, nexusmanifest.Environment{Name: "production"})
	path := writeManifestFile(t, m)

	stdout := new(bytes.Buffer)
	err := runLint(stdout, new(bytes.Buffer), lintOptions{filePath: path, jsonOut: true})
	if !IsLintExitError(err) {
		t.Fatalf("expected lint-exit error (errors present), got %v", err)
	}

	var doc struct {
		Issues  []nexusmanifest.Issue
		Summary struct {
			Errors   int
			Warnings int
		}
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("parse json output: %v\n%s", err, stdout.String())
	}
	if doc.Summary.Errors == 0 {
		t.Errorf("expected at least one error in summary; got %+v", doc.Summary)
	}
	if len(doc.Issues) == 0 {
		t.Errorf("issues slice empty")
	}
}

func TestLint_Quiet_DropsWarnings(t *testing.T) {
	// Required env with no Default and no BoundTo = warning, not
	// error. Without --quiet it shows up; with --quiet it's filtered.
	m := nexusmanifest.Manifest{
		Environments: []nexusmanifest.Environment{{Name: "production"}},
		Env: []nexusmanifest.EnvVar{
			{Name: "API_KEY", Required: true},
		},
	}
	path := writeManifestFile(t, m)

	stdout := new(bytes.Buffer)
	err := runLint(stdout, new(bytes.Buffer), lintOptions{filePath: path})
	if err != nil {
		t.Fatalf("expected nil error (warnings only), got %v", err)
	}
	if !strings.Contains(stdout.String(), "warning") {
		t.Errorf("non-quiet run should print the warning:\n%s", stdout.String())
	}

	stdoutQ := new(bytes.Buffer)
	err = runLint(stdoutQ, new(bytes.Buffer), lintOptions{filePath: path, quiet: true})
	if err != nil {
		t.Fatalf("quiet run with no errors should exit 0, got %v", err)
	}
	// The warning's path / message must be gone. The summary line
	// may still mention warnings in its count, but no per-issue
	// warning block should appear. Check by looking for the
	// distinctive issue path that the warning emits.
	if strings.Contains(stdoutQ.String(), "env.API_KEY") {
		t.Errorf("--quiet should suppress the warning's issue block:\n%s", stdoutQ.String())
	}
	// With everything filtered out, we still want the success line.
	if !strings.Contains(stdoutQ.String(), "manifest is valid") {
		t.Errorf("quiet+clean should show success line:\n%s", stdoutQ.String())
	}
}

func TestLint_PathAndBinary_MutuallyExclusive(t *testing.T) {
	err := runLint(new(bytes.Buffer), new(bytes.Buffer), lintOptions{
		filePath:   "x.json",
		binaryPath: "./bin",
	})
	if err == nil {
		t.Fatal("expected error when both filePath and binaryPath are set")
	}
	if !strings.Contains(err.Error(), "cannot combine") {
		t.Errorf("error should mention the conflict, got %v", err)
	}
}

func TestLint_MalformedJSON_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runLint(new(bytes.Buffer), new(bytes.Buffer), lintOptions{filePath: path})
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse manifest JSON") {
		t.Errorf("error should explain parse failure, got %v", err)
	}
}

func TestLint_MissingFile_Rejected(t *testing.T) {
	err := runLint(new(bytes.Buffer), new(bytes.Buffer), lintOptions{
		filePath: "/nonexistent/manifest.json",
	})
	if err == nil {
		t.Fatal("expected read error")
	}
}

func TestLint_TextOutput_OrdersErrorsBeforeWarnings(t *testing.T) {
	// One warning + one error; ensure error appears first in the
	// human report.
	m := nexusmanifest.Manifest{
		Environments: []nexusmanifest.Environment{
			{Name: "production"},
			{Name: "production"}, // dup → error
		},
		Env: []nexusmanifest.EnvVar{
			{Name: "API_KEY", Required: true}, // warning
		},
	}
	path := writeManifestFile(t, m)

	stdout := new(bytes.Buffer)
	_ = runLint(stdout, new(bytes.Buffer), lintOptions{filePath: path})

	out := stdout.String()
	errIdx := strings.Index(out, "error")
	warnIdx := strings.Index(out, "warning")
	if errIdx < 0 || warnIdx < 0 {
		t.Fatalf("expected both error and warning in output:\n%s", out)
	}
	if errIdx > warnIdx {
		t.Errorf("error should appear before warning in report:\n%s", out)
	}
}

func TestLint_CobraCommand_WiredInRoot(t *testing.T) {
	// Sanity: the lint subcommand is registered on the root so
	// `nexus lint` resolves. Future refactors that drop it from the
	// root would break the CI integration path silently otherwise.
	root := newRootCmd(new(bytes.Buffer), new(bytes.Buffer))
	for _, c := range root.Commands() {
		if c.Name() == "lint" {
			return
		}
	}
	t.Fatal("lint subcommand not registered on root")
}
