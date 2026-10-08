package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/orm"
)

// TestMigrationCommands runs the migration commands on an app: makemigrations
// writes Go migrations (asking about a rename), migrate applies and
// unapplies them on its SQLite database, showmigrations and sqlmigrate
// read them.
func TestMigrationCommands(t *testing.T) {
	root := modelsApp(t)
	run := func(what string, f func(stdout, stderr *bytes.Buffer) error) string {
		t.Helper()
		var out, errb bytes.Buffer
		if err := f(&out, &errb); err != nil {
			t.Fatalf("%s: %v\n%s\n%s", what, err, out.String(), errb.String())
		}
		return out.String()
	}
	make := func(o makeMigrationsOpts) string {
		o.root = root
		return run("makemigrations", func(out, errb *bytes.Buffer) error { return runMakeMigrations(o, out, errb) })
	}
	migrate := func(target string) string {
		return run("migrate "+target, func(out, errb *bytes.Buffer) error {
			return runMigrationCommand(root, "", orm.CLIRequest{Command: "migrate", Target: target}, out, errb)
		})
	}
	show := func() string {
		return run("showmigrations", func(out, errb *bytes.Buffer) error {
			return runMigrationCommand(root, "", orm.CLIRequest{Command: "show"}, out, errb)
		})
	}

	if out := make(makeMigrationsOpts{}); !strings.Contains(out, "migrations/0001_initial.go") {
		t.Fatalf("makemigrations: %s", out)
	}
	src, err := os.ReadFile(filepath.Join(root, "migrations", "0001_initial.go"))
	if err != nil || !strings.Contains(string(src), "package migrations") || !strings.Contains(string(src), `m.F("email", m.Text())`) {
		t.Fatalf("0001_initial.go: %v\n%s", err, src)
	}
	if out := make(makeMigrationsOpts{check: true}); !strings.Contains(out, "No changes") {
		t.Fatalf("--check after it: %s", out)
	}

	// users.email renamed: asked, and on yes a rename.
	userGo := filepath.Join(root, "models", "user.go")
	old, _ := os.ReadFile(userGo)
	writeFile(t, userGo, strings.Replace(string(old), "Email string", "Mail  string", 1))
	if out := make(makeMigrationsOpts{dryRun: true, noinput: true}); !strings.Contains(out, `m.RemoveField{Table: "users", Name: "email"}`) || !strings.Contains(out, "// NOTE: drops users.email") {
		t.Fatalf("--noinput: %s", out)
	}
	if out := make(makeMigrationsOpts{interactive: true, in: strings.NewReader("y\n")}); !strings.Contains(out, "0002_rename_field_users_email_to_mail.go") {
		t.Fatalf("the rename: %s", out)
	}

	out := migrate("")
	if !strings.Contains(out, "migration applied: 0001_initial") || !strings.Contains(out, "migration applied: 0002_rename_field_users_email_to_mail") {
		t.Fatalf("migrate: %s", out)
	}
	if out := show(); !strings.Contains(out, " [X] 0001_initial") || !strings.Contains(out, " [X] 0002_rename") {
		t.Fatalf("showmigrations: %s", out)
	}
	sqlOut := run("sqlmigrate", func(out, errb *bytes.Buffer) error {
		return runORMTool(root, orm.CLIRequest{Command: "sql", Dialect: "sqlite", Target: "0002"}, nil, out, errb, nil)
	})
	if !strings.Contains(sqlOut, `ALTER TABLE "users" RENAME COLUMN "email" TO "mail";`) {
		t.Fatalf("sqlmigrate: %s", sqlOut)
	}
	if out := migrate("0001"); !strings.Contains(out, "migration unapplied: 0002_rename") {
		t.Fatalf("migrate 0001: %s", out)
	}
	if out := show(); !strings.Contains(out, " [ ] 0002_rename") {
		t.Fatalf("showmigrations after going back: %s", out)
	}
	migrate("zero")
	if out := show(); !strings.Contains(out, " [ ] 0001_initial") {
		t.Fatalf("showmigrations at zero: %s", out)
	}

	if out := make(makeMigrationsOpts{empty: true, name: "seed"}); !strings.Contains(out, "0003_seed.go") {
		t.Fatalf("--empty: %s", out)
	}
	seed, _ := os.ReadFile(filepath.Join(root, "migrations", "0003_seed.go"))
	if !strings.Contains(string(seed), "m.RunGo{") || !strings.Contains(string(seed), `Dependencies: []string{"0002_rename_field_users_email_to_mail"}`) {
		t.Fatalf("0003_seed.go:\n%s", seed)
	}
	migrate("")
}
