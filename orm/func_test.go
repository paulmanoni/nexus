package orm_test

import (
	"slices"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/orm"
)

var (
	// Prefix is a function of the app's own: SQL of its own on every
	// database.
	Prefix = orm.Function("prefix", orm.Template("", "substr({0}, 1, {1})"))
	// Initial is one used as a Q key step: Q{"name__initial": "A"}.
	Initial = orm.Function("initial", orm.Template("", "substr({0}, 1, 1)")).Transform()
	// GroupConcat is an aggregate SQLite and MySQL have.
	GroupConcat = orm.Function("group_concat")
)

type UserStats struct {
	ID      int64
	Name    string
	NameLen int    `orm:"computed"`
	Short   string `orm:"computed"`
}

var Stats = orm.For[UserStats](orm.Table("users"))

func TestFunctions(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)

	got, err := Users.Filter(orm.Where(Prefix.Of(orm.F("name"), 2), "exact", "Am")).All(ctx)
	if err != nil || !slices.Equal(names(got), []string{"Amina"}) {
		t.Fatalf("custom function = %v, %v", names(got), err)
	}
	got, err = Users.Filter(orm.Q{"name__initial": "A"}).OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(names(got), []string{"Ali", "Amina"}) {
		t.Fatalf("custom transform = %v, %v", names(got), err)
	}
	got, err = Users.Filter(orm.Q{"name__lower__startswith": "ne"}).All(ctx)
	if err != nil || !slices.Equal(names(got), []string{"Neema"}) {
		t.Fatalf("lower transform = %v, %v", names(got), err)
	}
	got, err = Users.Filter(orm.Q{"created_at__year": time.Now().Year()}).All(ctx)
	if err != nil || len(got) != 4 {
		t.Fatalf("year transform = %v, %v", names(got), err)
	}
	got, err = Users.Filter(orm.Q{"name__length__gte": 5}).OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(names(got), []string{"Neema", "Amina"}) {
		t.Fatalf("length transform = %v, %v", names(got), err)
	}

	long, err := Users.Annotate("name_len", orm.Length.Of(orm.F("name"))).
		Filter(orm.Q{"name_len__gte": 4}).OrderBy("-name_len", "name").Values[string]("name").All(ctx)
	if err != nil || !slices.Equal(long, []string{"Amina", "Neema", "Juma"}) {
		t.Fatalf("annotate = %v, %v", long, err)
	}
	type nameLen struct {
		Name    string
		NameLen int
	}
	pairs, err := Users.Annotate("name_len", orm.Length.Of(orm.F("name"))).OrderBy("id").Values[nameLen]("name", "name_len").All(ctx)
	if err != nil || len(pairs) != 4 || pairs[0] != (nameLen{"Ali", 3}) {
		t.Fatalf("annotation in Values = %v, %v", pairs, err)
	}

	stats, err := Stats.Annotate("name_len", orm.Length.Of(orm.F("name"))).
		Annotate("short", Prefix.Of(orm.F("name"), 2)).OrderBy("id").All(ctx)
	if err != nil || len(stats) != 4 || stats[1].NameLen != 5 || stats[1].Short != "Ne" {
		t.Fatalf("computed fields = %+v, %v", stats, err)
	}
	if plain, _ := Stats.OrderBy("id").First(ctx); plain.NameLen != 0 {
		t.Fatalf("computed field filled with no annotation: %+v", plain)
	}

	if n, err := Users.Filter(orm.Q{"age__lt": 30}).Update(ctx, orm.Set{"age": orm.SQL("{0} + {1}", orm.F("age"), 10)}); n != 2 || err != nil {
		t.Fatalf("update with an expression = %d, %v", n, err)
	}
	ali, _ := Users.Get(ctx, orm.Q{"name": "Ali"})
	if ali.Age != 35 {
		t.Fatalf("age = %d", ali.Age)
	}
	got, err = Users.Filter(orm.Q{"age__gt": orm.F("id")}).All(ctx)
	if err != nil || len(got) != 4 {
		t.Fatalf("column against column = %v, %v", names(got), err)
	}

	r, err := Users.Filter(orm.Q{"active": true}).Aggregate(ctx, orm.AggOf("names", GroupConcat.Of(orm.F("name"), ", ")), orm.Count("id"))
	if err != nil || r["names"] != "Ali, Juma" || r.Int("id__count") != 2 {
		t.Fatalf("custom aggregate = %v, %v", r, err)
	}
}
