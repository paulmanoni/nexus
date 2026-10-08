package orm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/db"
)

// Manager is a model's entry point, Django's Model.objects: a QuerySet of
// every row (its methods are the QuerySet's), the writes of single rows,
// and a nexus.Option that binds the model to the app's database when
// passed to nexus.Boot.
//
//	var Users = orm.For[User]()
//
//	nexus.Boot(db.Bind[MainDB]("main", …, db.WithDefault()), Users)
type Manager[T any] struct {
	nexus.Option
	QuerySet[T]

	meta      *model
	err       error // what is wrong with the model, reported at boot
	dbName    string
	names     string
	mirror    string
	unmanaged bool
	bound     atomic.Pointer[binding]
	synced    sync.Map // *sql.DB → true: its key sequence is past the table's keys
	ls        listeners[T]
}

// ForOption tunes a model.
type ForOption func(*forConfig)

type forConfig struct {
	db, table, mirror, names string
	unmanaged                bool
}

// On binds the model to the database db.Bind registered as name; the
// default database otherwise.
func On(name string) ForOption { return func(c *forConfig) { c.db = name } }

// Table names the model's table, over TableName and the plural of the
// type's name.
func Table(name string) ForOption { return func(c *forConfig) { c.table = name } }

// Unmanaged marks the model's table as the database's own: never created
// or migrated by the ORM. Every legacy table is one.
func Unmanaged() ForOption { return func(c *forConfig) { c.unmanaged = true } }

// Names reads the model under a names set (see Schema): each field's tag
// of that key over its default naming, and <Names>TableName (names
// "legacy": LegacyTableName) over TableName.
func Names(set string) ForOption { return func(c *forConfig) { c.names = set } }

// For is the manager of the model T. Declare it once, package-level, and
// pass it to nexus.Boot (or a module) so the model finds its database and
// a model the ORM can't map fails boot. Its options win over the model's
// Meta. The first For of a model without Names is its own manager, the
// one Objects and its rows' writes use.
func For[T any](opts ...ForOption) *Manager[T] {
	var c forConfig
	for _, o := range opts {
		o(&c)
	}
	m := newManager[T](c)
	if c.names == "" {
		t := reflect.TypeFor[T]()
		// A For replaces the manager Objects made before it ran (Objects
		// called during another package's init).
		if v, loaded := homes.LoadOrStore(t, homeEntry{m: m}); loaded && v.(homeEntry).made {
			homes.CompareAndSwap(t, v, homeEntry{m: m})
		}
	}
	return m
}

func newManager[T any](c forConfig) *Manager[T] {
	m := &Manager[T]{mirror: c.mirror, names: c.names}
	m.meta, m.err = modelOf(reflect.TypeFor[T](), c.names, c.table)
	var meta Meta
	if m.meta != nil {
		meta = m.meta.meta
	}
	m.dbName, m.unmanaged = cmp.Or(c.db, meta.DB), c.unmanaged || meta.Unmanaged
	m.QuerySet = QuerySet[T]{m: m, q: query{m: m.meta}}
	m.Option = nexus.Invoke(func(app *nexus.App, lc nexus.Lifecycle) error {
		if m.err != nil {
			return m.err
		}
		b := bindApp(app, lc)
		// nexus.toml's route is this app's: kept on its binding, never on
		// the manager, which outlives the app and may serve others.
		if r, ok := m.route(); ok {
			b.routes.Store(m, r)
		}
		m.bound.Store(b)
		lastBinding.Store(b)
		lc.Append(nexus.Hook{OnStop: func(context.Context) error {
			m.bound.CompareAndSwap(b, nil)
			lastBinding.CompareAndSwap(b, nil)
			return nil
		}})
		describe(app, m.routeOn(b).DB, m.meta, func() bool { _, ok := rowScanner[T](m.meta); return ok })
		return nil
	})
	return m
}

func (m *Manager[T]) option() nexus.Option { return m.Option }

func (m *Manager[T]) declaration() declaredModel {
	return declaredModel{model: m.meta == nil || m.meta.state != nil, db: m.dbName, unmanaged: m.unmanaged, tables: m.tables, madeOn: m.madeOn}
}

// schema is the schema the manager queries, on the app it was last bound
// to.
func (m *Manager[T]) schema() Schema {
	return Schema{DB: m.routeOn(m.bound.Load()).DB, Names: m.names, Unmanaged: m.unmanaged}
}

// routeOn is the model's databases on the app b binds: nexus.toml's route
// there, else its own (For's or Meta's).
func (m *Manager[T]) routeOn(b *binding) Route {
	if b != nil {
		if r, ok := b.routes.Load(m); ok {
			return r.(Route)
		}
	}
	return Route{DB: m.dbName, Mirror: m.mirror}
}

