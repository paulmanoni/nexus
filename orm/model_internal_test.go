package orm

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/db"
	_ "github.com/paulmanoni/nexus/v2/db/sqlite"

	"github.com/paulmanoni/nexus/orm/internal/schema"
)

// isolate restores the registry when the test ends: a model a test
// registers would otherwise be bound to every app the test binary boots.
func isolate(t *testing.T) {
	t.Helper()
	registry.mu.Lock()
	types, homes := registry.types, slices.Clone(registry.homes)
	registry.types = map[reflect.Type]bool{}
	for k, v := range types {
		registry.types[k] = v
	}
	registry.mu.Unlock()
	t.Cleanup(func() {
		registry.mu.Lock()
		registry.types, registry.homes = types, homes
		registry.mu.Unlock()
	})
}

type gadget struct {
	Model[gadget]
	ID   int64
	Name string
}

// Ledger lives on its own database, by its Meta.
type ledger struct {
	Model[ledger]
	ID     int64
	Amount int64
}

func (ledger) Meta() Meta { return Meta{DB: "ledgers", Table: "ledger_rows"} }

type archived struct {
	Model[archived]
	ID int64
}

func (archived) Meta() Meta { return Meta{DB: "ledgers", Unmanaged: true} }

func TestModelMisuse(t *testing.T) {
	type twice struct {
		Model[twice]
		halfOf[twice]
		ID int64
	}
	type pointer struct {
		*Model[pointer]
		ID int64
	}
	type base struct {
		Model[gadget]
		ID int64
	}
	type inner struct {
		base
		Name string
	}
	// A model embedding another model to add fields isn't one of its own.
	type wider struct {
		gadget
		Extra string
	}
	for _, c := range []struct {
		v    any
		want string
	}{
		{twice{}, "embeds orm.Model 2 times"},
		{pointer{}, "embeds orm.Model through a pointer: embed orm.Model[pointer] by value"},
		{inner{}, "inner embeds orm.Model[gadget]: a model embeds orm.Model of its own type, orm.Model[inner]"},
		{wider{}, "wider embeds orm.Model[gadget]"},
	} {
		_, err := modelOf(reflect.TypeOf(c.v), "", "")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%T: %v, want %q", c.v, err, c.want)
		}
	}
	// Writes report it; boot fails on a registered one.
	if err := (&twice{}).Save(context.Background()); err == nil || !strings.Contains(err.Error(), "2 times") {
		t.Fatalf("Save of a misused model: %v", err)
	}
	isolate(t)
	Register[pointer]()
	_, stop, err := nexus.InProcess(config.Runtime{})
	if err == nil {
		_ = stop(context.Background())
	}
	if err == nil || !strings.Contains(err.Error(), "through a pointer") {
		t.Fatalf("boot with a misused model registered: %v", err)
	}
	// A struct reading values, never a model, may hold one.
	if _, err := modelOf(reflect.TypeFor[wider](), "", "-"); err != nil {
		t.Fatal(err)
	}
}

