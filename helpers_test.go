package nexus

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTOML writes body to a temp nexus.toml and returns its path. Shared
// across the config-loading tests (previously lived in database_toml_test.go,
// whose binders moved to package db).
func writeTOML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nexus.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