// Schema is a database the models are queried on and the names they have
// there: one set of Go models, relations declared on them, over schemas
// that name their tables and columns apart.
//
//	var Legacy = orm.Schema{DB: "legacy", Names: "legacy", Unmanaged: true}
//
//	type User struct {
//		ID      int64
//		Phone   string   `legacy:"tel_no"`              // the column there
//		Bio     string   `legacy:"-"`                   // no such column there
//		Code    string   `gorm:"-" legacy:"code"`       // there only
//		City    string   `legacy:"profile__city"`       // read through a relation there
//		Profile *Profile `gorm:"foreignKey:UserID"`
//		Groups  []Group  `gorm:"many2many:user_groups" legacy:"many2many:auth_user_groups;joinReferences:group_id"`
//	}
//	func (User) LegacyTableName() string { return "auth_user" }
//
//	users, err := orm.Of[User](Legacy).Filter(orm.Q{"groups__name": "staff"}).All(ctx)
//
// Queries name Go fields, default names and relation paths whatever the
// schema; the ORM writes them in the schema's names. A field read through
// a relation is read-only there: Create and Save skip it, and Update
// refuses it.
type Schema struct {
	DB        string // the database db.Bind registered; "" the default
	Names     string // the names set; "" the default naming
	Unmanaged bool   // the tables are the database's own: never migrated
}

type schemaKey struct {
	t reflect.Type
	s Schema
}

var schemaManagers sync.Map // schemaKey → *Manager[T]

// Of is the manager of the model T on schema s, made once per model and
// schema. It finds its database as For's managers do: the app serving the
// request, or the one a manager was last bound to.
func Of[T any](s Schema) *Manager[T] {
	k := schemaKey{reflect.TypeFor[T](), s}
	if v, ok := schemaManagers.Load(k); ok {
		return v.(*Manager[T])
	}
	c := forConfig{db: s.DB, names: s.Names, unmanaged: s.Unmanaged}
	v, _ := schemaManagers.LoadOrStore(k, newManager[T](c))
	return v.(*Manager[T])
}

// Check fails boot when one of the models (values or pointers of their
// types) doesn't map onto the schema, as Verify finds; once its database
// connects, boot waits for it. Pass it after orm.Migrate, which may
// install what it checks for.
//
//	nexus.Boot(…, Legacy.Check(User{}, Profile{}, Group{}))
func (s Schema) Check(models ...any) nexus.Option {
	return nexus.Setup(func(ctx context.Context, app *nexus.App) error {
		return s.Verify(ctx, app, models...)
	})
}

// Verify is what Check checks, for a schema an app picks at run time:
// that the models (values or pointers of their types) map onto it — no
// tag naming no field, relation to a model by no field of it, column read
// through no relation, or two relations claiming one inverse name — and,
// once its database answers (it waits for it to connect, as Migrate
// does), that the database can make their columns, indexes and generated
// columns (a vector on MySQL can't be) and has their extensions.
//
//	nexus.Setup(func(ctx context.Context, app *nexus.App, cfg *Config) error {
//		return schemaFor(cfg).Verify(ctx, app, User{}, Group{})
//	})
func (s Schema) Verify(ctx context.Context, app *nexus.App, models ...any) error {
	var errs []error
	var ms []*model
	for _, v := range models {
		t := reflect.TypeOf(v)
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		m, err := modelOf(t, s.Names, "")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ms = append(ms, m)
		errs = append(errs, m.check()...)
	}
	d, err := waitConnected(ctx, &binding{app: app}, s.DB)
	switch {
	case errors.Is(err, nexus.Unavailable):
		errs = append(errs, err)
	case err == nil:
		var exts []string
		for _, m := range ms {
			if err := m.madeOn(d.dialect); err != nil {
				errs = append(errs, err)
			}
			exts = append(exts, m.extensions()...)
		}
		slices.Sort(exts)
		errs = append(errs, missingExtensions(ctx, d, slices.Compact(exts)))
	}
	return errors.Join(errs...)
}

// TableName is the model's table.
func (m *Manager[T]) TableName() string {
	if m.meta == nil {
		return ""
	}
	return m.meta.Table
}

// ColumnValues is row's columns and their values as the database is sent
// them (a nil pointer as nil): a snapshot of the row for code that speaks
// columns, such as a change feed to another system.
func (m *Manager[T]) ColumnValues(row *T) map[string]any {
	if m.meta == nil || row == nil {
		return nil
	}
	v := reflect.ValueOf(row).Elem()
	out := make(map[string]any, len(m.meta.Fields))
	for _, f := range m.meta.Fields {
		out[f.Column] = value(peek(v, f.Index, f.Type))
	}
	return out
}

// PKColumn is the column of the model's primary key, "" for none.
func (m *Manager[T]) PKColumn() string {
	if m.meta == nil || m.meta.PK == nil {
		return ""
	}
	return m.meta.PK.Column
}

// Columns is the model's columns, in field order.
func (m *Manager[T]) Columns() []string {
	if m.meta == nil {
		return nil
	}
	return m.meta.columns()
}