// TestRegisterBoot boots an app given no model: the registered one is
// bound to its database all the same.
func TestRegisterBoot(t *testing.T) {
	isolate(t)
	Register[gadget]()
	var mgr *db.Manager
	_, stop, err := nexus.InProcess(config.Runtime{},
		db.Bind[testDB]("main", func() db.Config { return db.Config{Driver: db.SQLite, Database: ":memory:", LogLevel: "silent"} }, db.WithDefault()),
		nexus.Invoke(func(m *testDB) { mgr = m.Manager }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	for end := time.Now().Add(5 * time.Second); !mgr.IsConnected(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("no database")
		}
	}
	ctx := context.Background()
	if err := CreateTables(ctx, Objects[gadget]()); err != nil {
		t.Fatal(err)
	}
	g := gadget{Name: "lamp"}
	if err := g.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := Objects[gadget]().Count(ctx); err != nil || n != 1 {
		t.Fatalf("%d, %v", n, err)
	}
}

type testDB struct{ *db.Manager }

// TestRegisterPlan: a registered model is planned on its Meta's database
// with no orm.For, and an unmanaged one isn't.
func TestRegisterPlan(t *testing.T) {
	isolate(t)
	Register[ledger]()
	Register[archived]()
	st, err := declaredState([]string{"ledgers"}, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tb := range st.Tables() {
		names = append(names, tb.Name)
	}
	if !slices.Contains(names, "ledger_rows") || slices.Contains(names, "archiveds") {
		t.Fatalf("the tables of database ledgers: %v", names)
	}
	if st, _ := declaredState([]string{""}, "sqlite"); slices.ContainsFunc(st.Tables(), func(tb schema.Table) bool { return tb.Name == "ledger_rows" }) {
		t.Fatal("a model of database ledgers planned on the default")
	}
}

// halfOf embeds a Model for the model embedding it.
type halfOf[T any] struct{ Model[T] }

type routed struct {
	Model[routed]
	ID int64
}

func (routed) Meta() Meta { return Meta{DB: "routes"} }

type indexed struct {
	ID    int64
	Title string
}

func (indexed) Indexes() []Index { return []Index{GinIndex("title")} }

func (indexed) Meta() Meta { return Meta{Indexes: []Index{FullTextIndex("title")}} }

func TestMetaPrecedence(t *testing.T) {
	m, err := modelOf(reflect.TypeFor[indexed](), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.indexes) != 1 || m.indexes[0].kind != "fulltext" {
		t.Fatalf("Meta's indexes over Indexes(): %+v", m.indexes)
	}
	// For's options over Meta.
	if got := For[routed](On("elsewhere"), Table("t")); got.dbName != "elsewhere" || got.TableName() != "t" {
		t.Fatalf("For over Meta: %s %s", got.dbName, got.TableName())
	}
	if got := Of[routed](Schema{}); got.dbName != "routes" {
		t.Fatalf("Of a schema of the default database: %s", got.dbName)
	}
}

// TestGormTagPrecedence: gorm:"-" hides a field from GORM, not from the
// ORM when its own tag names it.
func TestGormTagPrecedence(t *testing.T) {
	type person struct {
		ID        int64
		FirstName string `gorm:"-" orm:"profile__first_name" legacy:"name"`
		Nick      string `gorm:"-" orm:"column:nick"`
		Hidden    string `gorm:"-"`
		Profile   *struct {
			ID        int64
			FirstName string
		}
	}
	m, err := modelOf(reflect.TypeFor[person](), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := m.field("FirstName"); !ok || f.Via != "profile__first_name" {
		t.Fatalf("default names: %+v", f)
	}
	if f, ok := m.field("nick"); !ok || f.Column != "nick" {
		t.Fatalf("a gorm:\"-\" column the ORM names: %+v", f)
	}
	if _, ok := m.field("hidden"); ok {
		t.Fatal("gorm:\"-\" alone keeps the field")
	}
	lm, err := modelOf(reflect.TypeFor[person](), "legacy", "")
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := lm.field("FirstName"); !ok || f.Column != "name" || f.Via != "" {
		t.Fatalf("legacy names: %+v", f)
	}
}

// TestModelGorm: GORM reads and writes a model embedding Model, which it
// ignores, and the ORM reads GORM's rows.
func TestModelGorm(t *testing.T) {
	isolate(t)
	m, err := db.Open(db.Config{Driver: db.SQLite, Database: ":memory:", LogLevel: "silent"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	g := m.GetDB()
	if err := g.AutoMigrate(&gadget{}); err != nil {
		t.Fatal(err)
	}
	if err := g.Create(&gadget{Name: "lamp"}).Error; err != nil {
		t.Fatal(err)
	}
	var back gadget
	if err := g.First(&back).Error; err != nil || back.Name != "lamp" {
		t.Fatalf("%+v, %v", back, err)
	}
	cols, err := g.Migrator().ColumnTypes(&gadget{})
	if err != nil || len(cols) != 2 {
		t.Fatalf("GORM's columns: %d, %v", len(cols), err)
	}
	s, _ := g.DB()
	ctx := WithDB(context.Background(), Open(s, "sqlite"))
	got, err := Objects[gadget]().Get(ctx, Q{"name": "lamp"})
	if err != nil {
		t.Fatal(err)
	}
	got.Name = "desk"
	if err := got.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.First(&back).Error; err != nil || back.Name != "desk" {
		t.Fatalf("%+v, %v", back, err)
	}
}

// TestVerify checks a schema an app picks at run time.
func TestVerify(t *testing.T) {
	type owner struct {
		ID   int64
		Name string
	}
	type misfit struct {
		ID      int64
		OwnerID int64
		Owner   *owner `gorm:"foreignKey:OwnerID;references:Nope"`
	}
	app, stop, err := nexus.InProcess(config.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	ctx := context.Background()
	if err := (Schema{}).Verify(ctx, app, owner{}, &gadget{}); err != nil {
		t.Fatal(err)
	}
	if err := (Schema{}).Verify(ctx, app, misfit{}); err == nil || !strings.Contains(err.Error(), `misfit.Owner: owner has no field "Nope"`) {
		t.Fatalf("Verify: %v", err)
	}
}
