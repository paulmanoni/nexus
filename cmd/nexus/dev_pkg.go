package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	devBuildScriptName = "dev:build"
	devBuildScriptCmd  = "vite build --watch --emptyOutDir false"
)

// ensureDevBuildScript injects the dev:build script into the
// frontend project's package.json when it isn't already declared.
// Idempotent — re-running is a no-op once the entry has landed.
//
// Operates on the raw bytes (no full JSON re-marshaling) so the
// file's existing key order, indentation, and any custom fields the
// project has added stay untouched. Skips silently when the file
// doesn't exist or the JSON shape is too irregular for the
// heuristic — the watcher then surfaces npm's "missing script"
// error, which the user can fix with a one-line manual addition.
func ensureDevBuildScript(frontendDir string, stdout io.Writer) error {
	pkgPath := filepath.Join(frontendDir, "package.json")
	src, err := os.ReadFile(pkgPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(stdout, "[nexus] %s not found — skip script injection\n", pkgPath)
			return nil
		}
		return err
	}
	body := string(src)

	// Idempotence: bail if the script key already appears anywhere
	// in the file. Constructed as `"dev:build":` so a string VALUE
	// containing "dev:build" can't false-match.
	if strings.Contains(body, `"`+devBuildScriptName+`":`) {
		return nil
	}

	indent := detectJSONIndent(body)
	twoLevel := indent + indent
	entryLine := fmt.Sprintf(`"%s": "%s"`, devBuildScriptName, devBuildScriptCmd)

	scriptsRe := regexp.MustCompile(`"scripts"\s*:\s*\{`)
	loc := scriptsRe.FindStringIndex(body)
	if loc != nil {
		openBrace := loc[1] - 1
		closeBrace := matchingCloseBrace(body, openBrace)
		if closeBrace < 0 {
			return fmt.Errorf("malformed scripts object in %s", pkgPath)
		}
		var insertion string
		if strings.TrimSpace(body[openBrace+1:closeBrace]) == "" {
			insertion = "\n" + twoLevel + entryLine + "\n" + indent
		} else {
			// Prepend so the new entry doesn't compete with a
			// trailing-comma policy choice the project may have made.
			insertion = "\n" + twoLevel + entryLine + ","
		}
		out := body[:openBrace+1] + insertion + body[openBrace+1:]
		if err := os.WriteFile(pkgPath, []byte(out), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s●%s added %s script to %s\n", ansiCyan, ansiReset, devBuildScriptName, pkgPath)
		return nil
	}

	// No scripts object yet — append one near the root close brace.
	rootClose := lastTopLevelBrace(body)
	if rootClose < 0 {
		return fmt.Errorf("no top-level object in %s", pkgPath)
	}
	prefix := body[:rootClose]
	trimmed := strings.TrimRight(prefix, " \t\n\r")
	suffix := prefix[len(trimmed):]
	last := byte('{')
	if len(trimmed) > 0 {
		last = trimmed[len(trimmed)-1]
	}
	scriptsBlock := indent + `"scripts": {` + "\n" +
		twoLevel + entryLine + "\n" +
		indent + "}"
	var insertion string
	switch last {
	case '{':
		insertion = "\n" + scriptsBlock + "\n"
	case ',':
		insertion = "\n" + scriptsBlock + suffix
	default:
		insertion = ",\n" + scriptsBlock + suffix
	}
	out := trimmed + insertion + body[rootClose:]
	if err := os.WriteFile(pkgPath, []byte(out), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s●%s added scripts.%s to %s\n", ansiCyan, ansiReset, devBuildScriptName, pkgPath)
	return nil
}

// detectJSONIndent reads the first leading-whitespace prefix in body
// (the indentation the project's editor / formatter has chosen) and
// returns it. Falls back to two spaces when the file uses zero
// indentation or starts with content on the same line.
func detectJSONIndent(body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || trimmed == line {
			continue
		}
		return line[:len(line)-len(trimmed)]
	}
	return "  "
}

// matchingCloseBrace returns the offset of the `}` that closes the
// `{` at openIdx, ignoring braces inside strings. Returns -1 when
// the braces don't balance.
func matchingCloseBrace(s string, openIdx int) int {
	if openIdx >= len(s) || s[openIdx] != '{' {
		return -1
	}
	depth := 0
	var inStr byte
	for i := openIdx; i < len(s); i++ {
		c := s[i]
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
		case '"':
			inStr = c
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// lastTopLevelBrace returns the offset of the `}` that closes the
// outermost JSON object in body. Symmetric with the file's first `{`.
func lastTopLevelBrace(body string) int {
	open := strings.IndexByte(body, '{')
	if open < 0 {
		return -1
	}
	return matchingCloseBrace(body, open)
}