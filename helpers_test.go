package nexus

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmanoni/nexus/di"
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

// fxBootOptions returns the complete baseline di.Option chain —
// test-only sugar over the same fxEarlyOptions/fxLateOptions pair
// nexus.Run assembles (Run additionally interleaves user options,
// deferred options, and the stop timeout between them).
func fxBootOptions(cfg Config) di.Option {
	return di.Options(
		fxEarlyOptions(cfg),
		fxLateOptions(),
	)
}
