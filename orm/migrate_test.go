package orm_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/db"

	"github.com/paulmanoni/nexus/orm"
	"github.com/paulmanoni/nexus/orm/ormtest"
)

func TestMigrations(t *testing.T) {
	ctx := ormtest.Open(t)
	db, _ := orm.DBFrom(ctx)
	driver := db.Dialect().Name()

	plan, err := orm.PlanMigration(nil, driver)
	if err != nil {
		t.Fatal(err)
	}
	src := string(orm.MigrationFile(plan.Steps))
	for _, want := range []string{"CREATE TABLE", "users", "book_tags"} {
		if !strings.Contains(src, want) {
			t.Fatalf("initial migration lacks %q:\n%s", want, src)
		}
	}
	fsys := fstest.MapFS{"0001_initial.sql": {Data: []byte(src)}}
	applied, err := orm.ApplyMigrations(ctx, db, fsys)
	if err != nil || !slices.Equal(applied, []string{"0001_initial.sql"}) {
		t.Fatalf("applied %v, %v", applied, err)
	}
	if err := Users.Create(ctx, &User{Name: "Ali", Email: "ali@x"}); err != nil {
		t.Fatal(err)
	}
	if applied, err = orm.ApplyMigrations(ctx, db, fsys); err != nil || len(applied) != 0 {
		t.Fatalf("applied twice: %v, %v", applied, err)
	}

	again, err := orm.PlanMigration(plan.Snapshot, driver)
	if err != nil || len(again.Steps) != 0 {
		t.Fatalf("a plan from the models' own snapshot: %+v, %v", again.Steps, err)
	}

	fsys["0002_bad.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE extra (id INTEGER PRIMARY KEY);\nSELECT * FROM nothing_here;")}
	if _, err := orm.ApplyMigrations(ctx, db, fsys); err == nil || !strings.Contains(err.Error(), "0002_bad.sql") {
		t.Fatalf("a failing migration: %v", err)
	}
	// MySQL commits DDL as it runs: the table the failed attempt made stays.
	fsys["0002_bad.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE IF NOT EXISTS extra (id INTEGER PRIMARY KEY);")}
	if applied, err = orm.ApplyMigrations(ctx, db, fsys); err != nil || len(applied) != 1 {
		t.Fatalf("after fixing it: %v, %v", applied, err)
	}
	fsys["2_x.sql"] = &fstest.MapFile{Data: []byte("--")}
	fsys["bad name.sql"] = &fstest.MapFile{Data: []byte("--")}
	if _, err := orm.ApplyMigrations(ctx, db, fsys); err == nil {
		t.Fatal("took a migration named outside NNNN_words.sql")
	}
}

func TestMigrateAtBoot(t *testing.T) {
	plan, err := orm.PlanMigration(nil, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{"migrations/0001_initial.sql": {Data: orm.MigrationFile(plan.Steps)}}
	_, stop, err := nexus.InProcess(config.Runtime{},
		db.Bind[mainDB]("main", func() db.Config { return db.Config{Driver: db.SQLite, Database: ":memory:", LogLevel: "silent"} }, db.WithDefault()),
		orm.Migrate(fsys, orm.MigrateDir("migrations")),
		Users,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	if err := Users.Create(context.Background(), &User{Name: "Ali", Email: "ali@x"}); err != nil {
		t.Fatalf("the migrated table: %v", err)
	}
}

// TestSearchMigrations plans Doc's generated column, vector and indexes,
// and the extension declared for them (search_test.go).
func TestSearchMigrations(t *testing.T) {
	plan, err := orm.PlanMigration(nil, "postgres")
	if err != nil || plan.Steps[0].SQL != `CREATE EXTENSION IF NOT EXISTS "vector"` || !strings.Contains(string(plan.Snapshot), `"extensions": [`) {
		t.Fatalf("plan %v, %v", plan.Steps, err)
	}
	src := string(orm.MigrationFile(plan.Steps))
	for _, want := range []string{`"search" tsvector GENERATED ALWAYS AS`, `"embedding" vector(3)`, `USING hnsw ("embedding" vector_cosine_ops)`} {
		if !strings.Contains(src, want) {
			t.Fatalf("migration lacks %q", want)
		}
	}
	if again, err := orm.PlanMigration(plan.Snapshot, "postgres"); err != nil || len(again.Steps) != 0 {
		t.Fatalf("again %+v, %v", again.Steps, err)
	}
	lite, err := orm.PlanMigration(nil, "sqlite")
	if err != nil || strings.Contains(string(orm.MigrationFile(lite.Steps)), "EXTENSION") {
		t.Fatalf("sqlite %v", err)
	}
	ctx := ormtest.Open(t)
	db, _ := orm.DBFrom(ctx)
	if _, err := orm.ApplyMigrations(ctx, db, fstest.MapFS{"0001_initial.sql": {Data: orm.MigrationFile(lite.Steps)}}); err != nil {
		t.Fatal(err)
	}
	if err := Docs.Create(ctx, &Doc{Title: "a", Body: "b"}); err != nil {
		t.Fatalf("the migrated table: %v", err)
	}
}
