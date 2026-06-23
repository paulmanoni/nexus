package main

import (
	"path/filepath"
	"testing"
)

func TestInertiaConfigOverride(t *testing.T) {
	cases := []struct {
		name      string
		toml      string
		wantValue bool
		wantSet   bool
	}{
		{"no file", "", false, false},
		{"key unset", "[runtime.inertia]\n", false, false},
		{"enabled true", "[runtime.inertia]\nenabled = true\n", true, true},
		{"enabled false", "[runtime.inertia]\nenabled = false\n", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.toml != "" {
				writeFile(t, filepath.Join(dir, "nexus.toml"), c.toml)
			}
			value, set := inertiaConfigOverride(dir)
			if value != c.wantValue || set != c.wantSet {
				t.Errorf("got (value=%v set=%v), want (value=%v set=%v)", value, set, c.wantValue, c.wantSet)
			}
		})
	}
}

func TestAppImportsPackage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/m\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "foo", "foo.go"), "package foo\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nimport _ \"example.com/m/foo\"\n\nfunc main() {}\n")

	if !appImportsPackage(dir, "example.com/m/foo") {
		t.Error("expected imported package to be detected")
	}
	if appImportsPackage(dir, "example.com/m/bar") {
		t.Error("did not expect a non-imported package to be detected")
	}
}

func TestDevInertiaEnabledResolution(t *testing.T) {
	// Explicit override wins over auto-detection (both directions).
	t.Run("force on", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "nexus.toml"), "[runtime.inertia]\nenabled = true\n")
		if !devInertiaEnabled(dir) {
			t.Error("enabled=true should force Inertia mode on")
		}
	})
	t.Run("force off", func(t *testing.T) {
		dir := t.TempDir()
		// An app that DOES import inertia, but explicitly opts out.
		writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/m\n\ngo 1.21\n")
		writeFile(t, filepath.Join(dir, "nexus.toml"), "[runtime.inertia]\nenabled = false\n")
		if devInertiaEnabled(dir) {
			t.Error("enabled=false should force Inertia mode off")
		}
	})
	t.Run("auto off when not imported", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/m\n\ngo 1.21\n")
		writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
		if devInertiaEnabled(dir) {
			t.Error("no inertia import and no override should resolve to off")
		}
	})
}
