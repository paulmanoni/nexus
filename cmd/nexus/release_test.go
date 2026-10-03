package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls  []string
	status string
}

func (f *fakeRunner) run(dir string, env []string, name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	switch {
	case name == "git" && len(args) > 0 && args[0] == "status":
		return f.status, nil
	case name == "git" && len(args) > 0 && args[0] == "rev-parse" && args[1] == "-q":
		return "", os.ErrNotExist
	case name == "git" && len(args) > 0 && args[0] == "rev-parse":
		return "main\n", nil
	}
	return "", nil
}

func releaseRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/kit/v2\n\ngo 1.26\n")
	write("CHANGELOG.md", "## [2.1.0] - 2026-10-04\n")
	write("cmd/kit/go.mod", "module example.com/kit/cmd/kit/v2\n\ngo 1.26\n\nrequire example.com/kit/v2 v2.0.0\n")
	write("examples/shop/go.mod", "module example.com/kit/examples/shop/v2\n\ngo 1.26\n\nrequire example.com/kit/v2 v2.0.0\n")
	write("tools/go.mod", "module example.com/kit/tools/v2\n\ngo 1.26\n")
	return root
}

func TestReleasePlanAndRun(t *testing.T) {
	root := releaseRepo(t)
	var out bytes.Buffer
	r := &fakeRunner{}
	if err := runRelease(&out, &out, "v2.1.0", releaseOptions{dir: root, remote: "origin", cliPath: "example.com/kit/cmd/kit/v2"}, r); err != nil {
		t.Fatal(err)
	}
	plan := out.String()
	for _, want := range []string{"tag v2.1.0", "cmd/kit", "tag cmd/kit/v2.1.0", "examples/shop", "not tagged (example)", "nothing done"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "tools ") {
		t.Errorf("a module that doesn't require the root is in the plan:\n%s", plan)
	}

	r = &fakeRunner{}
	out.Reset()
	if err := runRelease(&out, &out, "v2.1.0", releaseOptions{dir: root, yes: true, remote: "origin", cliPath: "example.com/kit/cmd/kit/v2"}, r); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.calls, "\n")
	for _, want := range []string{
		"git tag v2.1.0", "git push origin v2.1.0",
		"go get example.com/kit/v2@v2.1.0", "go mod tidy", "go build ./...",
		"git commit -m chore(release): submodules require parent v2.1.0",
		"git tag cmd/kit/v2.1.0", "git push origin main cmd/kit/v2.1.0",
		"go install example.com/kit/cmd/kit/v2@v2.1.0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "git push origin v2.1.0"), strings.Index(got, "go get"); i > j {
		t.Error("submodules moved before the root tag was pushed")
	}
}

func TestReleaseRefusesWhatWouldBreak(t *testing.T) {
	root := releaseRepo(t)
	var out bytes.Buffer
	err := runRelease(&out, &out, "v2.2.0", releaseOptions{dir: root}, &fakeRunner{status: " M go.mod\n M go.work\n"})
	if err == nil || !strings.Contains(err.Error(), "no \"## [2.2.0]\"") || !strings.Contains(err.Error(), "uncommitted change: M go.mod") || strings.Contains(err.Error(), "go.work") {
		t.Fatalf("err = %v", err)
	}
	if err := runRelease(&out, &out, "v3.0.0", releaseOptions{dir: root}, &fakeRunner{}); err == nil || !strings.Contains(err.Error(), "v3 version") {
		t.Fatalf("major mismatch: %v", err)
	}
}
