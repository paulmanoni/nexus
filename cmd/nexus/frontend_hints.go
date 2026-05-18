package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/paulmanoni/nexus/frontend/deps/fetcher"
)

// suggestNexusAdd scans the supplied entry files for bare-spec
// imports and returns a sorted slice of suggested `nexus add`
// commands — one per package root. Used when `nexus dev` or
// `nexus build` detects islands.src/ entries but no nexus.lock,
// so the error message can tell the user exactly which
// dependencies they need to fetch rather than just hand-waving
// at "frontend dependencies".
//
//	import { ref } from 'vue';           → nexus add vue
//	import { VueFlow } from '@vue-flow/core';
//	                                     → nexus add @vue-flow/core
//	import './style.css';                → (no suggestion — relative)
//	import { x } from '@vue/runtime-dom/sub';
//	                                     → nexus add @vue/runtime-dom
//
// Best-effort: errors reading a file are logged and the file is
// skipped, since this is a hint generator — not a critical path.
// Returns nil when no bare specs are found (a project with only
// relative imports doesn't actually need nexus add, so we
// shouldn't fabricate suggestions).
func suggestNexusAdd(entries []string) []string {
	roots := map[string]struct{}{}
	for _, path := range entries {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, imp := range fetcher.ExtractImports(string(body)) {
			root := packageRootOf(imp)
			if root == "" {
				continue
			}
			roots[root] = struct{}{}
		}
	}
	if len(roots) == 0 {
		return nil
	}
	out := make([]string, 0, len(roots))
	for r := range roots {
		out = append(out, "nexus add "+r)
	}
	sort.Strings(out)
	return out
}

// packageRootOf returns the package name portion of a bare
// import specifier, dropping any sub-path. Returns "" for
// non-bare imports (relative / absolute / URL).
//
//	"vue"                         → "vue"
//	"vue/dist/vue.esm.js"         → "vue"
//	"@vue/runtime-dom"            → "@vue/runtime-dom"
//	"@vue/runtime-dom/foo.js"     → "@vue/runtime-dom"
//	"./local"                     → ""
//	"/abs"                        → ""
//	"https://example.com/x"       → ""
//
// Same convention as resolver.splitSpec — duplicated here so
// cmd/nexus doesn't pull frontend/deps/resolver into its
// import graph just for this helper.
func packageRootOf(spec string) string {
	if spec == "" {
		return ""
	}
	if spec[0] == '.' || spec[0] == '/' {
		return ""
	}
	if strings.Contains(spec, "://") || strings.HasPrefix(spec, "data:") {
		return ""
	}
	if spec[0] == '@' {
		parts := strings.SplitN(spec, "/", 3)
		if len(parts) < 2 {
			return spec // degenerate @scoped name with no path — pass through
		}
		return parts[0] + "/" + parts[1]
	}
	if i := strings.Index(spec, "/"); i >= 0 {
		return spec[:i]
	}
	return spec
}

// formatMissingLockfileError builds the user-facing message both
// nexus dev and nexus build emit when islands.src/ has entries
// but nexus.lock is absent. Centralized here so the wording
// stays consistent across the two surfaces.
func formatMissingLockfileError(srcDir string, entries []string) string {
	var sb strings.Builder
	sb.WriteString("nexus.lock missing — first-time setup needed for ")
	sb.WriteString(srcDir)
	sb.WriteString(".\n")
	suggestions := suggestNexusAdd(entries)
	if len(suggestions) > 0 {
		sb.WriteString("Run:\n")
		for _, s := range suggestions {
			sb.WriteString("  ")
			sb.WriteString(s)
			sb.WriteString("\n")
		}
	} else {
		sb.WriteString("Run `nexus add <pkg>` for any frontend dependency your code imports.\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// Compile-time check that fmt is imported (used by tests).
var _ = fmt.Sprintf
