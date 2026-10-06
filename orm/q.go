package orm

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Cond is a condition a QuerySet filters by: a Q, or Or / Not / And of
// conditions.
type Cond interface {
	sql(b *builder) (string, error)
}

// Q is conditions joined by AND, keyed Django's way: a field, then
// optionally __ and a lookup.
//
//	orm.Q{"age__gte": 18, "name__icontains": "a", "email": e}
//
// Lookups: exact (the default), iexact, contains, icontains, startswith,
// istartswith, endswith, iendswith, in, gt, gte, lt, lte, range, isnull.
// A nil value with exact means IS NULL.
type Q map[string]any

func (q Q) sql(b *builder) (string, error) {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range keys {
		s, err := b.lookup(k, q[k])
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	return join(parts, " AND "), nil
}

type group struct {
	op    string
	conds []Cond
}

func (g group) sql(b *builder) (string, error) {
	var parts []string
	for _, c := range g.conds {
		s, err := c.sql(b)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	return join(parts, " "+g.op+" "), nil
}

// And holds when every condition does.
func And(conds ...Cond) Cond { return group{"AND", conds} }

// Or holds when any condition does.
func Or(conds ...Cond) Cond { return group{"OR", conds} }

type not struct{ c Cond }

func (n not) sql(b *builder) (string, error) {
	s, err := n.c.sql(b)
	if err != nil || s == "" {
		return s, err
	}
	return "NOT (" + s + ")", nil
}

// Not holds when c doesn't.
func Not(c Cond) Cond { return not{c} }

func join(parts []string, sep string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return "(" + strings.Join(parts, sep) + ")"
}

var lookups = map[string]bool{
	"exact": true, "iexact": true, "contains": true, "icontains": true,
	"startswith": true, "istartswith": true, "endswith": true, "iendswith": true,
	"in": true, "gt": true, "gte": true, "lt": true, "lte": true,
	"range": true, "isnull": true,
}

// builder is one statement being written: its dialect, model, the
// query's annotations, and arguments so far.
type builder struct {
	d     Dialect
	m     *model
	ann   map[string]Expr
	depth int
	args  []any
}

func (b *builder) arg(v any) string {
	b.args = append(b.args, v)
	return b.d.Placeholder(len(b.args))
}

func (b *builder) col(f *field) string { return b.d.Quote(f.Column) }

// ref is a name as SQL: an annotation's expression, else a field's column.
func (b *builder) ref(name string) (string, error) {
	if e, ok := b.ann[name]; ok {
		if b.depth > 16 {
			return "", fmt.Errorf("orm: annotation %q refers to itself", name)
		}
		b.depth++
		defer func() { b.depth-- }()
		s, err := e.exprSQL(b)
		if err != nil {
			return "", err
		}
		return "(" + s + ")", nil
	}
	if f, ok := b.m.field(name); ok {
		return b.col(f), nil
	}
	return "", fmt.Errorf("orm: %s has no field %q", b.m.Name, name)
}

// resolve is the SQL a lookup key compares and its lookup: a field or
// annotation, then any transforms, then the lookup (exact by default).
func (b *builder) resolve(key string) (string, string, error) {
	parts := strings.Split(key, "__")
	lookup := "exact"
	if len(parts) > 1 && lookups[parts[len(parts)-1]] {
		lookup = parts[len(parts)-1]
		parts = parts[:len(parts)-1]
	}
	col, err := b.ref(parts[0])
	if err != nil {
		return "", "", err
	}
	for _, name := range parts[1:] {
		f, ok := transform(name)
		if !ok {
			return "", "", fmt.Errorf("orm: %s has no field %q (relations are not followed yet, and %q is no transform)", b.m.Name, strings.Join(parts, "__"), name)
		}
		if col, err = f.Of(rawSQL(col)).exprSQL(b); err != nil {
			return "", "", err
		}
	}
	return col, lookup, nil
}

// rawSQL is SQL already written, passed back into a template.
type rawSQL string

func (r rawSQL) exprSQL(*builder) (string, error) { return string(r), nil }

func (b *builder) lookup(key string, v any) (string, error) {
	col, lookup, err := b.resolve(key)
	if err != nil {
		return "", err
	}
	return b.compare(col, lookup, v, key)
}

// pattern is a LIKE pattern around v: a value escaped, an Expr
// concatenated.
func (b *builder) pattern(v any, before, after string) (string, error) {
	if e, ok := v.(Expr); ok {
		s, err := e.exprSQL(b)
		if err != nil {
			return "", err
		}
		concat := func(parts ...string) string {
			if b.d.Name() == "mysql" {
				return "CONCAT(" + strings.Join(parts, ", ") + ")"
			}
			return strings.Join(parts, " || ")
		}
		var parts []string
		if before != "" {
			parts = append(parts, "'"+before+"'")
		}
		parts = append(parts, s)
		if after != "" {
			parts = append(parts, "'"+after+"'")
		}
		return concat(parts...), nil
	}
	return b.arg(before + likeEscape(fmt.Sprint(v)) + after), nil
}

// compare is col against v by lookup; v an Expr or a value.
func (b *builder) compare(col, lookup string, v any, key string) (string, error) {
	like := func(before, after string, insensitive bool) (string, error) {
		p, err := b.pattern(v, before, after)
		if err != nil {
			return "", err
		}
		if insensitive {
			return b.d.ILike(col, p), nil
		}
		return col + " LIKE " + p + " ESCAPE '!'", nil
	}
	cmp := func(op string) (string, error) {
		o, err := b.operand(v)
		if err != nil {
			return "", err
		}
		return col + " " + op + " " + o, nil
	}
	switch lookup {
	case "exact":
		if v == nil {
			return col + " IS NULL", nil
		}
		return cmp("=")
	case "iexact":
		o, err := b.operand(v)
		if err != nil {
			return "", err
		}
		return "LOWER(" + col + ") = LOWER(" + o + ")", nil
	case "contains":
		return like("%", "%", false)
	case "icontains":
		return like("%", "%", true)
	case "startswith":
		return like("", "%", false)
	case "istartswith":
		return like("", "%", true)
	case "endswith":
		return like("%", "", false)
	case "iendswith":
		return like("%", "", true)
	case "gt":
		return cmp(">")
	case "gte":
		return cmp(">=")
	case "lt":
		return cmp("<")
	case "lte":
		return cmp("<=")
	case "isnull":
		if yes, _ := v.(bool); yes {
			return col + " IS NULL", nil
		}
		return col + " IS NOT NULL", nil
	case "in":
		items, err := list(v)
		if err != nil {
			return "", fmt.Errorf("orm: %s__in: %w", key, err)
		}
		if len(items) == 0 {
			return "1 = 0", nil
		}
		marks := make([]string, len(items))
		for i, x := range items {
			if marks[i], err = b.operand(x); err != nil {
				return "", err
			}
		}
		return col + " IN (" + strings.Join(marks, ", ") + ")", nil
	case "range":
		items, err := list(v)
		if err != nil || len(items) != 2 {
			return "", fmt.Errorf("orm: %s__range needs two values", key)
		}
		lo, err := b.operand(items[0])
		if err != nil {
			return "", err
		}
		hi, err := b.operand(items[1])
		if err != nil {
			return "", err
		}
		return col + " BETWEEN " + lo + " AND " + hi, nil
	}
	return "", fmt.Errorf("orm: unknown lookup %q", lookup)
}

// list is a slice or array's items.
func list(v any) ([]any, error) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, fmt.Errorf("want a slice, got %T", v)
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, nil
}
