package dts

import (
	"sort"
	"strings"
	"testing"
)

// fakeServer models esm.sh: a map of URL → (body, typesHeader). The bare
// package URL carries the X-TypeScript-Types header; .d.ts URLs carry "".
func fakeGetter(t *testing.T, files map[string]string, headers map[string]string) Getter {
	return func(url string) (string, string, error) {
		if h, ok := headers[url]; ok {
			return files[url], h, nil
		}
		body, ok := files[url]
		if !ok {
			t.Logf("fake 404: %s", url)
			return "", "", errNotFound
		}
		return body, "", nil
	}
}

var errNotFound = &dtsErr{"not found"}

type dtsErr struct{ s string }

func (e *dtsErr) Error() string { return e.s }

func TestEmit_RewritesCrossPackageHTTPSRefsToLocalRelative(t *testing.T) {
	// vue-router's .d.ts imports vue by absolute esm.sh URL; vue's imports
	// @vue/runtime-dom by absolute URL. After emit, every ref must be a
	// LOCAL relative path and the graph must be fully written.
	headers := map[string]string{
		"https://esm.sh/vue-router@4.4.5": "https://esm.sh/vue-router@4.4.5/dist/vue-router.d.ts",
		"https://esm.sh/vue@3.5.34":       "https://esm.sh/vue@3.5.34/dist/vue.d.mts",
	}
	files := map[string]string{
		"https://esm.sh/vue-router@4.4.5/dist/vue-router.d.ts": `import { App } from 'https://esm.sh/vue@3.5.34/dist/vue.d.mts';
export declare const x: App;`,
		"https://esm.sh/vue@3.5.34/dist/vue.d.mts":                     `export * from 'https://esm.sh/@vue/runtime-dom@3.5.34/dist/runtime-dom.d.ts';`,
		"https://esm.sh/@vue/runtime-dom@3.5.34/dist/runtime-dom.d.ts": `export declare class App {}`,
	}

	out := map[string]string{}
	write := func(rel, contents string) error { out[rel] = contents; return nil }

	pkgs := []Pkg{
		{Name: "vue-router", Version: "4.4.5", ProbeURL: "https://esm.sh/vue-router@4.4.5"},
		{Name: "vue", Version: "3.5.34", ProbeURL: "https://esm.sh/vue@3.5.34"},
	}
	res, err := Emit(pkgs, fakeGetter(t, files, headers), write)
	if err != nil {
		t.Fatal(err)
	}

	// NO https:// refs may survive in any emitted file.
	for rel, c := range out {
		if strings.Contains(c, "https://esm.sh/") {
			t.Errorf("emitted %s still has an https ref:\n%s", rel, c)
		}
	}
	// vue-router.d.ts must import vue via a relative path resolving to vue's
	// local file.
	vr := out["vue-router/dist/vue-router.d.ts"]
	// Rewritten to a local relative path WITHOUT the declaration extension
	// (TS resolves a bare module path to the .d.ts itself; importing a
	// literal .d.ts/.d.mts triggers TS2846).
	if !strings.Contains(vr, "../../vue/dist/vue") {
		t.Errorf("vue-router did not rewrite vue import to local relative path:\n%s", vr)
	}
	if strings.Contains(vr, "vue.d.mts") {
		t.Errorf("vue-router kept the .d.mts extension (would trigger TS2846):\n%s", vr)
	}
	// The transitive @vue/runtime-dom file must have been crawled + written.
	if _, ok := out["@vue/runtime-dom/dist/runtime-dom.d.ts"]; !ok {
		t.Errorf("transitive @vue/runtime-dom not emitted; got files: %v", keys(out))
	}
	// package.json with types entry per top-level package.
	if pj := out["vue/package.json"]; !strings.Contains(pj, `"types": "./dist/vue.d.mts"`) {
		t.Errorf("vue package.json missing/incorrect types entry:\n%s", pj)
	}
	if res.Packages != 2 {
		t.Errorf("Packages = %d, want 2", res.Packages)
	}
	if res.Files < 3 {
		t.Errorf("Files = %d, want >=3 (vue-router + vue + runtime-dom)", res.Files)
	}
}

func TestEmit_SkipsPackagesWithoutTypes(t *testing.T) {
	headers := map[string]string{} // no X-TypeScript-Types for anyone
	out := map[string]string{}
	res, err := Emit(
		[]Pkg{{Name: "no-types-pkg", Version: "1.0.0", ProbeURL: "https://esm.sh/no-types-pkg@1.0.0"}},
		fakeGetter(t, map[string]string{"https://esm.sh/no-types-pkg@1.0.0": "export const x=1"}, headers),
		func(rel, c string) error { out[rel] = c; return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if res.Packages != 0 || len(out) != 0 {
		t.Errorf("expected nothing emitted for a package with no types; got %d pkgs, %d files", res.Packages, len(out))
	}
	if len(res.Skipped) != 1 {
		t.Errorf("expected 1 skipped, got %v", res.Skipped)
	}
}

func TestRelPath(t *testing.T) {
	cases := []struct{ from, to, want string }{
		{"vue-router/dist", "vue/dist/vue.d.mts", "../../vue/dist/vue.d.mts"},
		{"vue/dist", "@vue/runtime-dom/dist/runtime-dom.d.ts", "../../@vue/runtime-dom/dist/runtime-dom.d.ts"},
		{"vue/dist", "vue/dist/shared.d.ts", "./shared.d.ts"},
		{".", "vue/index.d.ts", "./vue/index.d.ts"},
	}
	for _, c := range cases {
		got, err := relPath(c.from, c.to)
		if err != nil || got != c.want {
			t.Errorf("relPath(%q,%q) = %q,%v want %q", c.from, c.to, got, err, c.want)
		}
	}
}

func keys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
