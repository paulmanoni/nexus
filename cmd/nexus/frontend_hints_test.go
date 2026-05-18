package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPackageRootOf(t *testing.T) {
	cases := []struct{ in, want string }{
		{"vue", "vue"},
		{"vue/dist/vue.esm.js", "vue"},
		{"@vue/runtime-dom", "@vue/runtime-dom"},
		{"@vue/runtime-dom/foo.js", "@vue/runtime-dom"},
		{"lucide-vue-next", "lucide-vue-next"},
		{"./local", ""},
		{"../parent", ""},
		{"/abs", ""},
		{"https://example.com/x", ""},
		{"data:text/javascript,...", ""},
		{"", ""},
		{"@scoped-no-name", "@scoped-no-name"}, // degenerate
	}
	for _, tc := range cases {
		got := packageRootOf(tc.in)
		if got != tc.want {
			t.Errorf("packageRootOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSuggestNexusAdd(t *testing.T) {
	tmp := t.TempDir()
	main := filepath.Join(tmp, "main.ts")
	if err := os.WriteFile(main, []byte(
		`import { createApp } from 'vue';
import { VueFlow } from '@vue-flow/core';
import { Heart } from 'lucide-vue-next';
import App from './App.vue';
import './style.css';
console.log(createApp, VueFlow, Heart, App);
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := suggestNexusAdd([]string{main})
	want := []string{
		"nexus add @vue-flow/core",
		"nexus add lucide-vue-next",
		"nexus add vue",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSuggestNexusAdd_DedupAcrossFiles(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a.ts")
	b := filepath.Join(tmp, "b.ts")
	_ = os.WriteFile(a, []byte(`import 'vue';`), 0o644)
	_ = os.WriteFile(b, []byte(`import { ref } from 'vue';`), 0o644)
	got := suggestNexusAdd([]string{a, b})
	if !reflect.DeepEqual(got, []string{"nexus add vue"}) {
		t.Errorf("got %v, want [nexus add vue]", got)
	}
}

func TestSuggestNexusAdd_NoBareSpecs(t *testing.T) {
	tmp := t.TempDir()
	main := filepath.Join(tmp, "main.ts")
	_ = os.WriteFile(main, []byte(
		`import App from './App.vue';
import './style.css';
console.log(App);
`), 0o644)
	got := suggestNexusAdd([]string{main})
	if got != nil {
		t.Errorf("got %v, want nil (only relative imports)", got)
	}
}

func TestFormatMissingLockfileError_SpecificSuggestions(t *testing.T) {
	tmp := t.TempDir()
	main := filepath.Join(tmp, "main.ts")
	_ = os.WriteFile(main, []byte(`import { createApp } from 'vue';`), 0o644)
	msg := formatMissingLockfileError(tmp, []string{main})
	for _, want := range []string{
		"nexus.lock missing",
		"Run:",
		"nexus add vue",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("msg missing %q\n--- msg ---\n%s", want, msg)
		}
	}
}

func TestFormatMissingLockfileError_GenericFallback(t *testing.T) {
	// When no bare imports surface, fall back to a generic hint
	// rather than printing an empty "Run:" block.
	tmp := t.TempDir()
	main := filepath.Join(tmp, "main.ts")
	_ = os.WriteFile(main, []byte(`import App from './App.vue';`), 0o644)
	msg := formatMissingLockfileError(tmp, []string{main})
	if strings.Contains(msg, "Run:\n") {
		t.Errorf("expected generic fallback, got specific Run: block:\n%s", msg)
	}
	if !strings.Contains(msg, "any frontend dependency") {
		t.Errorf("missing generic fallback wording:\n%s", msg)
	}
}
