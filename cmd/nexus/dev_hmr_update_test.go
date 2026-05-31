package main

import (
	"strings"
	"testing"
)

func TestScanVueImports(t *testing.T) {
	cases := []struct {
		name string
		code string
		want []string
	}{
		{
			"aliased named imports (SFC compiler shape)",
			`import { openBlock as _openBlock, createElementBlock as _ceb } from "vue"`,
			[]string{"openBlock", "createElementBlock"},
		},
		{
			"plain named import",
			`import { defineComponent } from "vue"`,
			[]string{"defineComponent"},
		},
		{
			"single quotes",
			`import { ref } from 'vue'`,
			[]string{"ref"},
		},
		{
			"no vue import",
			`import { foo } from "./bar"`,
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scanVueImports(tc.code)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("scanVueImports(%q) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

func TestBuildVueUpdateModule_ExternalizesVue(t *testing.T) {
	// A minimal compiled-SFC shape: imports a vnode helper from vue and
	// exports a default component. The built update module must NOT bundle
	// vue — it must read from globalThis.__nexus_vue__.
	sfc := `import { createElementBlock as _ceb, openBlock as _ob } from "vue";
const __sfc__ = { render() { return _ob(), _ceb("div") } };
export default __sfc__;
`
	out, err := buildVueUpdateModule(sfc, "/proj/src/Foo.vue")
	if err != nil {
		t.Fatalf("buildVueUpdateModule: %v", err)
	}
	// The output must reference the global bridge, not a bundled vue.
	if !strings.Contains(out, "__nexus_vue__") {
		t.Errorf("update module does not bind to globalThis.__nexus_vue__:\n%s", out)
	}
	// It must still export a default (the component).
	if !strings.Contains(out, "export") || !strings.Contains(out, "default") {
		t.Errorf("update module missing default export:\n%s", out)
	}
	// It must not contain a fresh Vue implementation (no createApp def).
	if strings.Contains(out, "function createApp") {
		t.Errorf("update module appears to bundle Vue itself:\n%s", out)
	}
}

func TestUpdateModuleCache_PutGet(t *testing.T) {
	c := newUpdateModuleCache()
	if _, ok := c.get("/x"); ok {
		t.Error("empty cache returned a hit")
	}
	c.put("/x", "export default 1")
	got, ok := c.get("/x")
	if !ok || got != "export default 1" {
		t.Errorf("get after put = %q,%v", got, ok)
	}
	// Overwrite (new generation).
	c.put("/x", "export default 2")
	if got, _ := c.get("/x"); got != "export default 2" {
		t.Errorf("overwrite failed: %q", got)
	}
}
