package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stagedProject writes a minimal islands.src + lockfile + package.json
// layout into t.TempDir() and returns the project root. Used by every
// cache test to start from a known-good state.
func stagedProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "islands.src"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "islands.src", "main.ts"), `console.log("hi");`)
	mustWrite(t, filepath.Join(root, "nexus.lock"), `{"version":1,"deps":{}}`)
	mustWrite(t, filepath.Join(root, "package.json"), `{"name":"x","type":"module"}`)
	return root
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestFrontendBuildHash_Deterministic locks in that two calls
// against the same tree return the same digest. If this ever
// regresses, the cache becomes useless — every build looks new.
func TestFrontendBuildHash_Deterministic(t *testing.T) {
	root := stagedProject(t)
	a, err := frontendBuildHash(root)
	if err != nil {
		t.Fatalf("hash 1: %v", err)
	}
	b, err := frontendBuildHash(root)
	if err != nil {
		t.Fatalf("hash 2: %v", err)
	}
	if a != b {
		t.Fatalf("hash not deterministic: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Errorf("expected 64-char sha256 hex, got %d chars", len(a))
	}
}

// TestFrontendBuildHash_SourceChangeInvalidates is the headline
// case — editing an islands.src file MUST change the hash so the
// cache invalidates and the bundler reruns.
func TestFrontendBuildHash_SourceChangeInvalidates(t *testing.T) {
	root := stagedProject(t)
	before, _ := frontendBuildHash(root)
	mustWrite(t, filepath.Join(root, "islands.src", "main.ts"), `console.log("changed");`)
	after, _ := frontendBuildHash(root)
	if before == after {
		t.Fatal("hash unchanged after editing source file — cache would never invalidate")
	}
}

// TestFrontendBuildHash_NewSourceFileInvalidates covers the
// "added a new component" case: a file appearing under islands.src/
// must bust the cache even when no existing file changed.
func TestFrontendBuildHash_NewSourceFileInvalidates(t *testing.T) {
	root := stagedProject(t)
	before, _ := frontendBuildHash(root)
	mustWrite(t, filepath.Join(root, "islands.src", "Extra.ts"), `export const x = 1;`)
	after, _ := frontendBuildHash(root)
	if before == after {
		t.Fatal("hash unchanged after adding a new source file")
	}
}

// TestFrontendBuildHash_LockfileChangeInvalidates covers the
// "user ran nexus add" case: nexus.lock content changes must bust
// the cache so the new dep is bundled in.
func TestFrontendBuildHash_LockfileChangeInvalidates(t *testing.T) {
	root := stagedProject(t)
	before, _ := frontendBuildHash(root)
	mustWrite(t, filepath.Join(root, "nexus.lock"), `{"version":1,"deps":{"htm":"3.1.1"}}`)
	after, _ := frontendBuildHash(root)
	if before == after {
		t.Fatal("hash unchanged after lockfile change")
	}
}

// TestFrontendBuildHash_PackageJSONChangeInvalidates protects
// against the case where a user flips module type or adjusts a
// scripts entry — anything in package.json could affect bundling
// behavior, so the cache should bust.
func TestFrontendBuildHash_PackageJSONChangeInvalidates(t *testing.T) {
	root := stagedProject(t)
	before, _ := frontendBuildHash(root)
	mustWrite(t, filepath.Join(root, "package.json"), `{"name":"x","type":"commonjs"}`)
	after, _ := frontendBuildHash(root)
	if before == after {
		t.Fatal("hash unchanged after package.json change")
	}
}

// TestFrontendBuildHash_TSConfigChangeInvalidates makes sure the
// tsconfig content is part of the digest — flipping `target` or
// `strict` affects the emitted bundle.
func TestFrontendBuildHash_TSConfigChangeInvalidates(t *testing.T) {
	root := stagedProject(t)
	mustWrite(t, filepath.Join(root, "tsconfig.json"), `{"compilerOptions":{"target":"es2020"}}`)
	before, _ := frontendBuildHash(root)
	mustWrite(t, filepath.Join(root, "tsconfig.json"), `{"compilerOptions":{"target":"esnext"}}`)
	after, _ := frontendBuildHash(root)
	if before == after {
		t.Fatal("hash unchanged after tsconfig change")
	}
}

// TestFrontendBuildHash_EnvFileChangeInvalidates protects against
// stale define-replacements. loadViteEnv reads .env files; the
// cache MUST track them or a user could flip API_URL and see the
// old value baked into the bundle.
func TestFrontendBuildHash_EnvFileChangeInvalidates(t *testing.T) {
	root := stagedProject(t)
	mustWrite(t, filepath.Join(root, ".env"), `API_URL=http://localhost:8080`)
	before, _ := frontendBuildHash(root)
	mustWrite(t, filepath.Join(root, ".env"), `API_URL=https://prod.example.com`)
	after, _ := frontendBuildHash(root)
	if before == after {
		t.Fatal("hash unchanged after .env change")
	}
}

// TestFrontendBuildHash_OSGarbageIgnored ensures editor / OS
// noise files don't appear in the digest. Without this, a Finder
// browse on islands.src would drop a .DS_Store and bust the cache
// for no reason.
func TestFrontendBuildHash_OSGarbageIgnored(t *testing.T) {
	root := stagedProject(t)
	before, _ := frontendBuildHash(root)
	mustWrite(t, filepath.Join(root, "islands.src", ".DS_Store"), "garbage")
	mustWrite(t, filepath.Join(root, "islands.src", "main.ts~"), "vim backup")
	after, _ := frontendBuildHash(root)
	if before != after {
		t.Fatalf("hash changed after dropping editor/OS noise: %s vs %s", before, after)
	}
}

// TestReadWriteFrontendBuildHash exercises the persistence
// round-trip. Cache file lands at .nexus-cache/frontend-build.hash;
// reading it back returns the same string with no surrounding
// whitespace.
func TestReadWriteFrontendBuildHash(t *testing.T) {
	root := stagedProject(t)
	if got := readFrontendBuildHash(root); got != "" {
		t.Fatalf("expected empty initial cache, got %q", got)
	}
	const sample = "abc123"
	if err := writeFrontendBuildHash(root, sample); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readFrontendBuildHash(root)
	if got != sample {
		t.Fatalf("round-trip mismatch: want %q got %q", sample, got)
	}
	// Verify the on-disk shape matches what the constants advertise.
	path := filepath.Join(root, ".nexus-cache", "frontend-build.hash")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("hash file missing at %s: %v", path, err)
	}
}

