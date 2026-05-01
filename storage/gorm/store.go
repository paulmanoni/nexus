// Package gorm is the GORM-backed nexus.Store[T] adapter referenced by
// crud_store.go's doc comment. It turns any GORM-mapped struct into a
// production-ready Store the AsCRUD generator can drive directly.
//
// Usage:
//
//	import (
//	    "github.com/paulmanoni/nexus"
//	    nxgorm "github.com/paulmanoni/nexus/storage/gorm"
//	)
//
//	var Module = nexus.Module("notes",
//	    nexus.Provide(NewNotesDB),
//	    nexus.AsCRUD[Note](
//	        func(ctx context.Context, db *DB) (nexus.Store[Note], error) {
//	            return nxgorm.New[Note](db.GetDB())
//	        },
//	    ),
//	)
//
// The adapter handles ID assignment (UUID by default), sort whitelisting,
// pagination, and translates gorm.ErrRecordNotFound to nexus.ErrCRUDNotFound
// so AsCRUD's HTTP/GraphQL status mapping works without extra wiring.
package gorm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/paulmanoni/nexus"
	gormpkg "gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// Store is a generic GORM-backed nexus.Store[T]. Construct one via New
// and return it from an AsCRUD resolver. A single Store is safe to share
// across requests — the underlying *gorm.DB handles its own pooling.
type Store[T any] struct {
	db       *gormpkg.DB
	getID    func(*T) string
	setID    func(*T, string)
	newID    func() string
	sortable map[string]string
	sch      *schema.Schema
	pkCol    string
}

// Option tweaks Store construction.
type Option[T any] func(*Store[T])

// WithID overrides the reflected ID accessors. Use when T's primary key
// field isn't named "ID" or isn't a string — the resolver still needs
// to surface a string identifier on the wire.
func WithID[T any](get func(*T) string, set func(*T, string)) Option[T] {
	return func(s *Store[T]) { s.getID, s.setID = get, set }
}

// WithIDGenerator overrides the default uuid.NewString. Useful for
// deterministic IDs in tests or for ULID/snowflake schemes.
func WithIDGenerator[T any](fn func() string) Option[T] {
	return func(s *Store[T]) { s.newID = fn }
}

// WithSortable restricts and maps allowed Sort field names. Keys are the
// names callers send in ?sort=; values are the SQL column names. Without
// this option the Store accepts any field GORM's schema can resolve
// (struct field name or db column name).
func WithSortable[T any](mapping map[string]string) Option[T] {
	return func(s *Store[T]) {
		// Defensive copy — caller might keep mutating the map.
		s.sortable = make(map[string]string, len(mapping))
		for k, v := range mapping {
			s.sortable[k] = v
		}
	}
}

var schemaCache sync.Map

// New builds a GORM-backed Store[T]. It parses T's schema once to learn
// the primary-key column and field-name → db-column mapping.
func New[T any](db *gormpkg.DB, opts ...Option[T]) (*Store[T], error) {
	if db == nil {
		return nil, errors.New("nexus/storage/gorm: nil *gorm.DB")
	}
	s := &Store[T]{db: db, newID: uuid.NewString}
	for _, o := range opts {
		o(s)
	}
	if s.getID == nil || s.setID == nil {
		get, set, err := reflectIDAccessors[T]()
		if err != nil {
			return nil, err
		}
		s.getID, s.setID = get, set
	}

	var zero T
	sch, err := schema.Parse(&zero, &schemaCache, db.NamingStrategy)
	if err != nil {
		return nil, fmt.Errorf("nexus/storage/gorm: schema parse: %w", err)
	}
	s.sch = sch
	if pk := sch.PrioritizedPrimaryField; pk != nil {
		s.pkCol = pk.DBName
	} else {
		s.pkCol = "id"
	}
	return s, nil
}

