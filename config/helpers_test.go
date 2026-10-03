package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTOML writes body to a temp nexus.toml and returns its path.
func writeTOML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nexus.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
