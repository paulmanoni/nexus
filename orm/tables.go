package orm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"

	"github.com/paulmanoni/nexus/orm/internal/schema"
)

// Model is a model's manager, as CreateTables and the migrations take it.
type Model interface {
	tables() ([]schema.Table, error)
	conn(ctx context.Context) (conn, error)
}

// CreateTables creates the models' tables, and the tables between their
// many-to-many relations, where they are not already: for tests and
// tools. A deployed schema changes through migrations (orm.Migrate).
func CreateTables(ctx context.Context, models ...Model) error {
	if len(models) == 0 {
		return nil
	}
	c, err := models[0].conn(ctx)
	if err != nil {
		return err
	}
	var all []schema.Table
	seen := map[string]bool{}
	for _, m := range models {
		ts, err := m.tables()
		if err != nil {
			return err
		}
		for _, t := range ts {
			if !seen[t.Name] {
				seen[t.Name] = true
				all = append(all, t)
			}
		}
	}
	d := schema.Dialect{Name: c.d.Name(), Quote: c.d.Quote}
	for _, t := range schema.Sort(all) {
		for _, s := range d.Create(t, true) {
			if _, err := c.exec(ctx, s, nil); err != nil {
				return fmt.Errorf("orm: creating %s: %w", t.Name, err)
			}
		}
	}
	return nil
}

func (m *Manager[T]) tables() ([]schema.Table, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.meta.tables()
}

// tables is the model's table, and the tables between its many-to-many
// relations.
func (m *model) tables() ([]schema.Table, error) {
	t := schema.Table{Name: m.Table}
	for _, f := range m.Fields {
		t.Columns = append(t.Columns, columnOf(f))
	}
	out := []schema.Table{t}
	for _, r := range m.relList {
		switch r.Kind {
		case relFK:
			target, err := r.target()
			if err != nil {
				return nil, err
			}
			out[0].FKs = append(out[0].FKs, schema.ForeignKey{Column: r.Column, Table: target.Table, Ref: target.PK.Column, OnDelete: r.OnDelete})
		case relM2M:
			target, err := r.target()
			if err != nil {
				return nil, err
			}
			local, remote := columnOf(m.PK), columnOf(target.PK)
			local.Name, remote.Name = r.ThroughLocal, r.ThroughRemote
			local.Auto, remote.Auto = false, false
			local.Unique, remote.Unique = false, false
			out = append(out, schema.Table{
				Name:    r.Through,
				Columns: []schema.Column{local, remote},
				FKs: []schema.ForeignKey{
					{Column: r.ThroughLocal, Table: m.Table, Ref: m.PK.Column, OnDelete: "cascade"},
					{Column: r.ThroughRemote, Table: target.Table, Ref: target.PK.Column, OnDelete: "cascade"},
				},
			})
		}
	}
	return out, nil
}

// columnOf is a field as a column: its kind from its Go type, NULL when
// it is a pointer.
func columnOf(f *field) schema.Column {
	t := f.Type
	c := schema.Column{Name: f.Column, PK: f.PK, Unique: f.Unique, Index: f.Indexed, Size: f.Size, Type: f.SQLType}
	if t.Kind() == reflect.Pointer {
		c.Nullable, t = true, t.Elem()
	}
	c.Kind = kindOf(t)
	if c.PK {
		c.Nullable = false
		c.Auto = c.Kind == schema.Int || c.Kind == schema.Int32
	}
	return c
}

var (
	scannerT = reflect.TypeFor[sql.Scanner]()
	valuerT  = reflect.TypeFor[driver.Valuer]()
)

func kindOf(t reflect.Type) schema.Kind {
	switch {
	case t == timeType:
		return schema.Time
	case reflect.PointerTo(t).Implements(scannerT) || t.Implements(valuerT):
		return schema.Custom
	}
	switch t.Kind() {
	case reflect.Bool:
		return schema.Bool
	case reflect.Int, reflect.Int64, reflect.Uint, reflect.Uint64, reflect.Uint32:
		return schema.Int
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Uint8, reflect.Uint16:
		return schema.Int32
	case reflect.Float64:
		return schema.Float
	case reflect.Float32:
		return schema.Float32
	case reflect.String:
		return schema.String
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return schema.Bytes
		}
	}
	return schema.Custom
}
