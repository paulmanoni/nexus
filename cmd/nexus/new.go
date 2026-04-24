package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// cmdNew scaffolds a minimal nexus app in the given directory. The generated
// project uses the reflective API (`nexus.AsRest` + module composition) so
// running `go mod tidy && go run .` produces a working app with the dashboard
// already mounted.
func cmdNew(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	module := fs.String("module", "", "go.mod module path (default: derived from dir basename)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: nexus new [-module path] <dir>")
	}
	dir := fs.Arg(0)
	if err := scaffold(dir, *module, stdout); err != nil {
		return err
	}
	return nil
}

// scaffold writes the skeleton files into dir. It refuses to touch an existing
// non-empty directory — a misaimed `nexus new .` in someone's repo could clobber
// live code otherwise.
func scaffold(dir, modulePath string, stdout io.Writer) error {
	if dir == "" {
		return fmt.Errorf("directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if modulePath == "" {
		modulePath = filepath.Base(abs)
	}
	if !isValidModulePath(modulePath) {
		return fmt.Errorf("module path %q is not a valid Go module path", modulePath)
	}

	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		entries, _ := os.ReadDir(abs)
		if len(entries) > 0 {
			return fmt.Errorf("%s already exists and is not empty — refusing to overwrite", abs)
		}
	} else if err == nil {
		return fmt.Errorf("%s exists and is not a directory", abs)
	}

	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}

	files := map[string]string{
		"go.mod":     tmplGoMod(modulePath),
		"main.go":    tmplMainGo,
		"module.go":  tmplModuleGo,
		".gitignore": tmplGitignore,
		"README.md":  tmplReadme(filepath.Base(abs)),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(abs, name), []byte(content), 0o644); err != nil {
			return err
		}
	}

	fmt.Fprintf(stdout, "Scaffolded %s (module %s)\n", abs, modulePath)
	fmt.Fprintf(stdout, "Next:\n")
	fmt.Fprintf(stdout, "  cd %s\n", dir)
	fmt.Fprintf(stdout, "  go mod tidy\n")
	fmt.Fprintf(stdout, "  nexus dev        # then open http://localhost:8080/__nexus/\n")
	return nil
}

// isValidModulePath is a loose check — enough to catch typos ("my app" with
// spaces) without replicating the full spec. `go mod init` will still reject
// anything subtly wrong; this is a pre-flight guard.
func isValidModulePath(p string) bool {
	if p == "" || strings.ContainsAny(p, " \t\n") {
		return false
	}
	for _, r := range p {
		if r < 0x20 {
			return false
		}
	}
	return true
}

// tmplGoMod writes a minimal go.mod with no pinned dependencies. The nexus
// require lands when the user runs `go mod tidy` — this avoids baking in a
// version that may not be published yet.
func tmplGoMod(module string) string {
	return fmt.Sprintf(`module %s

go 1.25.1
`, module)
}

const tmplMainGo = `package main

import "github.com/paulmanoni/nexus"

// main boots the framework, mounts the dashboard at /__nexus/, and wires
// one module (see module.go). Run with ` + "`nexus dev`" + ` or ` + "`go run .`" + ` and hit
// http://localhost:8080/__nexus/ to see the live architecture view.
func main() {
	nexus.Run(
		nexus.Config{
			Addr:            ":8080",
			EnableDashboard: true,
		},
		helloModule,
	)
}
`

const tmplModuleGo = `package main

import "github.com/paulmanoni/nexus"

// HelloService — a typed wrapper around *nexus.Service so fx can route
// by type. Every handler that declares *HelloService as a dep grounds
// under the "hello" service on the dashboard's Architecture view.
type HelloService struct{ *nexus.Service }

func NewHelloService(app *nexus.App) *HelloService {
	return &HelloService{app.Service("hello").Describe("Hello world")}
}

type HelloResponse struct {
	Message string ` + "`json:\"message\"`" + `
}

type HelloArgs struct {
	Name string ` + "`graphql:\"name\" json:\"name\"`" + `
}

// NewHello is a reflective handler: the signature tells nexus how to wire it
// (first *Service dep grounds the op; nexus.Params[T] carries user input).
func NewHello(svc *HelloService, p nexus.Params[HelloArgs]) (*HelloResponse, error) {
	name := p.Args.Name
	if name == "" {
		name = "world"
	}
	return &HelloResponse{Message: "hello, " + name}, nil
}

var helloModule = nexus.Module("hello",
	nexus.Provide(NewHelloService),
	nexus.AsRest("GET", "/hello", NewHello),
)
`

const tmplGitignore = `/bin/
/dist/
/vendor/
*.test
*.out
.DS_Store
`

func tmplReadme(name string) string {
	return fmt.Sprintf(`# %s

Generated with ` + "`nexus new`" + `.

## Run

    go mod tidy
    nexus dev

Then open http://localhost:8080/__nexus/ to see the dashboard.

Hit the REST endpoint:

    curl 'http://localhost:8080/hello?name=Paul'
`, name)
}