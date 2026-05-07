package client

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MergeViteConfig wires the auto-select plugin into a Vite config
// file in two idempotent edits:
//
//   1. Adds `import nexusAutoSelect from '<rel>/nexus-vite-plugin.js'`
//      after the last existing top-level import.
//   2. Adds `nexusAutoSelect()` to the first plugins array it finds.
//
// String-pattern based — does NOT parse the config as TS. Handles
// the 95% of vite.config.{ts,js,mts,mjs} shapes that wrap a plugins
// array inside defineConfig({...}). When the heuristic can't locate
// a plugins array (variable-extracted plugins, conditional configs,
// etc.) the function leaves the file untouched and prints a hint
// pointing at the manual one-line change.
//
// configPath is the absolute or relative path to the user's Vite
// config; sdkDir is where nexus-vite-plugin.js was dumped (matches
// Config.Client.OutDir). Re-running is a no-op once both edits
// have landed — idempotency keys off the literal `nexusAutoSelect`
// identifier appearing anywhere in the file.
func MergeViteConfig(configPath, sdkDir string, stdout io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	src, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(stdout, "[nexus] vite config %q not found — skip auto-attach\n", configPath)
			return nil
		}
		return fmt.Errorf("read %s: %w", configPath, err)
	}

	body := string(src)
	cfgDir := filepath.Dir(configPath)
	pluginPath := filepath.Join(sdkDir, "nexus-vite-plugin.js")
	rel, err := filepath.Rel(cfgDir, pluginPath)
	if err != nil {
		return fmt.Errorf("relpath %s -> %s: %w", cfgDir, pluginPath, err)
	}
	rel = filepath.ToSlash(rel)
	if !strings.HasPrefix(rel, ".") {
		rel = "./" + rel
	}
	importLine := fmt.Sprintf("import nexusAutoSelect from '%s'", rel)

	changed := false
	// Idempotence: any existing reference to `nexusAutoSelect` means
	// we've wired it before — skip both edits.
	alreadyWired := strings.Contains(body, "nexusAutoSelect")

	if !alreadyWired {
		body = insertImport(body, importLine)
		changed = true
	}

	if !strings.Contains(body, "nexusAutoSelect()") {
		updated, ok := insertIntoPluginsArray(body, "nexusAutoSelect()")
		if !ok {
			fmt.Fprintf(stdout, "[nexus] couldn't locate a `plugins:` array in %s — add `nexusAutoSelect()` manually\n", configPath)
			// Still write the import (helpful even if the array edit failed).
			if changed {
				if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
					return fmt.Errorf("write %s: %w", configPath, err)
				}
			}
			return nil
		}
		body = updated
		changed = true
	}

	if !changed {
		return nil
	}
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	fmt.Fprintf(stdout, "[nexus] wired auto-select plugin into %s\n", configPath)
	return nil
}

// insertImport appends importLine after the last top-level `import …`
// line in body. Falls back to prepending when no imports exist.
func insertImport(body, importLine string) string {
	re := regexp.MustCompile(`(?m)^import .*$`)
	matches := re.FindAllStringIndex(body, -1)
	if len(matches) == 0 {
		return importLine + "\n" + body
	}
	last := matches[len(matches)-1]
	return body[:last[1]] + "\n" + importLine + body[last[1]:]
}

// insertIntoPluginsArray finds the first `plugins:` array in body and
// inserts entry inside it (before the closing `]`). Returns false
// when no `plugins:` array is found or the brackets don't balance.
func insertIntoPluginsArray(body, entry string) (string, bool) {
	keyRe := regexp.MustCompile(`plugins\s*:\s*\[`)
	loc := keyRe.FindStringIndex(body)
	if loc == nil {
		return body, false
	}
	openBracket := loc[1] - 1

	close := matchingCloseBracket(body, openBracket)
	if close < 0 {
		return body, false
	}

	inner := body[openBracket+1 : close]
	trimmed := strings.TrimRight(inner, " \t\n")
	suffix := inner[len(trimmed):]

	var insertion string
	switch {
	case strings.TrimSpace(trimmed) == "":
		insertion = entry
	case strings.HasSuffix(trimmed, ","):
		insertion = trimmed + " " + entry
	default:
		insertion = trimmed + ", " + entry
	}
	return body[:openBracket+1] + insertion + suffix + body[close:], true
}

// matchingCloseBracket returns the offset of the `]` that closes the
// `[` at openIdx, ignoring brackets inside strings/templates. Returns
// -1 when the brackets don't balance.
func matchingCloseBracket(s string, openIdx int) int {
	if openIdx >= len(s) || s[openIdx] != '[' {
		return -1
	}
	depth := 0
	var inStr byte
	var inLine, inBlock bool
	for i := openIdx; i < len(s); i++ {
		c := s[i]
		if inLine {
			if c == '\n' {
				inLine = false
			}
			continue
		}
		if inBlock {
			if c == '*' && i+1 < len(s) && s[i+1] == '/' {
				inBlock = false
				i++
			}
			continue
		}
		if inStr != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '/':
			if i+1 < len(s) && s[i+1] == '/' {
				inLine = true
				i++
				continue
			}
			if i+1 < len(s) && s[i+1] == '*' {
				inBlock = true
				i++
				continue
			}
		case '\'', '"', '`':
			inStr = c
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}