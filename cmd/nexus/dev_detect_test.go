package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDetectFrontendDir covers the auto-spawn discovery path:
// nexus dev scans the user's main package for a ServeFrontend
// call and derives the frontend project root from the embed-root
// argument. Each case writes a small main.go fixture, runs the
// detector, and asserts the inferred dir matches.
func TestDetectFrontendDir(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"qualified ServeFrontend with web/dist",
			`package main
import "github.com/paulmanoni/nexus"
//go:embed web/dist
var distFS = "stub"
func main() { nexus.ServeFrontend(distFS, "web/dist") }
`,
			"web",
		},
		{
			"unqualified (dot-import) ServeFrontend",
			`package main
import . "github.com/paulmanoni/nexus"
var distFS = "stub"
func main() { ServeFrontend(distFS, "client/build") }
`,
			"client",
		},
		{
			"single-segment embed root",
			`package main
import "github.com/paulmanoni/nexus"
var distFS = "stub"
func main() { nexus.ServeFrontend(distFS, "dist") }
`,
			"dist",
		},
		{
			"non-literal embed root falls through",
			`package main
import "github.com/paulmanoni/nexus"
var (
    distFS = "stub"
    root   = "web/dist"
)
func main() { nexus.ServeFrontend(distFS, root) }
`,
			"",
		},
		{
			"no ServeFrontend at all",
			`package main
func main() {}
`,
			"",
		},
		{
			"frontend.Plugin(Config{Root: web})",
			`package main
import "github.com/paulmanoni/nexus/extension/frontend"
var fs = "stub"
func main() {
    frontend.Plugin(frontend.Config{Root: "web", FS: fs})
}
`,
			"web",
		},
		{
			"frontend.Plugin with Root not first field",
			`package main
import "github.com/paulmanoni/nexus/extension/frontend"
var fs = "stub"
func main() {
    frontend.Plugin(frontend.Config{
        FS:        fs,
        Framework: frontend.Vue,
        Root:      "client",
    })
}
`,
			"client",
		},
		{
			"frontend.Plugin without Root field falls through",
			`package main
import "github.com/paulmanoni/nexus/extension/frontend"
var fs = "stub"
func main() { frontend.Plugin(frontend.Config{FS: fs}) }
`,
			"",
		},
		{
			"frontend.Plugin with non-literal Root falls through",
			`package main
import "github.com/paulmanoni/nexus/extension/frontend"
var (
    fs   = "stub"
    root = "web"
)
func main() { frontend.Plugin(frontend.Config{Root: root, FS: fs}) }
`,
			"",
		},
		{
			"both ServeFrontend and frontend.Plugin → ServeFrontend wins (legacy first)",
			`package main
import (
    "github.com/paulmanoni/nexus"
    "github.com/paulmanoni/nexus/extension/frontend"
)
var distFS = "stub"
func main() {
    nexus.ServeFrontend(distFS, "old/dist")
    frontend.Plugin(frontend.Config{Root: "new", FS: distFS})
}
`,
			"old",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			got := detectFrontendDir(dir)
			if got != tc.want {
				t.Errorf("detectFrontendDir = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectFrontendDir_SkipsTestsAndGenerated(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "zz_deploy_gen.go"), []byte(
		`package main
import "github.com/paulmanoni/nexus"
var fakeFS = "stub"
func init() { nexus.ServeFrontend(fakeFS, "STALE/dist") }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main_test.go"), []byte(
		`package main
import "github.com/paulmanoni/nexus"
var testFS = "stub"
func TestX() { nexus.ServeFrontend(testFS, "TEST/dist") }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(
		`package main
import "github.com/paulmanoni/nexus"
var distFS = "stub"
func main() { nexus.ServeFrontend(distFS, "real/dist") }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectFrontendDir(dir); got != "real" {
		t.Errorf("detectFrontendDir = %q, want %q (overlay/test files should be skipped)", got, "real")
	}
}