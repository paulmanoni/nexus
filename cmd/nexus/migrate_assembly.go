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
	{regexp.MustCompile(`\bAsCRUD\[`),
		"AsCRUD is gone; write the resource as a controller — nexus.Resource[*PetsController](\"/pets\") registers Index/Show/Create/Update/Destroy on the same routes"},
	{regexp.MustCompile(`\bauth\.(Single|Authentication|Authorization|UseBackend|StaticBackend|Scheme\{|Bearer|Cookie|APIKey|Chain|CacheFor|IdentityFrom|SubjectPtr|Describe|LoginEndpoint|LogoutEndpoint|LoginHandler|LogoutHandler|Endpoints|Manager|MemoryUserStore|NewMemoryUserStore|NewModelBackend|ModelBackend|Authenticate|ErrorHandler|Wildcard|AnyOf|AllOf|Authenticated|Permit|SessionCookie|Backend|PermissionFn|Resolver|Principal|DefaultPermissions)\b|\b(oauth2|iauth)\.[A-Z]|auth\.Identity\{[^}]*\b(Roles|Scopes|Extra):`),
		"extension/auth's v1 API is removed: implement auth.Users (FindLogin, Load) and use auth.Module(auth.Config{Users: auth.UseUsers(NewUsers)}); Identity.Roles/Scopes become Perms, Extra becomes User — docs/guide/migrating-to-v2.md (Auth)"},
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
