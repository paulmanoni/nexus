package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Dump writes the embedded SDK runtime + the live manifest +
// generated .d.ts to outDir. Optionally merges path mappings into
// tsconfig (or jsconfig — same shape) at the given path so an IDE
// resolves the runtime URL imports back to the dumped files.
//
// Idempotent: WriteIfChanged compares bytes before writing, so
// re-running against an already-up-to-date target preserves the
// existing files' mtime — no file-watch / IDE-reindex churn on
// no-op runs.
//
// outDir is created if missing. tsconfig is optional; pass "" to
// skip the IDE-config step. Errors short-circuit; partial writes
// (some files written, then a failure) are possible but rare —
// the helper writes small files in a fixed order and the
// filesystem operations themselves rarely fail mid-batch.
//
// stdout receives one line per file actually written. Files already
// current produce no output — this runs on every boot, and a restart that
// changed nothing has nothing to report. Pass io.Discard to silence entirely.
func (h *Handler) Dump(outDir, tsconfig, viteConfig string, stdout io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("nexus client: mkdir %s: %w", outDir, err)
	}

	// Force a manifest + .d.ts build if it hasn't happened yet.
	// The HTTP route is lazy on first request; for the dump path we
	// need them synchronously now.
	h.build()
	h.mu.Lock()
	clientDTS := append([]byte(nil), h.dtsClient.body...)
	vueDTS := append([]byte(nil), h.dtsVue.body...)
	inertiaDTS := append([]byte(nil), h.dtsInertia.body...)
	h.mu.Unlock()

	// Dump always writes the FULL manifest to disk — the cached
	// h.manifest bytes hold the public projection (skinny when
	// cfg.Public is false), but build-time TS / dev tooling needs
	// the complete shape. Marshalling is cheap enough to do here
	// without an extra cache slot.
	manifest, err := json.MarshalIndent(h.Manifest(), "", "  ")
	if err != nil {
		return fmt.Errorf("nexus client: marshal full manifest: %w", err)
	}

	// Static + generated files. Each .js sits next to its .d.ts so
	// the TypeScript compiler auto-pairs them whether the consumer
	// imports via the runtime URL or as a plain relative path.
	files := []struct {
		name string
		body []byte
	}{
		{"client.js", clientJS},
		{"client.d.ts", clientDTS},
		{"vue.js", vueJS},
		{"vue.d.ts", vueDTS},
		{"manifest.json", manifest},
		{"nexus-vite-plugin.js", vitePluginJS},
		{"nexus-vite-plugin.d.ts", vitePluginDTS},
	}
	for _, f := range files {
		if err := WriteIfChanged(filepath.Join(outDir, f.name), f.body, stdout); err != nil {
			return err
		}
	}

	if err := WriteInertiaDTS(filepath.Join(outDir, "inertia.d.ts"), inertiaDTS, stdout); err != nil {
		return err
	}

	// nexus.ts is a one-time wiring scaffold (singleton client +
	// composable re-exports + type re-exports). Written ONLY when
	// missing so subsequent boots don't clobber developer edits to
	// the singleton's construction (custom origin logic, alternate
	// token stores, extra exports). Delete the file to regenerate.
	nexusTS := []byte(GenerateNexusTS(h.Manifest()))
	if err := WriteIfMissing(filepath.Join(outDir, "nexus.ts"), nexusTS, stdout); err != nil {
		return err
	}

	if tsconfig != "" {
		if err := MergePathsConfig(tsconfig, outDir, stdout); err != nil {
			return err
		}
	}
	// vite.config is no longer touched — viteless serves the frontend and
	// owns the dev proxy, so there's no managed proxy block to inject.
	_ = viteConfig
	return nil
}

// WriteIfChanged writes body to path only when the file is missing
// or its current contents differ from body. Logs "wrote" with the
// byte count on a real write, "unchanged" when the disk copy
// already matched. Skipping the no-op write preserves mtime — file
// watchers (vite, webpack-dev-server, JetBrains' indexer) don't
// re-trigger on idempotent re-runs.
//
// Bytes-equal comparison rather than hash because the SDK files
// are tens of KB at most; the explicit byte-slice equality is
// allocation-free for the common no-change case.
//
// Exported because the CLI (cmd/nexus/client_cmd.go) and the
// in-process Dump path share the same write contract — keeping
// one helper means a fix for either site lands everywhere.
func WriteIfChanged(path string, body []byte, stdout io.Writer) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, body) {
		// Deliberately silent. The in-process dump runs on every boot, so a
		// line per already-current file meant ten lines of "nothing happened"
		// on each restart of `nexus dev`.
		return nil
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fdumpLine(stdout, ansiGreen, "wrote", path, fmt.Sprintf("%d bytes", len(body)))
	return nil
}

