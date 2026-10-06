// Package orm is nexus's Django-style ORM: a model is a plain struct, its
// manager a package-level value, and its queries lazy, immutable
// QuerySets read with Django's lookups.
//
//	var Users = orm.For[User]()
//
//	adults, err := Users.Filter(orm.Q{"age__gte": 18}).OrderBy("-age").All(ctx)
//	u, err := Users.Get(ctx, orm.Q{"email": e})
//
// Connections stay with nexus/db: a model reads and writes through the
// database a db.Bind registered, found by name when the manager is passed
// to nexus.Boot. See docs/design/orm.md.
package orm

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"github.com/paulmanoni/nexus/orm/internal/tags"
	"reflect"
	"strings"
	"sync"
	"time"
)

// field is a model's column: where it sits in the struct and how it is
// written.
type field struct {
	Name       string // Go name
	Column     string
	Index      []int
	Type       reflect.Type
	PK         bool
	AutoNowAdd bool
	AutoNow    bool
	Unique     bool
	Indexed    bool
	Size       int
	SQLType    string
}

// model is what a Go struct means to the database, computed once per
// type.
type model struct {
	Type   reflect.Type
	Name   string // the Go type's name
	Table  string
	Fields []*field
	PK     *field
	byName map[string]*field // Go name, lowercased, and column
	// computed is the fields tagged orm:"computed", by Go name lowercased
	// and snake_case: no column, filled from the annotation of that name.
	computed map[string]*field
	// rels is the relation fields, by Go name lowercased and snake_case.
	rels    map[string]*relation
	relList []*relation
}

// field is the column a lookup names: by Go field name (any case) or by
// column.
func (m *model) field(name string) (*field, bool) {
	f, ok := m.byName[strings.ToLower(name)]
	return f, ok
}

func (m *model) columns() []string {
	out := make([]string, len(m.Fields))
	for i, f := range m.Fields {
		out[i] = f.Column
	}
	return out
}

var models sync.Map // reflect.Type → *model, or error

// modelOf is the metadata of T, computed on first use.
func modelOf(t reflect.Type, table string) (*model, error) {
	key := t
	if table != "" {
		key = nil
	}
	if key != nil {
		if v, ok := models.Load(key); ok {
			if err, isErr := v.(error); isErr {
				return nil, err
			}
			return v.(*model), nil
		}
	}
	m, err := buildModel(t, table)
	if key != nil {
		if err != nil {
			models.Store(key, err)
		} else {
			models.Store(key, m)
		}
	}
	return m, err
}

type tableNamer interface{ TableName() string }

var (
	scannerType = reflect.TypeFor[sql.Scanner]()
	valuerType  = reflect.TypeFor[driver.Valuer]()
	timeType    = reflect.TypeFor[time.Time]()
)

func buildModel(t reflect.Type, table string) (*model, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("orm: %v is not a struct", t)
	}
	m := &model{Type: t, Name: t.Name(), byName: map[string]*field{}, computed: map[string]*field{}, rels: map[string]*relation{}}
	switch {
	case table != "":
		m.Table = table
	case reflect.PointerTo(t).Implements(reflect.TypeFor[tableNamer]()):
		m.Table = reflect.New(t).Interface().(tableNamer).TableName()
	default:
		m.Table = tags.Plural(tags.Snake(t.Name()))
	}
	if err := collect(m, t, nil, ""); err != nil {
		return nil, err
	}
	for _, f := range m.Fields {
		if f.PK {
			if m.PK != nil {
				return nil, fmt.Errorf("orm: %s has two primary keys, %s and %s", m.Name, m.PK.Name, f.Name)
			}
			m.PK = f
		}
	}
	if m.PK == nil {
		if f, ok := m.byName["id"]; ok {
			f.PK = true
			m.PK = f
		}
	}
	if len(m.Fields) == 0 {
		return nil, fmt.Errorf("orm: %s has no columns", m.Name)
	}
	for _, r := range m.relList {
		if err := r.settle(m); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// collect adds t's columns to m: its own fields, and those of the
// structs it embeds, flattened as Go promotes them; an outer field
// shadows an embedded one of the same name.
func collect(m *model, t reflect.Type, index []int, prefix string) error {
	for i := range t.NumField() {
		sf := t.Field(i)
		path := append(append([]int(nil), index...), i)
		tag := tags.Parse(sf.Name, sf.Tag, sf.Type == timeType)
		if tag.Skip {
			continue
		}
		ft := sf.Type
		base := ft
		if base.Kind() == reflect.Pointer {
			base = base.Elem()
		}
		if sf.Anonymous && base.Kind() == reflect.Struct && !isValue(ft) {
			if ft.Kind() == reflect.Pointer && !sf.IsExported() {
				return fmt.Errorf("orm: %s embeds *%s, which can't be allocated while scanning: export it or embed it by value", m.Name, base.Name())
			}
			if err := collect(m, base, path, prefix); err != nil {
				return err
			}
			continue
		}
		if !sf.IsExported() {
			continue
		}
		if tag.Embedded && base.Kind() == reflect.Struct {
			if err := collect(m, base, path, prefix+tag.Prefix); err != nil {
				return err
			}
			continue
		}
		if !isValue(ft) {
			if r := relationOf(sf, path, tag); r != nil {
				m.addRelation(r)
			}
			continue // a type the database can't hold
		}
		if tag.Computed {
			f := &field{Name: sf.Name, Column: tags.Snake(sf.Name), Index: path, Type: ft}
			m.computed[strings.ToLower(sf.Name)] = f
			m.computed[f.Column] = f
			continue
		}
		col := tag.Column
		if col == "" {
			col = tags.Snake(sf.Name)
		}
		col = prefix + col
		f := &field{Name: sf.Name, Column: col, Index: path, Type: ft, PK: tag.PK,
			AutoNowAdd: tag.AutoNowAdd, AutoNow: tag.AutoNow, Unique: tag.Unique, Indexed: tag.Index, Size: tag.Size, SQLType: tag.Type}
		key := strings.ToLower(sf.Name)
		if old, ok := m.byName[key]; ok {
			if len(old.Index) <= len(path) {
				continue // shadowed by a shallower field
			}
			m.remove(old)
		}
		m.Fields = append(m.Fields, f)
		m.byName[key] = f
		m.byName[strings.ToLower(col)] = f
	}
	return nil
}

func (m *model) remove(old *field) {
	for i, f := range m.Fields {
		if f == old {
			m.Fields = append(m.Fields[:i], m.Fields[i+1:]...)
			break
		}
	}
	for k, f := range m.byName {
		if f == old {
			delete(m.byName, k)
		}
	}
}

// isValue is whether a column can hold t: basic kinds, times, bytes, and
// types that scan or value themselves.
func isValue(t reflect.Type) bool {
	if t.Implements(scannerType) || reflect.PointerTo(t).Implements(scannerType) || t.Implements(valuerType) {
		return true
	}
	if t.Kind() == reflect.Pointer {
		return isValue(t.Elem())
	}
	if t == timeType {
		return true
	}
	switch t.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.String:
		return true
	case reflect.Slice:
		return t.Elem().Kind() == reflect.Uint8
	}
	return false
}
