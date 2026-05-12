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

// writeDoctorYAML drops a YAML doc into a temp file at .yaml so
// auto-detection picks the YAML loader. Tests use this to drive
// the file-input path of the doctor CLI without coupling to Go
// struct → YAML marshaling.
func writeDoctorYAML(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.deploy.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestDoctor_CleanManifest_ExitZeroSuccessLine(t *testing.T) {
	path := writeDoctorYAML(t, `
environments:
  production: {}
env:
  LOG_LEVEL: { default: info }
`)
	stdout := new(bytes.Buffer)
	err := runDoctor(stdout, new(bytes.Buffer), doctorOptions{filePath: path})
	if err != nil {
		t.Fatalf("expected nil error, got %v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "no findings") {
		t.Errorf("expected success line:\n%s", stdout.String())
	}
}

func TestDoctor_BrokenManifest_ExitOneWithReport(t *testing.T) {
	path := writeDoctorYAML(t, `
environments:
  production: {}
environment_overrides:
  qa:
    env: {}
`)
	stdout := new(bytes.Buffer)
	err := runDoctor(stdout, new(bytes.Buffer), doctorOptions{filePath: path})
	if !IsLintExitError(err) {
		t.Fatalf("expected exit-error; got %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "overrides.qa") {
		t.Errorf("output missing qa path:\n%s", out)
	}
	if !strings.Contains(out, "needs attention") {
		t.Errorf("output missing failure summary:\n%s", out)
	}
}

func TestDoctor_JSON_EmitsStructuredOutput(t *testing.T) {
	path := writeDoctorYAML(t, `
environments:
  production: {}
environment_overrides:
  qa: {}
`)
	stdout := new(bytes.Buffer)
	err := runDoctor(stdout, new(bytes.Buffer), doctorOptions{filePath: path, jsonOut: true})
	if !IsLintExitError(err) {
		t.Fatalf("expected exit-error on errors present; got %v", err)
	}
	var doc struct {
		Findings []nexusmanifest.Finding
		Summary  struct {
			Errors   int
			Warnings int
		}
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("parse json: %v\n%s", err, stdout.String())
	}
	if doc.Summary.Errors == 0 {
		t.Errorf("expected errors > 0; got %+v", doc.Summary)
	}
}

func TestDoctor_Quiet_DropsWarnings(t *testing.T) {
	// Required env without default = warning, no error.
	path := writeDoctorYAML(t, `
environments:
  production: {}
env:
  API_KEY:
    required: true
`)
	stdout := new(bytes.Buffer)
	err := runDoctor(stdout, new(bytes.Buffer), doctorOptions{filePath: path, quiet: true})
	if err != nil {
		t.Fatalf("quiet + warnings-only should exit 0; got %v", err)
	}
	if strings.Contains(stdout.String(), "env.API_KEY") {
		t.Errorf("--quiet should drop the warning block:\n%s", stdout.String())
	}
}

func TestDoctor_PathAndBinary_MutuallyExclusive(t *testing.T) {
	err := runDoctor(new(bytes.Buffer), new(bytes.Buffer), doctorOptions{
		filePath:   "x.yaml",
		binaryPath: "./bin",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "cannot combine") {
		t.Errorf("error should mention conflict: %v", err)
	}
}

func TestDoctor_YamlBinaryCombo_Rejected(t *testing.T) {
	err := runDoctor(new(bytes.Buffer), new(bytes.Buffer), doctorOptions{
		inputFormat: "yaml",
		binaryPath:  "./bin",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error should mention yaml: %v", err)
	}
}

func TestDoctor_MissingFile_Rejected(t *testing.T) {
	err := runDoctor(new(bytes.Buffer), new(bytes.Buffer), doctorOptions{
		filePath: "/nope/missing.yaml",
	})
	if err == nil {
		t.Fatal("expected read error")
	}
}

func TestDoctor_CobraCommand_WiredInRoot(t *testing.T) {
	root := newRootCmd(new(bytes.Buffer), new(bytes.Buffer))
	for _, c := range root.Commands() {
		if c.Name() == "doctor" {
			return
		}
	}
	t.Fatal("doctor not registered on root")
}
