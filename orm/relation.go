package orm

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/paulmanoni/nexus/orm/internal/tags"
)

type relKind int

const (
	relFK  relKind = iota + 1 // this row holds the related row's key
	relRev                    // the related rows hold this row's key
	relM2M                    // a table between holds both keys
)

// relation is a field holding related rows: a pointer or struct for a
// foreign key, a slice for reverse foreign keys and many-to-many.
type relation struct {
	Name   string // Go name
	Index  []int
	Kind   relKind
	Target reflect.Type // the related model's struct type
	Ptr    bool         // the field (or the slice's elements) is a pointer
	Slice  bool
	// Single is a reverse foreign key holding one row (GORM's has-one):
	// the related model holds this row's key, at most once.
	Single bool

	fkTag, relTag, m2mTag, gormFK string
	OnDelete                      string // a foreign key's: cascade, set_null, restrict

	// FK: the column of this model holding the key. Rev: the related
	// model's column holding this row's key.
	Column string
	// M2M: the table between, its column for this row's key and for the
	// related row's.
	Through, ThroughLocal, ThroughRemote string
}

// relationOf is the relation a field declares, or nil: a struct, a
// pointer to one or a slice of either, of a type that isn't a column.
func relationOf(sf reflect.StructField, index []int, t tags.Tags) *relation {
	if !sf.IsExported() {
		return nil
	}
	ft := sf.Type
	r := &relation{Name: sf.Name, Index: index, fkTag: t.FK, relTag: t.Rel, m2mTag: t.M2M, gormFK: t.GormForeignKey, OnDelete: t.OnDelete}
	if ft.Kind() == reflect.Slice {
		r.Slice, ft = true, ft.Elem()
	}
	if ft.Kind() == reflect.Pointer {
		r.Ptr, ft = true, ft.Elem()
	}
	if ft.Kind() != reflect.Struct || ft == timeType {
		return nil
	}
	r.Target = ft
	return r
}

func (m *model) addRelation(r *relation) {
	m.relList = append(m.relList, r)
	m.rels[strings.ToLower(r.Name)] = r
	m.rels[tags.Snake(r.Name)] = r
}

// relation is the relation a lookup step names, by Go field name (any
// case) or snake_case.
func (m *model) relation(name string) (*relation, bool) {
	r, ok := m.rels[strings.ToLower(name)]
	if !ok {
		r, ok = m.rels[name]
	}
	return r, ok && r.Kind != 0
}

// settle decides what a relation is from its tags, else by convention:
// Author *User with an AuthorID field is a foreign key; Posts []Post a
// reverse foreign key on Post's <Model>ID. A field that is neither stays
// out of the model, as before.
func (r *relation) settle(m *model) error {
	switch {
	case r.m2mTag != "":
		r.Kind = relM2M
		parts := strings.Split(r.m2mTag, ",")
		r.Through = strings.TrimSpace(parts[0])
		r.ThroughLocal = tags.Snake(m.Name) + "_id"
		r.ThroughRemote = tags.Snake(r.Target.Name()) + "_id"
		if len(parts) == 3 {
			r.ThroughLocal, r.ThroughRemote = strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
		}
		if !r.Slice {
			return fmt.Errorf("orm: %s.%s is many-to-many but not a slice", m.Name, r.Name)
		}
		if m.PK == nil {
			return fmt.Errorf("orm: %s.%s: many-to-many needs %s's primary key", m.Name, r.Name, m.Name)
		}
	case r.fkTag != "" || (r.gormFK != "" && !r.Slice):
		r.Kind = relFK
		r.Column = r.fkTag
		if r.Column == "" {
			f, ok := m.field(r.gormFK)
			if !ok {
				// GORM's has-one: the key is on the related model.
				if m.PK == nil {
					return fmt.Errorf("orm: %s.%s: foreignKey %s is no field of %s, and %s has no primary key for a has-one", m.Name, r.Name, r.gormFK, m.Name, m.Name)
				}
				r.Kind, r.Single = relRev, true
				return nil
			}
			r.Column = f.Column
		}
		if _, ok := m.field(r.Column); !ok {
			return fmt.Errorf("orm: %s.%s: %s has no column %q", m.Name, r.Name, m.Name, r.Column)
		}
		if r.Slice {
			return fmt.Errorf("orm: %s.%s is a foreign key but a slice", m.Name, r.Name)
		}
	case r.relTag != "" || (r.gormFK != "" && r.Slice):
		r.Kind = relRev
		r.Column = r.relTag // checked against the related model when it is used
		if !r.Slice {
			return fmt.Errorf("orm: %s.%s is a reverse foreign key but not a slice", m.Name, r.Name)
		}
		if m.PK == nil {
			return fmt.Errorf("orm: %s.%s: a reverse foreign key needs %s's primary key", m.Name, r.Name, m.Name)
		}
	case !r.Slice:
		if f, ok := m.field(r.Name + "ID"); ok {
			r.Kind, r.Column = relFK, f.Column
		}
	default:
		if m.PK != nil {
			r.Kind = relRev // its column is found on the related model when used
		}
	}
	return nil
}

// target is the related model.
func (r *relation) target() (*model, error) {
	t, err := modelOf(r.Target, "")
	if err != nil {
		return nil, err
	}
	if t.PK == nil {
		return nil, fmt.Errorf("orm: %s has no primary key to relate by", t.Name)
	}
	return t, nil
}

// childColumn is a reverse relation's column on the related model:
// tagged, GORM's foreignKey field, or <Model>ID by convention.
func (r *relation) childColumn(m, child *model) (*field, error) {
	switch {
	case r.Column != "":
		if f, ok := child.field(r.Column); ok {
			return f, nil
		}
	case r.gormFK != "":
		if f, ok := child.field(r.gormFK); ok {
			return f, nil
		}
	default:
		if f, ok := child.field(m.Name + "ID"); ok {
			return f, nil
		}
	}
	return nil, fmt.Errorf("orm: %s.%s: %s has no column holding %s's key (tag it orm:\"rel:<column>\")", m.Name, r.Name, child.Name, m.Name)
}

// one is whether the relation holds one row: a foreign key.
func (r *relation) one() bool { return r.Kind == relFK }