// WriteInertiaDTS writes inertia.d.ts when body is non-empty. When body is
// empty (the manifest no longer has pages or typed shared props) a
// previously generated copy is removed: it imports NexusSharedProps from
// ./client, which the regenerated client.d.ts no longer exports, so leaving
// it would break type-checking. A file without the generated banner is the
// developer's and is left alone. Shared by Dump and `nexus client`.
func WriteInertiaDTS(path string, body []byte, stdout io.Writer) error {
	if len(body) > 0 {
		return WriteIfChanged(path, body, stdout)
	}
	existing, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(existing, []byte(generatedBanner)) {
		return nil
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	fdumpLine(stdout, ansiYellow, "removed", path, "no Inertia pages or typed shared props")
	return nil
}

// WriteIfMissing writes body to path only when the file does not
// already exist. The first run scaffolds it; subsequent runs
// observe the user's edits and skip. Distinct from WriteIfChanged
// (which compares bytes and rewrites on drift) — used for the
// nexus.ts wiring file that the developer is expected to edit.
func WriteIfMissing(path string, body []byte, stdout io.Writer) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fdumpLine(stdout, ansiGreen, "wrote", path, fmt.Sprintf("%d bytes, scaffold — feel free to edit", len(body)))
	return nil
}

// MergePathsConfig writes (or merges into) a jsconfig.json /
// tsconfig.json at configPath, adding compilerOptions.paths
// entries that map the runtime URL imports back to the SDK files
// in outDir. Existing fields (compilerOptions.target, include,
// exclude, custom paths entries) are preserved entry-for-entry.
//
// File shape is identical for jsconfig and tsconfig — caller picks
// the filename. No baseUrl is added (TypeScript resolves paths against
// the config file without one, and 6.0 deprecates it); an existing
// baseUrl is honoured, with the mapped paths computed relative to it.
//
// Besides the runtime URL imports, the bare specifier "nexus-client"
// maps to the SDK's client.d.ts, which carries every generated type:
// `import type { NexusPageProps } from 'nexus-client'` resolves in the
// editor, in vue-tsc, and in Vue's SFC compiler (which resolves through
// these paths when TypeScript is installed). It names the .d.ts, not
// client.js: TypeScript does not swap declarations in for a mapped .js
// target. nexus-vite-plugin aliases the same name to client.js for Vite,
// so a value import works at runtime too.
//
// When the config lists "include", the SDK's client.d.ts joins it (it
// references inertia.d.ts): inertia.d.ts types page.props through a
// global augmentation, which applies only if the file is in the program —
// and a component that just calls usePage() imports nothing that would
// pull it in. A user's own 'nexus-client' mapping is left alone. A
// solution-style root (files: [] + references) is not written; the
// referenced configs covering src/ are.
//
// Exported for the same reason as WriteIfChanged: the CLI flag
// (--tsconfig / --jsconfig) and the in-process Dump path share
// the same merge logic.
func MergePathsConfig(configPath, outDir string, stdout io.Writer) error {
	doc, err := readConfigJSONC(configPath)
	if err != nil {
		return err
	}
	// A solution-style root ("files": [] plus "references", the create-vue
	// and create-vite layout) compiles nothing itself: paths written there
	// reach no source file. The mapping belongs in the referenced configs
	// that cover the app's sources.
	if refs := solutionReferences(doc, configPath); len(refs) > 0 {
		merged := 0
		for _, ref := range refs {
			rdoc, err := readConfigJSONC(ref)
			if err != nil || !coversSources(rdoc) {
				continue
			}
			if err := mergePaths(ref, rdoc, outDir, stdout); err != nil {
				return err
			}
			merged++
		}
		if merged > 0 {
			return nil
		}
	}
	return mergePaths(configPath, doc, outDir, stdout)
}

// mergePaths adds the SDK mappings (and include entry) to one parsed
// config and writes it back if that changed anything.
func mergePaths(configPath string, doc map[string]any, outDir string, stdout io.Writer) error {
	co, _ := doc["compilerOptions"].(map[string]any)
	if co == nil {
		co = map[string]any{}
		doc["compilerOptions"] = co
	}
	base := filepath.Dir(configPath)
	if b, ok := co["baseUrl"].(string); ok && b != "" {
		if filepath.IsAbs(b) {
			base = b
		} else {
			base = filepath.Join(base, b)
		}
	}
	mappings, err := pathMappings(outDir, base)
	if err != nil {
		return fmt.Errorf("nexus client: compute paths: %w", err)
	}
	paths, _ := co["paths"].(map[string]any)
	if paths == nil {
		paths = map[string]any{}
		co["paths"] = paths
	}
	for k, v := range mappings {
		// A project that points 'nexus-client' somewhere of its own (a
		// wrapper module) keeps it; only a mapping that names a generated
		// SDK file is ours to update.
		if k == "nexus-client" && !generatedClientMapping(paths[k]) {
			continue
		}
		paths[k] = v
	}
	if include, ok := doc["include"].([]any); ok {
		if entry, err := includeEntry(filepath.Dir(configPath), outDir); err == nil && !containsString(include, entry) {
			doc["include"] = append(include, entry)
		}
	}

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return fmt.Errorf("nexus client: mkdir %s: %w", filepath.Dir(configPath), err)
	}
	return WriteIfChanged(configPath, body, stdout)
}

