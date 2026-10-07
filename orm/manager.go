package orm

import (
	"context"
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
	mirror    string
	unmanaged bool
	bound     atomic.Pointer[binding]
	synced    sync.Map // *sql.DB → true: its key sequence is past the table's keys
	ls        listeners[T]
}

// ForOption tunes a model.
type ForOption func(*forConfig)

type forConfig struct {
	db, table, mirror string
	unmanaged         bool
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

// For is the manager of the model T. Declare it once, package-level, and
// pass it to nexus.Boot (or a module) so the model finds its database and
// a model the ORM can't map fails boot.
func For[T any](opts ...ForOption) *Manager[T] {
	var c forConfig
	for _, o := range opts {
		o(&c)
	}
	m := &Manager[T]{dbName: c.db, mirror: c.mirror, unmanaged: c.unmanaged}
	m.meta, m.err = modelOf(reflect.TypeFor[T](), c.table)
	m.QuerySet = QuerySet[T]{m: m, q: query{m: m.meta}}
	declare(declaredModel{db: c.db, unmanaged: c.unmanaged, tables: m.tables})
	m.Option = nexus.Invoke(func(app *nexus.App, lc nexus.Lifecycle) error {
		if m.err != nil {
			return m.err
		}
		m.route()
		b := bindApp(app, lc)
		m.bound.Store(b)
		lastBinding.Store(b)
		describe(app, m.dbName, m.meta, func() bool { _, ok := rowScanner[T](m.meta); return ok })
		return nil
	})
	return m
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
	if d, ok := ctx.Value(dbKey{}).(*DB); ok {
		return on(ctx, d), nil
	}
	name := m.dbName
	if n, ok := ctx.Value(usingKey{}).(string); ok {
		name = n
	}
	b := ctxBinding(ctx)
	if b == nil {
		b = m.bound.Load()
	}
	if b == nil {
		b = lastBinding.Load()
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
	app *nexus.App
	dbs sync.Map // *sql.DB → *DB
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
