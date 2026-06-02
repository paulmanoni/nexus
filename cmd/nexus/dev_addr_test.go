package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDevAddrFromConfig covers the addr resolution `nexus dev` does when
// --addr isn't passed: it must read [runtime.server].addr from
// nexus.toml so the vite proxy targets the port the app actually binds.
func TestDevAddrFromConfig(t *testing.T) {
	t.Run("reads runtime.server.addr", func(t *testing.T) {
		dir := t.TempDir()
		toml := `[runtime]
environment = "development"

[runtime.server]
addr = ":9590"

[runtime.dashboard]
enabled = true
`
		if err := os.WriteFile(filepath.Join(dir, "nexus.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := devAddrFromConfig(dir); got != ":9590" {
			t.Errorf("devAddrFromConfig = %q, want %q", got, ":9590")
		}
	})

	t.Run("empty when no nexus.toml", func(t *testing.T) {
		if got := devAddrFromConfig(t.TempDir()); got != "" {
			t.Errorf("devAddrFromConfig = %q, want empty", got)
		}
	})

	t.Run("empty when addr key absent", func(t *testing.T) {
		dir := t.TempDir()
		toml := "[runtime]\nenvironment = \"development\"\n"
		_ = os.WriteFile(filepath.Join(dir, "nexus.toml"), []byte(toml), 0o644)
		if got := devAddrFromConfig(dir); got != "" {
			t.Errorf("devAddrFromConfig = %q, want empty", got)
		}
	})

	t.Run("malformed toml falls back to empty", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "nexus.toml"), []byte("[runtime\n addr = "), 0o644)
		if got := devAddrFromConfig(dir); got != "" {
			t.Errorf("devAddrFromConfig = %q, want empty on parse error", got)
		}
	})

	t.Run("accepts a file target (uses its dir)", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "nexus.toml"),
			[]byte("[runtime.server]\naddr = \":7000\"\n"), 0o644)
		// Pass a file path that lives in dir; the helper should read
		// nexus.toml from the file's directory.
		fileTarget := filepath.Join(dir, "main.go")
		_ = os.WriteFile(fileTarget, []byte("package main\n"), 0o644)
		if got := devAddrFromConfig(fileTarget); got != ":7000" {
			t.Errorf("devAddrFromConfig(file) = %q, want %q", got, ":7000")
		}
	})
}