// Find loads one row by primary key. Returns nexus.ErrCRUDNotFound when
// no row matches so AsCRUD maps it to a 404.
func (s *Store[T]) Find(ctx context.Context, id string) (*T, error) {
	var t T
	err := s.db.WithContext(ctx).
		Where(s.pkCol+" = ?", id).
		First(&t).Error
	if errors.Is(err, gormpkg.ErrRecordNotFound) {
		return nil, nexus.ErrCRUDNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Search runs a paginated query. Limit is clamped by AsCRUD before this
// call, but we re-clamp defensively so direct callers don't need to.
func (s *Store[T]) Search(ctx context.Context, opts nexus.ListOptions) ([]T, int, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	var zero T
	var total int64
	if err := s.db.WithContext(ctx).Model(&zero).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	q := s.db.WithContext(ctx).Limit(limit).Offset(offset)
	for _, raw := range opts.Sort {
		col, desc, ok := s.resolveSort(raw)
		if !ok {
			continue
		}
		dir := "ASC"
		if desc {
			dir = "DESC"
		}
		q = q.Order(col + " " + dir)
	}

	var items []T
	if err := q.Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, int(total), nil
}

// Save upserts by primary key. An empty ID is treated as a create — the
// Store assigns a fresh ID via newID and writes it back through setID
// so the caller sees it on the response.
func (s *Store[T]) Save(ctx context.Context, item *T) error {
	if s.getID(item) == "" {
		s.setID(item, s.newID())
	}
	return s.db.WithContext(ctx).Save(item).Error
}

// Remove deletes by primary key. Returns nexus.ErrCRUDNotFound when no
// row matched so AsCRUD's DELETE handler can return a 404 instead of a
// silent 204.
func (s *Store[T]) Remove(ctx context.Context, id string) error {
	var zero T
	res := s.db.WithContext(ctx).Where(s.pkCol+" = ?", id).Delete(&zero)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nexus.ErrCRUDNotFound
	}
	return nil
}

// resolveSort parses one entry from ListOptions.Sort ("name", "-created_at")
// and returns the SQL column name plus direction. Unknown fields return
// ok=false so Search can drop them silently rather than 500ing on a typo.
func (s *Store[T]) resolveSort(raw string) (col string, desc bool, ok bool) {
	f := strings.TrimSpace(raw)
	if f == "" {
		return "", false, false
	}
	if strings.HasPrefix(f, "-") {
		desc = true
		f = strings.TrimPrefix(f, "-")
	}
	if s.sortable != nil {
		col, ok = s.sortable[f]
		return col, desc, ok
	}
	if s.sch == nil {
		return "", false, false
	}
	if field := s.sch.LookUpField(f); field != nil {
		return field.DBName, desc, true
	}
	return "", false, false
}

// reflectIDAccessors mirrors the helper in nexus/crud_reflect.go. We
// duplicate it here rather than re-export it to keep the parent package's
// surface area unchanged.
func reflectIDAccessors[T any]() (func(*T) string, func(*T, string), error) {
	var zero T
	rt := reflect.TypeOf(zero)
	if rt.Kind() != reflect.Struct {
		return nil, nil, fmt.Errorf("nexus/storage/gorm: Store[%s] requires a struct type", rt)
	}
	field, ok := rt.FieldByName("ID")
	if !ok {
		return nil, nil, fmt.Errorf("nexus/storage/gorm: Store[%s] cannot infer ID — add an exported string field named \"ID\" or pass WithID", rt)
	}
	if field.Type.Kind() != reflect.String {
		return nil, nil, fmt.Errorf("nexus/storage/gorm: Store[%s] ID field must be a string (got %s)", rt, field.Type)
	}
	idx := field.Index
	get := func(t *T) string {
		v := reflect.ValueOf(t).Elem().FieldByIndex(idx)
		return v.String()
	}
	set := func(t *T, id string) {
		v := reflect.ValueOf(t).Elem().FieldByIndex(idx)
		if v.CanSet() {
			v.SetString(id)
		}
	}
	return get, set, nil
}