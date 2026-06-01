package devserver

import (
	"strings"
	"testing"
)

func TestPkgKey(t *testing.T) {
	cases := map[string]string{
		"https://esm.sh/vue@3.5.34/es2022/vue.mjs":                         "vue@3.5.34",
		"https://esm.sh/@vue/runtime-core@3.5.34/es2022/runtime-core.mjs":  "@vue/runtime-core@3.5.34",
		"https://esm.sh/vuetify@3.11.7/X-ZX.../es2022/components.mjs":      "vuetify@3.11.7",
		"https://esm.sh/pinia@3.0.4?external=vue,react,react-dom":          "pinia@3.0.4", // query must not leak
		"https://esm.sh/@vueup/vue-quill@1.2.0?external=vue&target=es2022": "@vueup/vue-quill@1.2.0",
		"/src/main.ts": "", // not a package URL
		"":             "",
	}
	for url, want := range cases {
		if got := pkgKey(url); got != want {
			t.Errorf("pkgKey(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestPrebundleEligible(t *testing.T) {
	// The whole Vue family must be excluded (single-instance anchor);
	// everything else is eligible.
	excluded := []string{
		"https://esm.sh/vue@3.5.34/es2022/vue.development.mjs",
		"https://esm.sh/@vue/runtime-core@3.5.34/es2022/runtime-core.mjs",
		"https://esm.sh/@vue/reactivity@3.5.34/es2022/reactivity.mjs",
	}
	for _, u := range excluded {
		if prebundleEligible(u) {
			t.Errorf("prebundleEligible(%q) = true, want false (Vue family must stay per-module)", u)
		}
	}
	eligible := []string{
		"https://esm.sh/vuetify@3.11.7/es2022/vuetify.mjs",
		"https://esm.sh/pinia@3.0.4?external=vue,react,react-dom",
		"https://esm.sh/@apollo/client@3.14.0/core/index.mjs",
	}
	for _, u := range eligible {
		if !prebundleEligible(u) {
			t.Errorf("prebundleEligible(%q) = false, want true", u)
		}
	}
	// Non-package URLs are never eligible.
	if prebundleEligible("/src/main.ts") {
		t.Error("non-package URL should not be eligible")
	}
}

func TestToPrebundleURLSafe(t *testing.T) {
	h := New(Config{Root: t.TempDir(), Prebundle: true})
	// A query-bearing entry URL must produce a served path with NO query
	// chars (they'd split at the server and break the /@pre/ routing).
	entryURL := "https://esm.sh/pinia@3.0.4?external=vue,react-dom"
	sp := h.toPrebundle(entryURL)
	for _, bad := range []string{"?", "&", " "} {
		if containsStr(sp, bad) {
			t.Errorf("prebundle path %q contains %q — would break routing", sp, bad)
		}
	}
	// Shape: /@pre/<pkg@ver>/e-<hash>.js
	if !strings.HasPrefix(sp, PrebundlePrefix+"pinia@3.0.4/e-") || !strings.HasSuffix(sp, ".js") {
		t.Errorf("unexpected prebundle path shape: %q", sp)
	}
	// toPrebundle must register the entry under its package, so loadPrebundle
	// can split pkg/base back out and the package build covers it.
	pkg := "pinia@3.0.4"
	h.pre.mu.Lock()
	registered := h.pre.entries[pkg][entryURL]
	h.pre.mu.Unlock()
	if !registered {
		t.Errorf("toPrebundle did not register entry %q under %q", entryURL, pkg)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
