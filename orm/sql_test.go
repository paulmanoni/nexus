package orm

import (
	"reflect"
	"testing"
)

type SQLBase struct {
	ID int64 `orm:"pk"`
}

type sqlUser struct {
	*SQLBase
	Name string
	Age  int
}

func TestSQL(t *testing.T) {
	m := For[sqlUser](Table("users"))
	if m.err != nil {
		t.Fatal(m.err)
	}
	q := m.Filter(Q{"age__gte": 18, "name__icontains": "a_%"}).Exclude(Q{"id__in": []int{1, 2}}).OrderBy("-age").Limit(10).Offset(20)
	for _, c := range []struct {
		d    Dialect
		want string
	}{
		{postgres{}, `SELECT "users"."id", "users"."name", "users"."age" FROM "users" WHERE (("users"."age" >= $1 AND "users"."name" ILIKE $2 ESCAPE '!') AND NOT ("users"."id" IN ($3, $4))) ORDER BY "users"."age" DESC LIMIT 10 OFFSET 20`},
		{mysql{}, "SELECT `users`.`id`, `users`.`name`, `users`.`age` FROM `users` WHERE ((`users`.`age` >= ? AND LOWER(`users`.`name`) LIKE LOWER(?) ESCAPE '!') AND NOT (`users`.`id` IN (?, ?))) ORDER BY `users`.`age` DESC LIMIT 10 OFFSET 20"},
	} {
		b := q.q.builder(c.d)
		cols, _ := q.q.columns(b, nil)
		got, err := q.q.selectSQL(b, cols)
		if err != nil || got != c.want {
			t.Errorf("%s:\n got %s\nwant %s (%v)", c.d.Name(), got, c.want, err)
		}
		if want := []any{18, "%a!_!%%", 1, 2}; !reflect.DeepEqual(b.args(), want) {
			t.Errorf("%s args = %v, want %v", c.d.Name(), b.args(), want)
		}
	}
	b := newBuilder(mysql{}, m.meta)
	if got := m.Offset(5).q.pageSQL(b); got != " LIMIT 18446744073709551615 OFFSET 5" {
		t.Errorf("mysql offset alone: %q", got)
	}
}

func TestPointerEmbed(t *testing.T) {
	m, err := modelOf(reflect.TypeFor[sqlUser](), "")
	if err != nil || m.PK == nil || m.PK.Column != "id" {
		t.Fatalf("model = %+v, %v", m, err)
	}
	var u sqlUser
	if err := (&cell{fieldOf(reflect.ValueOf(&u).Elem(), m.PK.Index)}).Scan(int64(7)); err != nil || u.SQLBase == nil || u.ID != 7 {
		t.Fatalf("scan into a nil *Base: %+v, %v", u, err)
	}
	var none sqlUser
	if got := value(peek(reflect.ValueOf(&none).Elem(), m.PK.Index, m.PK.Type)); got != int64(0) {
		t.Fatalf("read through a nil *Base = %v", got)
	}
}

func TestViolations(t *testing.T) {
	for _, c := range []struct {
		d    Dialect
		msg  string
		kind violation
		col  string
	}{
		{postgres{}, `ERROR: duplicate key value violates unique constraint "users_email_key" (SQLSTATE 23505) Key (email)=(a@b.c) already exists.`, uniqueViolation, "email"},
		{mysql{}, `Error 1062 (23000): Duplicate entry 'a@b.c' for key 'users.email'`, uniqueViolation, "email"},
		{sqlite{}, `constraint failed: UNIQUE constraint failed: users.email (2067)`, uniqueViolation, "email"},
		{postgres{}, `ERROR: insert or update on table "posts" violates foreign key constraint (SQLSTATE 23503)`, foreignKeyViolation, ""},
	} {
		kind, col := c.d.Violation(errString(c.msg))
		if kind != c.kind || col != c.col {
			t.Errorf("%s %q: %v %q", c.d.Name(), c.msg, kind, col)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

type hidden struct{ ID int64 }

type hiddenPtr struct {
	*hidden
	Name string
}

func TestUnexportedPointerEmbed(t *testing.T) {
	if _, err := modelOf(reflect.TypeFor[hiddenPtr](), ""); err == nil {
		t.Fatal("an unexported *embed was accepted")
	}
}

func TestFunctionSQL(t *testing.T) {
	m := For[sqlUser](Table("users"))
	for _, c := range []struct {
		d    Dialect
		want string
	}{
		{postgres{}, `SELECT "id" FROM "users" WHERE CAST(EXTRACT(YEAR FROM ("created")) AS INTEGER) >= $1`},
		{mysql{}, "SELECT `id` FROM `users` WHERE year((`created`)) >= ?"},
		{sqlite{}, `SELECT "id" FROM "users" WHERE CAST(strftime('%Y', ("created")) AS INTEGER) >= ?`},
	} {
		b := newBuilder(c.d, m.meta)
		b.ann = map[string]Expr{"created": rawSQL(c.d.Quote("created"))}
		w, err := m.Filter(Q{"created__year__gte": 2026}).q.whereSQL(b)
		if got := `SELECT ` + c.d.Quote("id") + ` FROM ` + c.d.Quote("users") + w; err != nil || got != c.want {
			t.Errorf("%s:\n got %s\nwant %s (%v)", c.d.Name(), got, c.want, err)
		}
	}
	b := newBuilder(postgres{}, m.meta)
	s, err := SQL("{0} || '{{x}}' || {1}", F("name"), "!").exprSQL(b)
	if err != nil || s != `"users"."name" || '{x}' || $1` || b.args()[0] != "!" {
		t.Fatalf("template = %s %v %v", s, b.args(), err)
	}
	if _, err := SQL("{2}", 1).exprSQL(b); err == nil {
		t.Fatal("a missing argument was accepted")
	}
}
