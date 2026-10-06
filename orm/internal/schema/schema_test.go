package schema

import (
	"strings"
	"testing"
)

func quote(s string) string { return `"` + s + `"` }

func TestDiff(t *testing.T) {
	users := Table{Name: "users", Columns: []Column{{Name: "id", Kind: Int, PK: true, Auto: true}, {Name: "email", Kind: String}}}
	posts := Table{Name: "posts", Columns: []Column{{Name: "id", Kind: Int, PK: true, Auto: true}, {Name: "user_id", Kind: Int}},
		FKs: []ForeignKey{{Column: "user_id", Table: "users", Ref: "id", OnDelete: "cascade"}}}
	pg := Dialect{Name: "postgres", Quote: quote}

	steps := pg.Diff(nil, []Table{posts, users})
	if len(steps) != 2 || !strings.Contains(steps[0].SQL, `CREATE TABLE "users"`) || !strings.Contains(steps[1].SQL, "ON DELETE CASCADE") {
		t.Fatalf("create, referenced table first: %+v", steps)
	}

	users2 := users
	users2.Columns = []Column{users.Columns[0], {Name: "email", Kind: String, Unique: true}, {Name: "age", Kind: Int, Nullable: true}, {Name: "name", Kind: String, Index: true}}
	var sqls, notes []string
	for _, s := range pg.Diff([]Table{users, posts}, []Table{users2}) {
		sqls = append(sqls, s.SQL)
		notes = append(notes, s.Note)
	}
	all := strings.Join(sqls, "\n")
	for _, want := range []string{
		`CREATE UNIQUE INDEX "uniq_users_email"`,
		`ALTER TABLE "users" ADD COLUMN "age" BIGINT`,
		`ALTER TABLE "users" ADD COLUMN "name" TEXT NOT NULL`,
		`CREATE INDEX "idx_users_name"`,
		`DROP TABLE "posts"`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("lacks %s:\n%s", want, all)
		}
	}
	if n := strings.Join(notes, "\n"); !strings.Contains(n, "adds NOT NULL column users.name") || !strings.Contains(n, "drops table posts") {
		t.Errorf("notes: %s", n)
	}

	lite := Dialect{Name: "sqlite", Quote: quote}
	changed := users
	changed.Columns = []Column{users.Columns[0], {Name: "email", Kind: Int}}
	if s := lite.Diff([]Table{users}, []Table{changed}); len(s) != 1 || s[0].SQL != "" || !strings.Contains(s[0].Note, "rebuild") {
		t.Fatalf("sqlite type change: %+v", s)
	}
	my := Dialect{Name: "mysql", Quote: func(s string) string { return "`" + s + "`" }}
	if s := my.Diff([]Table{users}, []Table{changed}); len(s) != 1 || !strings.Contains(s[0].SQL, "MODIFY COLUMN `email` BIGINT NOT NULL") {
		t.Fatalf("mysql type change: %+v", s)
	}
	if got := my.Type(Column{Kind: String, Unique: true}); got != "VARCHAR(255)" {
		t.Fatalf("mysql keyed text = %s", got)
	}
}