// conn is the database ctx's queries of the model go to.
func (m *Manager[T]) conn(ctx context.Context) (conn, error) {
	if m.err != nil {
		return conn{}, m.err
	}
	if t, ok := ctx.Value(inMirrorKey{}).(mirrorTarget); ok {
		return m.mirrorConn(ctx, t)
	}
	b := pickBinding(ctx, m.bound.Load())
	return dbConnOn(ctx, m.routeOn(b).DB, b)
}

// dbConn is the database name (bound, else on the app a manager was last
// bound to) as ctx sees it: WithDB's, Using's, or the one of the app
// serving the request.
func dbConn(ctx context.Context, name string, bound *binding) (conn, error) {
	return dbConnOn(ctx, name, pickBinding(ctx, bound))
}

// pickBinding is the app ctx's queries go to: the one serving its
// request, else bound, else the one a manager was last bound to.
func pickBinding(ctx context.Context, bound *binding) *binding {
	if b := ctxBinding(ctx); b != nil {
		return b
	}
	if bound != nil {
		return bound
	}
	return lastBinding.Load()
}

// dbConnOn is database name on the app b binds, as ctx sees it.
func dbConnOn(ctx context.Context, name string, b *binding) (conn, error) {
	if d, ok := ctx.Value(dbKey{}).(*DB); ok {
		return on(ctx, d), nil
	}
	if n, ok := ctx.Value(usingKey{}).(string); ok {
		name = n
	}
	if b == nil {
		return conn{}, errNoDB
	}
	d, err := b.lookup(name)
	if err != nil {
		return conn{}, err
	}
	return on(ctx, d), nil
}

// binding is a model bound to an app: its databases are found by name.
type binding struct {
	app    *nexus.App
	dbs    sync.Map // *sql.DB → *DB
	routes sync.Map // *Manager[T] → Route: nexus.toml's for the app
}

// appBindings is the binding of each app, made once: SetRequestValue
// puts it on the app's request contexts, so a manager passed to two apps
// in one process (tests, a gateway) queries the database of the app
// serving the request.
var appBindings sync.Map // *nexus.App → *binding

type bindingKey struct{}

// bindApp is app's binding.
func bindApp(app *nexus.App, lc nexus.Lifecycle) *binding {
	v, loaded := appBindings.LoadOrStore(app, &binding{app: app})
	if !loaded {
		app.SetRequestValue(bindingKey{}, v)
		lc.Append(nexus.Hook{OnStop: func(context.Context) error {
			appBindings.Delete(app)
			return nil
		}})
	}
	return v.(*binding)
}

// ctxBinding is the binding of the app serving ctx's request, or nil.
func ctxBinding(ctx context.Context) *binding {
	b, _ := ctx.Value(bindingKey{}).(*binding)
	return b
}

// lastBinding is the app a manager was last bound to: the database of
// Atomic, and of models not passed to Boot themselves.
var lastBinding atomic.Pointer[binding]

func (b *binding) lookup(name string) (*DB, error) {
	mgr, ok := db.Lookup(b.app, name)
	if !ok {
		if name == "" {
			return nil, fmt.Errorf("orm: no default database: mark one db.Bind with db.WithDefault()")
		}
		return nil, fmt.Errorf("orm: no database %q: bind it with db.Bind", name)
	}
	g := mgr.GetDB()
	if g == nil {
		return nil, nexus.Errf(nexus.Unavailable, "database %q is not connected", name)
	}
	s, err := g.DB()
	if err != nil {
		return nil, err
	}
	if d, ok := b.dbs.Load(s); ok {
		return d.(*DB), nil
	}
	d, _ := b.dbs.LoadOrStore(s, Open(s, string(mgr.Driver())))
	return d.(*DB), nil
}

// appModels is the models bound to an app, by database name ("" the
// default), for the dashboard.
type appModels struct {
	mu   sync.Mutex
	byDB map[string][]modelEntry
}

type modelEntry struct {
	m         *model
	generated func() bool
}

type appModelsKey struct{}

// describe lists m on its database's dashboard entry: its name, table,
// and whether a generated scanner reads it.
func describe(app *nexus.App, dbName string, m *model, generated func() bool) {
	v, _ := app.Value(appModelsKey{})
	am, ok := v.(*appModels)
	if !ok {
		am = &appModels{byDB: map[string][]modelEntry{}}
		app.SetValue(appModelsKey{}, am)
	}
	am.mu.Lock()
	if !slices.ContainsFunc(am.byDB[dbName], func(e modelEntry) bool { return e.m == m }) {
		am.byDB[dbName] = append(am.byDB[dbName], modelEntry{m, generated})
	}
	am.mu.Unlock()
	db.Describe(app, dbName, "models", func() any {
		am.mu.Lock()
		defer am.mu.Unlock()
		var parts []string
		for _, e := range am.byDB[dbName] {
			s := e.m.Name + " (" + e.m.Table
			if e.generated() {
				s += ", generated scanner"
			}
			parts = append(parts, s+")")
		}
		return strings.Join(parts, ", ")
	})
}
