package vitemanifest

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

const sample = `{
  "src/main.ts": {"file":"assets/main-DfUgamcr.js","src":"src/main.ts","isEntry":true,
                  "css":["assets/main-mxpuzdxW.css"],"assets":["assets/logo-B2xQ9fLk.svg"],
                  "imports":["_vendor-Cq1zZ2aA.js"]},
  "_vendor-Cq1zZ2aA.js": {"file":"assets/vendor-Cq1zZ2aA.js"},
  "src/unhashed.ts": {"file":"assets/unhashed.js","isEntry":true}
}`

func TestLoadPrefersDotVite(t *testing.T) {
	fsys := fstest.MapFS{
		".vite/manifest.json": {Data: []byte(sample)},
		"manifest.json":       {Data: []byte(`{}`)},
	}
	m, err := Load(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}
	if m.Path != ".vite/manifest.json" || len(m.Chunks) != 3 || len(m.Version) != 16 {
		t.Fatalf("got path=%q chunks=%d version=%q", m.Path, len(m.Chunks), m.Version)
	}
}

func TestLoadFallsBackToLegacyLocation(t *testing.T) {
	m, err := Load(fstest.MapFS{"dist/manifest.json": {Data: []byte(sample)}}, "dist")
	if err != nil || m.Path != "dist/manifest.json" {
		t.Fatalf("got (%v, %v)", m, err)
	}
}

func TestMissingIsNotExist(t *testing.T) {
	_, err := Load(fstest.MapFS{}, ".")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want fs.ErrNotExist so callers can tell no build from a broken one, got %v", err)
	}
}

func TestMalformedIsNotNotExist(t *testing.T) {
	_, err := Load(fstest.MapFS{".vite/manifest.json": {Data: []byte(`{`)}}, ".")
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a broken manifest must be its own error, got %v", err)
	}
}

func TestEntryKeyIsDeterministic(t *testing.T) {
	m, _ := Load(fstest.MapFS{".vite/manifest.json": {Data: []byte(sample)}}, ".")
	if got := m.EntryKey(); got != "src/main.ts" {
		t.Fatalf("EntryKey = %q", got)
	}
}

func TestImmutableNeedsBothManifestAndHash(t *testing.T) {
	m, _ := Load(fstest.MapFS{".vite/manifest.json": {Data: []byte(sample)}}, ".")
	got := m.Immutable()
	for _, want := range []string{
		"assets/main-DfUgamcr.js", "assets/main-mxpuzdxW.css",
		"assets/logo-B2xQ9fLk.svg", "assets/vendor-Cq1zZ2aA.js",
	} {
		if !got[want] {
			t.Errorf("%s should be immutable", want)
		}
	}
	// Built by Vite, but under a name with no hash: a new build reuses the
	// name, so caching it forever would serve stale code.
	if got["assets/unhashed.js"] {
		t.Error("an unhashed output must not be immutable")
	}
}

func TestIsHashedName(t *testing.T) {
	cases := map[string]bool{
		"assets/index-DfUgamcr.js": true,
		"index-DfUgamcr.css":       true,
		"assets/logo.png":          false,
		"favicon.ico":              false,
		"assets/my-logo.png":       false, // a dash is not a hash
		"assets/app-v2.js":         false,
	}
	for name, want := range cases {
		if got := IsHashedName(name); got != want {
			t.Errorf("IsHashedName(%q) = %v, want %v", name, got, want)
		}
	}
}
