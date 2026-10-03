package main

import (
	"bytes"
	"regexp"
	"strings"
)

// migrateAssemblyFlags marks app-assembly code v2 replaced with something
// that can't be rewritten mechanically (docs/design/v2.md §12): each line
// gets a `// TODO(nexus v2): …` comment above it saying what replaces it.
// Running it twice adds nothing.
var migrateAssemblyFlags = []struct {
	match *regexp.Regexp
	todo  string
}{
	{regexp.MustCompile(`\.Middleware\.Global\b`),
		"Config.Middleware.Global is gone; register app-wide middleware with nexus.Middleware(...) — set middleware.Middleware.Stage for its place"},
	{regexp.MustCompile(`\bAsRestHandler\(`),
		"AsRestHandler is gone; register with nexus.AsRest — a raw handler takes its DI deps and *httpx.Ctx as parameters: func(m *Dep, c *httpx.Ctx)"},
	{regexp.MustCompile(`\bnexus\.Invoke\(\s*\w+\.Ensure\w*|\bnexus\.Invoke\(\s*\w*(Migrate|Seed|Backfill)\w*`),
		"pre-serve work belongs in nexus.Setup(...): it runs after resources start and before the listeners open"},
}

func migrateGoAssembly(rel string, src []byte) ([]byte, []migrateChange, error) {
	lines := bytes.Split(src, []byte("\n"))
	var out [][]byte
	var changes []migrateChange
	for i, line := range lines {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("//")) {
			out = append(out, line)
			continue
		}
		for _, f := range migrateAssemblyFlags {
			if !f.match.Match(line) {
				continue
			}
			todo := "// TODO(nexus v2): " + f.todo
			if i > 0 && strings.Contains(string(lines[i-1]), todo) {
				continue
			}
			indent := line[:len(line)-len(bytes.TrimLeft(line, " \t"))]
			out = append(out, append(append([]byte{}, indent...), todo...))
			changes = append(changes, migrateChange{Line: i + 1, Old: strings.TrimSpace(string(line)), New: todo})
		}
		out = append(out, line)
	}
	if len(changes) == 0 {
		return src, nil, nil
	}
	return bytes.Join(out, []byte("\n")), changes, nil
}