// readConfigJSONC parses a tsconfig/jsconfig, or returns an empty
// document when the file does not exist yet.
func readConfigJSONC(configPath string) (map[string]any, error) {
	var doc map[string]any
	if existing, err := os.ReadFile(configPath); err == nil {
		// tsconfig/jsconfig are JSONC — tsc tolerates // and /* */ comments
		// and trailing commas, so a hand-edited or editor-formatted file may
		// contain them. Strip those before the strict encoding/json parse.
		if err := json.Unmarshal(stripJSONC(existing), &doc); err != nil {
			return nil, fmt.Errorf("nexus client: parse existing %s: %w", configPath, err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

// solutionReferences returns the config files a solution-style root
// refers to — a root with an empty "files", no "include", and
// "references" — resolved against the root's directory (a reference
// naming a directory means its tsconfig.json). nil for any other config.
func solutionReferences(doc map[string]any, configPath string) []string {
	files, ok := doc["files"].([]any)
	if !ok || len(files) != 0 {
		return nil
	}
	if _, has := doc["include"]; has {
		return nil
	}
	refs, _ := doc["references"].([]any)
	var out []string
	for _, r := range refs {
		m, _ := r.(map[string]any)
		p, _ := m["path"].(string)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(configPath), p)
		}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			p = filepath.Join(p, "tsconfig.json")
		}
		out = append(out, p)
	}
	return out
}

// coversSources reports whether a referenced config compiles the app's
// sources (an include entry under src/), as tsconfig.app.json does and
// tsconfig.node.json (vite.config.*) does not.
func coversSources(doc map[string]any) bool {
	include, _ := doc["include"].([]any)
	for _, v := range include {
		s, _ := v.(string)
		s = strings.TrimPrefix(s, "./")
		if s == "src" || strings.HasPrefix(s, "src/") {
			return true
		}
	}
	return false
}

// generatedClientMapping reports whether a paths value is absent or
// names only a generated SDK client file (client.d.ts / client.js).
func generatedClientMapping(v any) bool {
	if v == nil {
		return true
	}
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return false
	}
	for _, e := range list {
		s, _ := e.(string)
		if !strings.HasSuffix(s, "/client.d.ts") && !strings.HasSuffix(s, "/client.js") {
			return false
		}
	}
	return true
}

// stripJSONC returns data with JSONC extensions removed so encoding/json can
// parse it: // line comments, /* */ block comments, and trailing commas before
// a } or ]. String literals are preserved verbatim (comment/comma markers
// inside a string are left alone). tsconfig/jsconfig are JSONC, so this makes
// nexus tolerant of files tsc itself accepts. Note the caller re-marshals the
// parsed doc with json.MarshalIndent, so the rewritten file is strict JSON —
// any comments are dropped on write, exactly as any JSON round-trip would.
func stripJSONC(data []byte) []byte {
	// Pass 1: strip comments.
	noComments := make([]byte, 0, len(data))
	inString, escaped := false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			noComments = append(noComments, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			noComments = append(noComments, c)
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				noComments = append(noComments, '\n')
			}
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			i += 2
			for i+1 < len(data) && !(data[i] == '*' && data[i+1] == '/') {
				i++
			}
			i++ // skip the closing '*'; loop's i++ skips the '/'
		default:
			noComments = append(noComments, c)
		}
	}

	// Pass 2: drop trailing commas (a comma whose next non-space token is } or ]).
	out := make([]byte, 0, len(noComments))
	inString, escaped = false, false
	for i := 0; i < len(noComments); i++ {
		c := noComments[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(noComments) {
				if b := noComments[j]; b == ' ' || b == '\t' || b == '\n' || b == '\r' {
					j++
					continue
				}
				break
			}
			if j < len(noComments) && (noComments[j] == '}' || noComments[j] == ']') {
				continue // drop the trailing comma
			}
		}
		out = append(out, c)
	}
	return out
}

// includeEntry is the "include" entry that puts the SDK's types in the
// program: client.d.ts, which references inertia.d.ts. Not a *.d.ts glob,
// which would also pull in vue.d.ts (and its 'vue' import) for a project
// that doesn't use Vue. Relative to the config file's directory (include
// is never resolved against baseUrl).
func includeEntry(configDir, outDir string) (string, error) {
	rel, err := filepath.Rel(configDir, outDir)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return "client.d.ts", nil
	}
	return rel + "/client.d.ts", nil
}

func containsString(list []any, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// pathMappings is the specifier → file map written into the config,
// relative to base (the config file's directory, or its baseUrl) so the
// paths stay portable when the project moves on disk. Always "./"-led and
// forward-slashed: TypeScript resolves a paths entry against base either
// way, and the spec calls for forward slashes on every platform.
func pathMappings(outDir, base string) (map[string][]string, error) {
	rel, err := filepath.Rel(base, outDir)
	if err != nil {
		return nil, err
	}
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." {
		rel = "."
	} else if !strings.HasPrefix(rel, "../") {
		rel = "./" + rel
	}
	return map[string][]string{
		"nexus-client":              {rel + "/client.d.ts"},
		"/__nexus/client/client.js": {rel + "/client.js"},
		"/__nexus/client/vue.js":    {rel + "/vue.js"},
	}, nil
}
