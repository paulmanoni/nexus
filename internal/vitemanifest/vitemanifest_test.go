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
		// Real Vite 6.4.3 output, including hashes that contain '-' and
		// start with a digit.
		"assets/index-3Ky9ilzn.js":             true,
		"assets/index-DxvvOqhM.css":            true,
		"assets/admin-dashboard-DogMas93.js":   true,
		"assets/admin-dashboard-C70Wr14K.css":  true,
		"assets/inter-variable-DXv-ZbW9.woff2": true,
		"assets/index-9fOxoUu9.css":            true,
		"assets/index-aR4veslo.js":             true,
		"index-DfUgamcr.css":                   true,
		"assets/vendor-Cq1zZ2aA.js":            true,
		"assets/x-_a-b_c1d.js":                 true,

		"assets/logo.png":    false,
		"favicon.ico":        false,
		"assets/my-logo.png": false, // a dash is not a hash
		"assets/app-v2.js":   false,
		// Kebab-case names: '-' is in the hash alphabet, so a plain
		// "dash then 8+ hash characters" test takes these for hashes.
		"assets/admin-dashboard.js":      false,
		"assets/admin-dashboard.css":     false,
		"assets/inter-variable.woff2":    false,
		"assets/my-component-name.js":    false,
		"assets/font-awesome.woff2":      false,
		"assets/company-logo.svg":        false,
		"assets/jquery-3.7.1.min.js":     false,
		"assets/bootstrap-5.3.3.min.css": false,
		// Exactly eight after the dash, but all lowercase: a word, or a
		// rare all-lowercase hash that is merely revalidated.
		"assets/theme-overview.css": false,
		"assets/index-abcdefgh.js":  false,
		// Longer than Vite's eight: not the default shape.
		"assets/index-DfUgamcrAB.js": false,
		// No extension, or nothing before the dash.
		"assets/index-DfUgamcr": false,
		"DfUgamcr.js":           false,
	}
	for name, want := range cases {
		if got := IsHashedName(name); got != want {
			t.Errorf("IsHashedName(%q) = %v, want %v", name, got, want)
		}
	}
}

// realHashed and realUnhashed are the manifests Vite 6.4.3 wrote for the same
// app: default names, and entryFileNames/chunkFileNames: "assets/[name].js",
// assetFileNames: "assets/[name].[ext]".
const realHashed = `{
  "index.html": {"file": "assets/index-3Ky9ilzn.js", "name": "index", "src": "index.html", "isEntry": true,
    "dynamicImports": ["src/components/admin-dashboard.vue"],
    "css": ["assets/index-DxvvOqhM.css"], "assets": ["assets/inter-variable-DXv-ZbW9.woff2"]},
  "src/components/admin-dashboard.vue": {"file": "assets/admin-dashboard-DogMas93.js", "name": "admin-dashboard",
    "src": "src/components/admin-dashboard.vue", "isDynamicEntry": true, "imports": ["index.html"],
    "css": ["assets/admin-dashboard-C70Wr14K.css"]},
  "src/fonts/inter-variable.woff2": {"file": "assets/inter-variable-DXv-ZbW9.woff2", "src": "src/fonts/inter-variable.woff2"}
}`

const realUnhashed = `{
  "index.html": {"file": "assets/index.js", "name": "index", "src": "index.html", "isEntry": true,
    "dynamicImports": ["src/components/admin-dashboard.vue"],
    "css": ["assets/index.css"], "assets": ["assets/inter-variable.woff2"]},
  "src/components/admin-dashboard.vue": {"file": "assets/admin-dashboard.js", "name": "admin-dashboard",
    "src": "src/components/admin-dashboard.vue", "isDynamicEntry": true, "imports": ["index.html"],
    "css": ["assets/admin-dashboard.css"]},
  "src/fonts/inter-variable.woff2": {"file": "assets/inter-variable.woff2", "src": "src/fonts/inter-variable.woff2"}
}`

func load(t *testing.T, raw string) *Manifest {
	t.Helper()
	m, err := Load(fstest.MapFS{".vite/manifest.json": {Data: []byte(raw)}}, ".")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestImmutableRealViteBuilds(t *testing.T) {
	got := load(t, realHashed).Immutable()
	want := []string{
		"assets/index-3Ky9ilzn.js", "assets/index-DxvvOqhM.css",
		"assets/admin-dashboard-DogMas93.js", "assets/admin-dashboard-C70Wr14K.css",
		"assets/inter-variable-DXv-ZbW9.woff2",
	}
	for _, p := range want {
		if !got[p] {
			t.Errorf("%s is hashed build output and should be immutable", p)
		}
	}
	if len(got) != len(want) {
		t.Errorf("immutable = %v, want exactly %v", got, want)
	}
	if got := load(t, realUnhashed).Immutable(); len(got) != 0 {
		t.Errorf("a build with [name] output names has nothing to cache forever, got %v", got)
	}
}

// The reviewer's case: hash-free names in kebab case, listed as build output.
func TestUnhashedKebabNamesAreNotImmutable(t *testing.T) {
	m := &Manifest{Chunks: map[string]Chunk{
		"src/admin-dashboard.ts": {File: "assets/admin-dashboard.js", IsEntry: true,
			CSS:    []string{"assets/admin-dashboard.css"},
			Assets: []string{"assets/font-awesome.woff2", "assets/inter-variable.woff2", "assets/company-logo.svg"}},
		"index.html": {File: "assets/my-component-name.js", IsEntry: true},
	}}
	imm := m.Immutable()
	for _, p := range []string{"assets/admin-dashboard.js", "assets/admin-dashboard.css", "assets/font-awesome.woff2",
		"assets/inter-variable.woff2", "assets/company-logo.svg", "assets/my-component-name.js"} {
		if imm[p] {
			t.Errorf("%s has no content hash but is marked immutable (cached for a year)", p)
		}
	}
}

// The manifest's own names settle what the shape alone can't.
func TestImmutableUsesTheRecordsNames(t *testing.T) {
	m := &Manifest{Chunks: map[string]Chunk{
		// Unhashed, though "UserCard" looks like a hash: the chunk name
		// is the whole stem.
		"src/nav-UserCard.vue": {File: "assets/nav-UserCard.js", Name: "nav-UserCard", Src: "src/nav-UserCard.vue"},
		// An asset whose source name is the whole stem, also listed by an
		// entry — the record's verdict wins over the shape.
		"src/icon-Arrow123.svg": {File: "assets/icon-Arrow123.svg", Src: "src/icon-Arrow123.svg"},
		"src/main.ts": {File: "assets/main-B1x2Qa9k.js", Name: "main", IsEntry: true,
			Assets: []string{"assets/icon-Arrow123.svg"}},
		// [hash:12]: longer than the default, recognised through the name.
		"src/long.ts": {File: "assets/long-DfUgamcrAB12.js", Name: "long"},
		// Vite 6 asset names.
		"src/a/logo.svg": {File: "assets/logo-B2xQ9fLk.svg", Names: []string{"logo.svg"}},
	}}
	imm := m.Immutable()
	for p, want := range map[string]bool{
		"assets/nav-UserCard.js":      false,
		"assets/icon-Arrow123.svg":    false,
		"assets/main-B1x2Qa9k.js":     true,
		"assets/long-DfUgamcrAB12.js": true,
		"assets/logo-B2xQ9fLk.svg":    true,
	} {
		if imm[p] != want {
			t.Errorf("%s: immutable = %v, want %v", p, imm[p], want)
		}
	}
}
