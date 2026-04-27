package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// newNewCmd builds the `nexus new` subcommand. The cobra wrapper is
// thin — all the real work lives in scaffold(), which is also driven
// directly by the tests.
func newNewCmd(stdout, stderr io.Writer) *cobra.Command {
	var modulePath string
	cmd := &cobra.Command{
		Use:   "new <dir>",
		Short: "Scaffold a minimal nexus app",
		Long: `Scaffold a runnable nexus app in <dir>.

The generated project uses the reflective API (nexus.AsRest +
nexus.Module) so 'go mod tidy && go run .' produces a working app
with the dashboard already mounted at /__nexus/.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return scaffold(args[0], modulePath, stdout)
		},
	}
	cmd.Flags().StringVar(&modulePath, "module", "",
		"go.mod module path (default: derived from <dir>'s basename)")
	return cmd
}

// scaffold writes the skeleton files into dir. It refuses to touch an
// existing non-empty directory — a misaimed `nexus new .` in someone's
// repo could clobber live code otherwise.
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
		"go.mod":            tmplGoMod(modulePath),
		"main.go":           tmplMainGo,
		"module.go":         tmplModuleGo,
		".gitignore":        tmplGitignore,
		"README.md":         tmplReadme(filepath.Base(abs)),
		"nexus.deploy.yaml": tmplDeployYaml,
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
	fmt.Fprintf(stdout, "  nexus dev                          # one process, dashboard at http://localhost:8080/__nexus/\n")
	fmt.Fprintf(stdout, "  nexus build --deployment monolith  # produce ./bin/monolith\n")
	fmt.Fprintf(stdout, "\n")
	fmt.Fprintf(stdout, "Edit nexus.deploy.yaml to add split deployments — the file's\n")
	fmt.Fprintf(stdout, "comments walk through tagging a module DeployAs(...), declaring a\n")
	fmt.Fprintf(stdout, "unit + port, and wiring peers. Then `nexus dev --split` boots\n")
	fmt.Fprintf(stdout, "every unit as a subprocess with cross-service HTTP between them.\n")
	return nil
}

// isValidModulePath is a loose check — enough to catch typos ("my app"
// with spaces) without replicating the full spec. `go mod init` will
// still reject anything subtly wrong; this is a pre-flight guard.
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

// tmplGoMod writes a minimal go.mod with no pinned dependencies. The
// nexus require lands when the user runs `go mod tidy` — this avoids
// baking in a version that may not be published yet.
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
			Server:    nexus.ServerConfig{Addr: ":8080"},
			Dashboard: nexus.DashboardConfig{Enabled: true},
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

// tmplDeployYaml is the starter manifest. It declares a single
// monolith deployment and embeds a hand-walkthrough showing how to
// split modules into independent services. The user edits this file
// (not main.go) when topology changes.
const tmplDeployYaml = `# nexus.deploy.yaml — deployment topology for this app.
#
# 'nexus build --deployment NAME' reads this file to decide which
# modules compile locally and which become HTTP-stub shadows.
# 'nexus dev --split' reads it to launch one subprocess per split
# unit. Application code (main.go, modules) stays
# deployment-agnostic; everything per-environment lives here.
#
# ── Concepts ──────────────────────────────────────────────────────
#
# deployments:    map of unit name → { owns: [...], port: N }
#                 Empty 'owns' = "owns every module" (the monolith).
#                 Listed 'owns' = real split unit; modules NOT
#                 listed get replaced by HTTP-stub shadows in this
#                 unit's binary.
# peers:          map of DeployAs-tag → transport config (URL,
#                 timeout, retries, min_version, auth). Codegen bakes
#                 this into the binary as Config.Topology defaults.

deployments:
  # Monolith owns every module by default. Run with:
  #     nexus build --deployment monolith
  #     ./bin/monolith
  monolith:
    port: 8080

# ── How to split a module out ─────────────────────────────────────
#
# 1. Tag the module's declaration with DeployAs:
#
#        var Module = nexus.Module("orders",
#            nexus.DeployAs("orders-svc"),  // names the deployment unit
#            nexus.Provide(NewService),
#            nexus.AsRest("GET", "/orders/:id", NewGet),
#        )
#
# 2. Add a deployment for it here, and add it to monolith's owns
#    list (or leave monolith empty so it auto-includes everything):
#
#        deployments:
#          monolith:
#            port: 8080
#          orders-svc:
#            owns: [orders]
#            port: 8081
#
# 3. Add a peer entry so other services can reach it. URL defaults
#    to http://localhost:<port> for local dev — override with an
#    env var in prod:
#
#        peers:
#          orders-svc:
#            timeout: 2s
#            # url: ${ORDERS_SVC_URL}     # interpolated at boot
#            # min_version: v0.9          # warn on peer-version skew
#            # retries: 1                 # idempotent retries only
#            # auth:                      # bearer-token credential
#            #   type: bearer
#            #   token: ${ORDERS_SVC_TOKEN}
#
# 4. Cross-module call sites stay unchanged — checkout's struct
#    field is *orders.Service in every binary; the build tool
#    swaps the body at compile time:
#
#        type Service struct {
#            *nexus.Service
#            orders *orders.Service   // local in monolith, HTTP in split
#        }
#
# 5. Build (or run) per deployment:
#
#        nexus build --deployment orders-svc   # ./bin/orders-svc
#        nexus build --deployment monolith     # ./bin/monolith
#        nexus dev --split                     # all units, one terminal

# ── Example split topology (uncomment to enable) ──────────────────
#
# deployments:
#   monolith:
#     port: 8080
#   orders-svc:
#     owns: [orders]
#     port: 8081
#   billing-svc:
#     owns: [billing]
#     port: 8082
#
# peers:
#   orders-svc:
#     timeout: 2s
#   billing-svc:
#     timeout: 2s
#     auth:
#       type: bearer
#       token: ${BILLING_SVC_TOKEN}
`

func tmplReadme(name string) string {
	return fmt.Sprintf(`# %s

Generated with `+"`nexus new`"+`.

## Run (single process)

    go mod tidy
    nexus dev

Then open http://localhost:8080/__nexus/ for the dashboard, and:

    curl 'http://localhost:8080/hello?name=Paul'

## Build a deployable binary

    nexus build --deployment monolith
    ./bin/monolith

## Split into microservices

Edit `+"`nexus.deploy.yaml`"+` to declare additional deployments and tag
your modules with `+"`nexus.DeployAs(\"...\")`"+`. The manifest comments
walk through each step. Then:

    nexus dev --split           # all units in one terminal
    nexus build --deployment orders-svc

Application code stays unchanged — the framework swaps cross-module
*Service struct bodies between the local impl and HTTP-stub shadows
at compile time, based on the active deployment.
`, name)
}