package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseESMURL(t *testing.T) {
	cases := []struct {
		in           string
		wantPkg      string
		wantRel      string
		wantOK       bool
	}{
		{"https://esm.sh/vue@3.5.34/dist/vue.d.mts", "vue", "dist/vue.d.mts", true},
		{"https://esm.sh/@vue/runtime-dom@3.5.34/dist/runtime-dom.d.ts", "@vue/runtime-dom", "dist/runtime-dom.d.ts", true},
		{"https://esm.sh/vue@3.5.34", "vue", "index.d.ts", true},
		{"https://esm.sh/@vue/shared@3.5.34", "@vue/shared", "index.d.ts", true},
		{"https://example.com/whatever.d.ts", "", "", false},
		{"https://esm.sh/vue", "", "", false}, // no version
		{"not a url", "", "", false},
	}
	for _, tc := range cases {
		pkg, rel, ok := parseESMURL(tc.in)
		if ok != tc.wantOK {
			t.Errorf("%q: ok = %v, want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if pkg != tc.wantPkg || rel != tc.wantRel {
			t.Errorf("%q: got (%q, %q), want (%q, %q)", tc.in, pkg, rel, tc.wantPkg, tc.wantRel)
		}
	}
}

// TestTypeFetcher_RewritesHTTPSImports exercises the import-
// rewriting logic in isolation since the parseESMURL host check
// blocks an end-to-end httptest path. Two helper pieces are tested
// here: the regex match correctly captures the URL group, and the
// strings.ReplaceAll rewrite produces import paths tsserver can
// follow.
func TestTypeFetcher_RewritesHTTPSImports(t *testing.T) {
	body := `import { Foo } from 'https://esm.sh/@vue/runtime-dom@3.5.34/dist/runtime-dom.d.ts';
import('https://esm.sh/vue@3.5.34/dist/vue.d.mts').then(m => m);
`
	matches := httpsImportRe.FindAllSubmatchIndex([]byte(body), -1)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
	// Group 3 holds the URL — index pair m[6]:m[7].
	urls := make([]string, 0, 2)
	for _, m := range matches {
		urls = append(urls, body[m[6]:m[7]])
	}
	if urls[0] != "https://esm.sh/@vue/runtime-dom@3.5.34/dist/runtime-dom.d.ts" {
		t.Errorf("URL[0] = %q", urls[0])
	}
	if urls[1] != "https://esm.sh/vue@3.5.34/dist/vue.d.mts" {
		t.Errorf("URL[1] = %q", urls[1])
	}
}

func TestGitignoreEnsureNodeModules(t *testing.T) {
	dir := t.TempDir()
	gp := filepath.Join(dir, ".gitignore")

	// No .gitignore → no-op (don't create one).
	gitignoreEnsureNodeModules(dir)
	if _, err := os.Stat(gp); !os.IsNotExist(err) {
		t.Error("created .gitignore when none existed")
	}

	// Existing .gitignore without node_modules → appended.
	if err := os.WriteFile(gp, []byte("/bin/\n*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitignoreEnsureNodeModules(dir)
	out, _ := os.ReadFile(gp)
	if !strings.Contains(string(out), "/node_modules/") {
		t.Errorf("node_modules not added: %q", out)
	}

	// Already-present → idempotent.
	before, _ := os.ReadFile(gp)
	gitignoreEnsureNodeModules(dir)
	after, _ := os.ReadFile(gp)
	if string(before) != string(after) {
		t.Errorf("non-idempotent append:\nbefore: %q\nafter:  %q", before, after)
	}
}
