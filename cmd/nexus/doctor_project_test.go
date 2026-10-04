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

func TestDoctorAuthTokenStore(t *testing.T) {
	dir := t.TempDir()
	write := func(src string) {
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package main\n\nimport \"github.com/paulmanoni/nexus/v2/extension/auth\"\n\nvar _ = auth.Module\n")
	if uses, store := authTokenStore(dir); !uses || store != "" {
		t.Fatalf("memory store: %v %q", uses, store)
	}
	write("package main\n\nimport (\n\t\"github.com/paulmanoni/nexus/v2/extension/auth\"\n\t\"github.com/paulmanoni/nexus/v2/extension/auth/authdb\"\n)\n\nvar _ = auth.Module\nvar _ = authdb.Bind[int]\n")
	if _, store := authTokenStore(dir); store != "authdb (SQL database)" {
		t.Fatalf("authdb: %q", store)
	}
	write("package main\n")
	if uses, _ := authTokenStore(dir); uses {
		t.Fatal("no auth, no check")
	}
}
