package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestLintV2(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), `package main

import (
	"embed"

	nx "github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/auth"
	"github.com/paulmanoni/nexus/middleware"
	"go.uber.org/zap"
)

var webFS embed.FS

func main() {
	cfg := nx.MustLoadConfig()
	cfg.Middleware.Global = append(cfg.Middleware.Global, middleware.Middleware{Name: "x", Gin: nil})
	app := nx.New(cfg)
	app.UseVolume(v)
	_ = zap.NewNop
	nx.Run(cfg,
		nx.ServeFrontend(webFS, "web/dist"),
		nx.AsQuery(NewListPets),
		nx.AsQuery(NewListOwners, nx.Op("listOwners")),
		nx.AsMutation(CreatePet),
		nx.Error(err),
		auth.LoginEndpoint(),
	)
}

type GetArgs struct {
	ID   string `+"`uri:\"id\"`"+`
	Slug string `+"`path:\"slug\"`"+`
}
`)
	writeFile(t, filepath.Join(dir, "users", "users.go"), `package users

// GetUser returns one user.
//
//@rest GET /users/:id
func GetUser() {}

// @query
func NewSearchUsers() {}

//nexus:query
//nexus:use nexus.Op("listUsers")
func NewListUsers() {}

//nexus:mutation
func DeleteUser() {}

// @Summary is swag's, not nexus's.
func Other() {}
`)
	writeFile(t, filepath.Join(dir, "views", "home.templ"), "package views\n\n//@page GET /\ntempl Home() {}\n\n//nexus:page GET /about\ntempl About() {}\n")
	writeFile(t, filepath.Join(dir, "nexus.toml"), "environment = \"production\"\n\n[runtime]\nintrospection = true\n\n[billing]\nplan = \"pro\"\n")

	findings, err := lintV2(dir)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, f := range findings {
		lines = append(lines, filepath.Base(f.File)+":"+itoa(f.Line)+": "+f.Message)
	}
	got := strings.Join(lines, "\n")

	for _, want := range []string{
		"main.go:15: nx.MustLoadConfig → config.MustLoad",
		"main.go:16: Config.Middleware.Global → nexus.Middleware(",
		"main.go:16: middleware.Middleware{Gin: …} → HTTP",
		"main.go:18: .UseVolume → App.DeclareVolume",
		"main.go:9: go.uber.org/zap → log/slog",
		"main.go:21: nx.ServeFrontend → nexus.Frontend",
		`main.go:22: op "listPets" from NewListPets: 2.0 names it "newListPets"`,
		`nx.Op("listPets")`,
		"main.go:25: nx.Error(err) → nexus.FailBoot(err)",
		"main.go:26: auth.LoginEndpoint → auth.Config.Endpoints",
		`main.go:31: uri:"id" → path:"id"`,
		"users.go:5: //@rest → //nexus:rest",
		"users.go:8: //@query → //nexus:query",
		`users.go:9: op "searchUsers" from NewSearchUsers`,
		"home.templ:3: //@page → //nexus:page",
		"nexus.toml:1: 2.0 fails boot on this key",
		`nexus.toml:6: [billing] is an app section`,
		"max_body_bytes is unset",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"listOwners", "listUsers", "CreatePet", "DeleteUser", "Summary", "slug", "home.templ:6", "nx.Run"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("unexpected %q in:\n%s", unwanted, got)
		}
	}

	var buf bytes.Buffer
	if err := runLintV2(&buf, dir, true); err != nil {
		t.Fatal(err)
	}
	var doc struct{ Findings []v2Finding }
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil || len(doc.Findings) != len(findings) {
		t.Fatalf("json: %v, %d findings:\n%s", err, len(doc.Findings), buf.String())
	}
}

func TestLintV2_Clean(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nimport \"github.com/paulmanoni/nexus\"\n\nfunc main() { nexus.Boot() }\n")
	writeFile(t, filepath.Join(dir, "nexus.toml"), "[runtime.server]\nmax_body_bytes = 1048576\n")
	var buf bytes.Buffer
	if err := runLintV2(&buf, dir, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "nothing to change") {
		t.Fatalf("clean project reported:\n%s", buf.String())
	}
}

func itoa(n int) string {
	var b [20]byte
	i := len(b)
	if n == 0 {
		return "0"
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
