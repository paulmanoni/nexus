package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
// stdout receives one line per file ("wrote …" / "unchanged …").
// Pass io.Discard to silence.
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
		fdumpLine(stdout, ansiYellow, "unchanged", path, fmt.Sprintf("%d bytes", len(body)))
		return nil
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fdumpLine(stdout, ansiGreen, "wrote", path, fmt.Sprintf("%d bytes", len(body)))
	return nil
}

// WriteIfMissing writes body to path only when the file does not
// already exist. The first run scaffolds it; subsequent runs
// observe the user's edits and skip. Distinct from WriteIfChanged
// (which compares bytes and rewrites on drift) — used for the
// nexus.ts wiring file that the developer is expected to edit.
func WriteIfMissing(path string, body []byte, stdout io.Writer) error {
	if _, err := os.Stat(path); err == nil {
		fdumpLine(stdout, ansiDim, "skipped", path, "already exists — edit freely")
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
// the filename. compilerOptions.baseUrl defaults to "." when
// missing (required for paths to resolve).
//
// Exported for the same reason as WriteIfChanged: the CLI flag
// (--tsconfig / --jsconfig) and the in-process Dump path share
// the same merge logic.
func MergePathsConfig(configPath, outDir string, stdout io.Writer) error {
	mappings, err := pathMappings(outDir, configPath)
	if err != nil {
		return fmt.Errorf("nexus client: compute paths: %w", err)
	}

	var doc map[string]any
	if existing, err := os.ReadFile(configPath); err == nil {
		// tsconfig/jsconfig are JSONC — tsc tolerates // and /* */ comments
		// and trailing commas, so a hand-edited or editor-formatted file may
		// contain them. Strip those before the strict encoding/json parse.
		if err := json.Unmarshal(stripJSONC(existing), &doc); err != nil {
			return fmt.Errorf("nexus client: parse existing %s: %w", configPath, err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}

	co, _ := doc["compilerOptions"].(map[string]any)
	if co == nil {
		co = map[string]any{}
		doc["compilerOptions"] = co
	}
	if _, ok := co["baseUrl"]; !ok {
		co["baseUrl"] = "."
	}
	paths, _ := co["paths"].(map[string]any)
	if paths == nil {
		paths = map[string]any{}
		co["paths"] = paths
	}
	for k, v := range mappings {
		paths[k] = v
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

// pathMappings is the URL → file map written into the config.
// Computes the relative path from the config file's directory to
// outDir so the resulting paths are portable when the project
// moves around on disk. Forward slashes regardless of OS — the
// jsconfig/tsconfig spec calls for them on every platform.
func pathMappings(outDir, configPath string) (map[string][]string, error) {
	configDir := filepath.Dir(configPath)
	rel, err := filepath.Rel(configDir, outDir)
	if err != nil {
		return nil, err
	}
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." {
		rel = "."
	}
	return map[string][]string{
		"/__nexus/client/client.js": {rel + "/client.js"},
		"/__nexus/client/vue.js":    {rel + "/vue.js"},
	}, nil
}
