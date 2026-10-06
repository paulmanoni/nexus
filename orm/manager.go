package orm

import (
	"context"
	"fmt"
	"reflect"
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
	unmanaged bool
	bound     atomic.Pointer[binding]
}

// ForOption tunes a model.
type ForOption func(*forConfig)

type forConfig struct {
	db, table string
	unmanaged bool
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
	m := &Manager[T]{dbName: c.db, unmanaged: c.unmanaged}
	m.meta, m.err = modelOf(reflect.TypeFor[T](), c.table)
	m.QuerySet = QuerySet[T]{m: m}
	m.Option = nexus.Invoke(func(app *nexus.App) error {
		if m.err != nil {
			return m.err
		}
		b := &binding{app: app}
		m.bound.Store(b)
		lastBinding.Store(b)
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
	if d, ok := ctx.Value(dbKey{}).(*DB); ok {
		return on(ctx, d), nil
	}
	name := m.dbName
	if n, ok := ctx.Value(usingKey{}).(string); ok {
		name = n
	}
	b := m.bound.Load()
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