// TestOutputDirHasFiles guards against the "stale hash + empty
// output" foot-gun. If islands/ is empty (user nuked it manually)
// or missing entirely, the cache MUST treat the build as stale.
func TestOutputDirHasFiles(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "islands")

	if outputDirHasFiles(out) {
		t.Error("missing dir should report no files")
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if outputDirHasFiles(out) {
		t.Error("empty dir should report no files")
	}

	mustWrite(t, filepath.Join(out, "main.js"), "console.log(1)")
	if !outputDirHasFiles(out) {
		t.Error("dir with one file should report present")
	}
}

// TestFrontendCacheDisabled covers the env-var kill switch. CI
// systems set NEXUS_FRONTEND_NO_CACHE=1 to force a clean build
// every time; debug sessions use it to confirm the cache isn't
// masking a bug.
func TestFrontendCacheDisabled(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"off", false},
		{"1", true},
		{"true", true},
		{"yes", true},
		{"TRUE", true},     // case-insensitive
		{" 1 ", true},      // trimmed
		{"anything", true}, // unknown values count as enabled
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			t.Setenv(envSkipFrontendCache, c.val)
			if got := frontendCacheDisabled(); got != c.want {
				t.Errorf("val=%q want=%v got=%v", c.val, c.want, got)
			}
		})
	}
}

// TestFrontendBuildHash_MissingProjectIsAbsent confirms a totally
// empty project still hashes successfully (returning a stable
// "everything absent" digest). Without this guard, the first build
// for a brand-new scaffold would error out.
func TestFrontendBuildHash_MissingProjectIsAbsent(t *testing.T) {
	root := t.TempDir()
	hash, err := frontendBuildHash(root)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(hash, "") || len(hash) != 64 {
		t.Errorf("expected valid sha256 even for empty project, got %q", hash)
	}
}
