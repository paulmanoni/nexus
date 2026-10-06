package orm_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/db"
	_ "github.com/paulmanoni/nexus/v2/db/sqlite"

	"github.com/paulmanoni/nexus/orm"
)

type Base struct {
	ID        int64 `orm:"pk"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (b *Base) BeforeCreate(context.Context) error {
	creates++
	return nil
}

var creates int

type User struct {
	Base
	Name   string
	Email  string `db:"email"`
	Age    int
	Bio    *string
	Active bool
	Posts  []Post // a relation: not a column
}

type Post struct {
	ID       int64  `gorm:"primaryKey"`
	Title    string `gorm:"column:headline"`
	AuthorID int64
	Draft    bool `gorm:"-"`
}

func (Post) TableName() string { return "articles" }

var (
	Users = orm.For[User]()
	Posts = orm.For[Post]()
)

const schema = `
CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, created_at DATETIME, updated_at DATETIME,
	name TEXT NOT NULL, email TEXT UNIQUE, age INTEGER, bio TEXT, active BOOLEAN);
CREATE TABLE articles (id INTEGER PRIMARY KEY AUTOINCREMENT, headline TEXT, author_id INTEGER REFERENCES users(id));
`

func open(t *testing.T) context.Context {
	t.Helper()
	m, err := db.Open(db.Config{Driver: db.SQLite, Database: ":memory:", LogLevel: "silent"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	s, err := m.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return orm.WithDB(context.Background(), orm.Open(s, "sqlite"))
}

func seed(t *testing.T, ctx context.Context) {
	t.Helper()
	bio := "writes Go"
	rows := []*User{
		{Name: "Ali", Email: "ali@mail.com", Age: 25, Active: true, Bio: &bio},
		{Name: "Neema", Email: "neema@mail.com", Age: 17},
		{Name: "Juma", Email: "juma@mail.com", Age: 32, Active: true},
		{Name: "Amina", Email: "amina@mail.com", Age: 41},
	}
	if err := Users.BulkCreate(ctx, rows); err != nil {
		t.Fatal(err)
	}
	for i, u := range rows {
		if u.ID != int64(i+1) || u.CreatedAt.IsZero() {
			t.Fatalf("row %d after create: %+v", i, u)
		}
	}
}

func names(us []User) []string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.Name
	}
	return out
}

func TestModel(t *testing.T) {
	if got := Users.TableName(); got != "users" {
		t.Fatalf("table = %s", got)
	}
	want := []string{"id", "created_at", "updated_at", "name", "email", "age", "bio", "active"}
	if got := Users.Columns(); !slices.Equal(got, want) {
		t.Fatalf("columns = %v: the embedded Base flattened, the relation left out", got)
	}
	if got := Posts.TableName(); got != "articles" {
		t.Fatalf("TableName ignored: %s", got)
	}
	if got := Posts.Columns(); !slices.Equal(got, []string{"id", "headline", "author_id"}) {
		t.Fatalf("GORM tags: %v", got)
	}
}

func TestQueries(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)

	all, err := Users.OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(names(all), []string{"Ali", "Neema", "Juma", "Amina"}) {
		t.Fatalf("all = %v, %v", names(all), err)
	}
	if all[0].Bio == nil || *all[0].Bio != "writes Go" || all[1].Bio != nil {
		t.Fatalf("nullable: %v %v", all[0].Bio, all[1].Bio)
	}
	adults, err := Users.Filter(orm.Q{"age__gte": 18}).Exclude(orm.Q{"name__icontains": "j"}).OrderBy("-age").All(ctx)
	if err != nil || !slices.Equal(names(adults), []string{"Amina", "Ali"}) {
		t.Fatalf("adults = %v, %v", names(adults), err)
	}
	for _, c := range []struct {
		q    orm.Cond
		want []string
	}{
		{orm.Q{"name": "Juma"}, []string{"Juma"}},
		{orm.Q{"name__iexact": "juma"}, []string{"Juma"}},
		{orm.Q{"name__startswith": "A"}, []string{"Ali", "Amina"}},
		{orm.Q{"name__iendswith": "MA"}, []string{"Neema", "Juma"}},
		{orm.Q{"age__in": []int{17, 41}}, []string{"Neema", "Amina"}},
		{orm.Q{"age__in": []int{}}, []string{}},
		{orm.Q{"age__range": []int{20, 35}}, []string{"Ali", "Juma"}},
		{orm.Q{"bio__isnull": false}, []string{"Ali"}},
		{orm.Q{"bio": nil}, []string{"Neema", "Juma", "Amina"}},
		{orm.Or(orm.Q{"age__lt": 18}, orm.Q{"age__gt": 40}), []string{"Neema", "Amina"}},
		{orm.Not(orm.Q{"active": true}), []string{"Neema", "Amina"}},
		{orm.Q{"Email__contains": "%"}, []string{}},
	} {
		got, err := Users.Filter(c.q).OrderBy("id").All(ctx)
		if err != nil || !slices.Equal(names(got), c.want) {
			t.Errorf("%v: %v, %v", c.q, names(got), err)
		}
	}

	page, _ := Users.OrderBy("id").Offset(1).Limit(2).All(ctx)
	if !slices.Equal(names(page), []string{"Neema", "Juma"}) {
		t.Fatalf("page = %v", names(page))
	}
	if n, err := Users.Filter(orm.Q{"active": true}).Count(ctx); n != 2 || err != nil {
		t.Fatalf("count = %d, %v", n, err)
	}
	if n, _ := Users.OrderBy("id").Limit(3).Count(ctx); n != 3 {
		t.Fatalf("count of a sliced query = %d", n)
	}
	if ok, _ := Users.Filter(orm.Q{"name": "Nobody"}).Exists(ctx); ok {
		t.Fatal("exists")
	}
	first, err := Users.First(ctx)
	if err != nil || first.Name != "Ali" {
		t.Fatalf("first = %v, %v", first.Name, err)
	}
	var seen []string
	for u, err := range Users.OrderBy("-id").Iter(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		if seen = append(seen, u.Name); len(seen) == 2 {
			break
		}
	}
	if !slices.Equal(seen, []string{"Amina", "Juma"}) {
		t.Fatalf("iter = %v", seen)
	}
}

func TestGet(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	u, err := Users.Get(ctx, orm.Q{"email": "juma@mail.com"})
	if err != nil || u.Name != "Juma" || u.ID != 3 {
		t.Fatalf("get = %+v, %v", u, err)
	}
	if _, err := Users.Get(ctx, orm.Q{"email": "nobody@mail.com"}); !errors.Is(err, nexus.NotFound) || err.Error() != "User matching query does not exist" {
		t.Fatalf("missing: %v", err)
	}
	if _, err := Users.Get(ctx, orm.Q{"active": true}); !errors.Is(err, nexus.Conflict) {
		t.Fatalf("two: %v", err)
	}
	if _, err := Users.Filter(orm.Q{"nmae": "x"}).All(ctx); err == nil || err.Error() != `orm: User has no field "nmae"` {
		t.Fatalf("typo: %v", err)
	}
	if _, err := Users.OrderBy("-nmae").All(ctx); err == nil {
		t.Fatal("ordered by a field that isn't there")
	}
}

func TestWrites(t *testing.T) {
	ctx := open(t)
	before := creates
	seed(t, ctx)
	if creates-before != 4 {
		t.Fatalf("BeforeCreate promoted from Base ran %d times", creates-before)
	}

	u, _ := Users.Get(ctx, orm.Q{"name": "Neema"})
	created := u.CreatedAt
	orm.Clock = func() time.Time { return time.Now().Add(time.Hour) }
	t.Cleanup(func() { orm.Clock = time.Now })
	u.Age = 18
	if err := Users.Save(ctx, &u); err != nil {
		t.Fatal(err)
	}
	got, _ := Users.Get(ctx, orm.Q{"id": u.ID})
	if got.Age != 18 || !got.UpdatedAt.After(created) || !got.CreatedAt.Equal(created) {
		t.Fatalf("saved: %+v (created %v)", got, created)
	}

	n, err := Users.Filter(orm.Q{"age__lt": 30}).Update(ctx, orm.Set{"active": true})
	if n != 2 || err != nil {
		t.Fatalf("update = %d, %v", n, err)
	}
	dup := User{Name: "Ali 2", Email: "ali@mail.com"}
	err = Users.Create(ctx, &dup)
	var ne *nexus.Error
	if !errors.As(err, &ne) || ne.Code != nexus.Conflict || len(ne.Fields["email"]) == 0 {
		t.Fatalf("duplicate email: %v", err)
	}

	row, made, err := Users.GetOrCreate(ctx, orm.Q{"email": "zawadi@mail.com"}, orm.Defaults{"name": "Zawadi", "age": 29})
	if err != nil || !made || row.ID == 0 || row.Name != "Zawadi" || row.Age != 29 {
		t.Fatalf("created = %+v %v %v", row, made, err)
	}
	again, made, err := Users.GetOrCreate(ctx, orm.Q{"email": "zawadi@mail.com"}, orm.Defaults{"name": "Other"})
	if err != nil || made || again.ID != row.ID {
		t.Fatalf("got = %+v %v %v", again, made, err)
	}

	if err := Users.Remove(ctx, &again); err != nil {
		t.Fatal(err)
	}
	if err := Users.Remove(ctx, &again); !errors.Is(err, nexus.NotFound) {
		t.Fatalf("removed twice: %v", err)
	}
	if n, err := Users.Filter(orm.Q{"age__lt": 20}).Delete(ctx); n != 1 || err != nil {
		t.Fatalf("delete = %d, %v", n, err)
	}

	post := Post{Title: "Django in Go", AuthorID: 1}
	if err := Posts.Create(ctx, &post); err != nil || post.ID == 0 {
		t.Fatalf("post = %+v, %v", post, err)
	}
}

func TestValuesAndAggregate(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	got, err := Users.OrderBy("age").Values[string]("name").All(ctx)
	if err != nil || !slices.Equal(got, []string{"Neema", "Ali", "Juma", "Amina"}) {
		t.Fatalf("values = %v, %v", got, err)
	}
	type nameAge struct {
		Name string
		Age  int
	}
	pairs, err := Users.Filter(orm.Q{"active": true}).OrderBy("id").Values[nameAge]("name", "age").All(ctx)
	if err != nil || len(pairs) != 2 || pairs[1] != (nameAge{"Juma", 32}) {
		t.Fatalf("pairs = %v, %v", pairs, err)
	}
	r, err := Users.Aggregate(ctx, orm.Count("id"), orm.Avg("age"), orm.Max("age"))
	if err != nil || r.Int("id__count") != 4 || r.Float("age__avg") != 28.75 || r.Int("age__max") != 41 {
		t.Fatalf("aggregate = %v, %v", r, err)
	}
}

func TestAtomic(t *testing.T) {
	ctx := open(t)
	seed(t, ctx)
	boom := errors.New("boom")
	err := orm.Atomic(ctx, func(ctx context.Context) error {
		if _, err := Users.Filter(orm.Q{"name": "Ali"}).Update(ctx, orm.Set{"age": 99}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if u, _ := Users.Get(ctx, orm.Q{"name": "Ali"}); u.Age != 25 {
		t.Fatalf("rolled back? age %d", u.Age)
	}

	err = orm.Atomic(ctx, func(ctx context.Context) error {
		if _, err := Users.Filter(orm.Q{"name": "Ali"}).Update(ctx, orm.Set{"age": 26}); err != nil {
			return err
		}
		inner := orm.Atomic(ctx, func(ctx context.Context) error {
			if _, err := Users.Filter(orm.Q{"name": "Juma"}).Update(ctx, orm.Set{"age": 99}); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(inner, boom) {
			t.Fatalf("savepoint: %v", inner)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ali, _ := Users.Get(ctx, orm.Q{"name": "Ali"})
	juma, _ := Users.Get(ctx, orm.Q{"name": "Juma"})
	if ali.Age != 26 || juma.Age != 32 {
		t.Fatalf("savepoint rolled back alone? ali %d, juma %d", ali.Age, juma.Age)
	}
}

// TestBoot is the ORM inside a nexus app: the model finds the database
// db.Bind registered, with no WithDB. The app's database has no users
// table, which is how the test knows the query went there.
func TestBoot(t *testing.T) {
	_, stop, err := nexus.InProcess(config.Runtime{},
		db.Bind[mainDB]("main", func() db.Config { return db.Config{Driver: db.SQLite, Database: ":memory:", LogLevel: "silent"} }, db.WithDefault()),
		Users,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	ctx := context.Background()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		_, err = Users.Count(ctx)
		if !errors.Is(err, nexus.Unavailable) || time.Now().After(end) {
			break
		}
	}
	if err == nil || !strings.Contains(err.Error(), "no such table: users") {
		t.Fatalf("bound to the app's database? %v", err)
	}
}

type mainDB struct{ *db.Manager }

func TestBadModelFailsBoot(t *testing.T) {
	type twoKeys struct {
		A int64 `orm:"pk"`
		B int64 `orm:"pk"`
	}
	_, stop, err := nexus.InProcess(config.Runtime{}, orm.For[twoKeys]())
	if stop != nil {
		t.Cleanup(func() { _ = stop(context.Background()) })
	}
	if err == nil || !strings.Contains(err.Error(), "two primary keys") {
		t.Fatalf("boot = %v", err)
	}
}

func TestGeneratedScanners(t *testing.T) {
	if !orm.Generated[User]() || !orm.Generated[Post]() {
		t.Skip("no generated scanners: run go run ./cmd/ormgen -tests .")
	}
	type stale struct{ ID int64 }
	if orm.Generated[stale]() {
		t.Fatal("a model with no generated scanner reports one")
	}
}
