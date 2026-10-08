package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// modelsApp is an app whose models embed orm.Model, with no orm.For: two
// packages the app links (models, blog) and one it doesn't (drafts).
func modelsApp(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a module")
	}
	_, file, _, _ := runtime.Caller(0)
	repo, _ := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	root := t.TempDir()
	var sum []byte
	for _, f := range []string{"go.sum", "orm/go.sum"} {
		b, err := os.ReadFile(filepath.Join(repo, f))
		if err != nil {
			t.Fatal(err)
		}
		sum = append(sum, b...)
	}
	files := map[string]string{
		"go.mod": "module example.com/shop\n\ngo 1.27\n\nrequire (\n\tgithub.com/paulmanoni/nexus/orm v0.0.0\n\tgithub.com/paulmanoni/nexus/v2 v2.0.0\n)\n\n" +
			"replace (\n\tgithub.com/paulmanoni/nexus/orm => " + filepath.Join(repo, "orm") + "\n\tgithub.com/paulmanoni/nexus/v2 => " + repo + "\n)\n",
		"go.sum":     string(sum),
		"nexus.toml": "[databases.main]\ndriver = \"sqlite\"\nname = \"app.db\"\ndefault = true\n",
		"main.go":    "package main\n\nimport (\n\t_ \"example.com/shop/blog\"\n\n\t_ \"github.com/paulmanoni/nexus/v2/db/sqlite\"\n)\n\nfunc main() {}\n",
		"models/user.go": `package models

import "github.com/paulmanoni/nexus/orm"

type User struct {
	orm.Model[User]
	ID    int64
	Email string
}

// UserRow adds a field to a user: a model of User's, not its own.
type UserRow struct {
	User
	Posts int
}
`,
		"blog/post.go": `package blog

import (
	"github.com/paulmanoni/nexus/orm"

	"example.com/shop/models"
)

type base[T any] struct {
	orm.Model[T]
	ID int64
}

type Post struct {
	base[Post]
	Title    string
	AuthorID int64
	Author   *models.User
}
`,
		"drafts/draft.go": `package drafts

import "github.com/paulmanoni/nexus/orm"

type Draft struct {
	orm.Model[Draft]
	ID int64
}
`,
	}
	for name, src := range files {
		writeFile(t, filepath.Join(root, name), src)
	}
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOPROXY", "off")
	return root
}

func TestGenerateModels(t *testing.T) {
	root := modelsApp(t)
	results, err := ormArtifacts(root, false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range results {
		rel, _ := filepath.Rel(root, r.Path)
		got[rel] = string(r.Content)
	}
	for file, want := range map[string]string{
		"models/orm_scanners_gen.go": "orm.Register[User]()",
		"blog/orm_scanners_gen.go":   "orm.Register[Post]()",
		"drafts/orm_scanners_gen.go": "orm.Register[Draft]()",
	} {
		if !strings.Contains(got[file], want) {
			t.Errorf("%s lacks %s:\n%s", file, want, got[file])
		}
	}
	if strings.Contains(got["models/orm_scanners_gen.go"], "Register[UserRow]") || strings.Contains(got["blog/orm_scanners_gen.go"], "Register[base") {
		t.Errorf("registered a type that isn't a model of its own:\n%s\n%s", got["models/orm_scanners_gen.go"], got["blog/orm_scanners_gen.go"])
	}

	var out, errb bytes.Buffer
	if err := writeGenerated(results, true, "models", &out, &errb); err == nil || !strings.Contains(errb.String(), "models/orm_scanners_gen.go") {
		t.Fatalf("--check before writing: %v\n%s", err, errb.String())
	}
	if err := writeGenerated(results, false, "models", &out, &errb); err != nil {
		t.Fatal(err)
	}
	if err := writeGenerated(results, true, "models", &out, &errb); err != nil {
		t.Fatalf("--check after writing: %v", err)
	}
}

// TestMakeMigrationsModels: the tool build overlays the registrations,
// so models with no orm.For are planned — those of linked packages only.
func TestMakeMigrationsModels(t *testing.T) {
	root := modelsApp(t)
	var out, errb bytes.Buffer
	if err := runMakeMigrations(makeMigrationsOpts{root: root, dryRun: true}, &out, &errb); err != nil {
		t.Fatalf("%v\n%s", err, errb.String())
	}
	plan := out.String()
	for _, want := range []string{`Table: "users"`, `Table: "posts"`, `Name: "0001_initial"`} {
		if !strings.Contains(plan, want) {
			t.Errorf("the plan lacks %s:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "drafts") {
		t.Errorf("planned a model of a package the app doesn't link:\n%s", plan)
	}
}
