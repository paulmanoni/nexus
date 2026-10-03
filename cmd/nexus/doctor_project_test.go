package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The project checks catch a Go older than go.mod asks for, a module not
// on nexus v2 and a missing nexus.toml, each with what to do.
func TestProjectChecks(t *testing.T) {
	dir := t.TempDir()
	mod := "module example.com/shop\n\ngo 9.99\n\nrequire github.com/paulmanoni/nexus v1.78.2\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	got := map[string]doctorCheck{}
	for _, c := range projectChecks(dir) {
		got[c.Name] = c
	}
	if c := got["go"]; c.Level != checkFail || c.Fix == "" {
		t.Errorf("go = %+v, want a failure asking for 9.99", c)
	}
	if c := got["module"]; c.Level != checkWarn || c.Fix != "move to v2 with nexus migrate v2" {
		t.Errorf("module = %+v", c)
	}
	if c := got["nexus.toml"]; c.Level != checkWarn {
		t.Errorf("nexus.toml = %+v", c)
	}
	if _, ok := got["node"]; ok {
		t.Error("node checked for a project without a frontend")
	}
}
