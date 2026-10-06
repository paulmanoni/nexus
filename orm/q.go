package orm

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/paulmanoni/nexus/orm/internal/tags"
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
	b.neg = !b.neg
	s, err := n.c.sql(b)
	b.neg = !b.neg
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

// stmt is what the builders of one statement share: its arguments and a
// counter naming its subqueries.
type stmt struct {
	args []any
	n    int
}

// join is a LEFT JOIN of a foreign key the statement follows, by its
// path from the queried model.
type joinClause struct {
	path, sql string
}

// builder writes SQL about one model under one alias: the queried model,
// a model a foreign key joins, or the related model of an EXISTS. Its
// joins go to the FROM it belongs to.
type builder struct {
	d     Dialect
	m     *model
	alias string
	path  string // the relation path from the queried model; "" for it
	ann   map[string]Expr
	depth int
	st    *stmt
	joins *[]joinClause
	outer *builder // the query a subquery's OuterRef reads
	neg   bool     // under an odd number of Nots
}

// newBuilder is a builder of a statement on m, aliased by its table.
func newBuilder(d Dialect, m *model) *builder {
	return &builder{d: d, m: m, alias: m.Table, st: &stmt{}, joins: &[]joinClause{}}
}

func (b *builder) arg(v any) string {
	b.st.args = append(b.st.args, v)
	return b.d.Placeholder(len(b.st.args))
}

// args is the statement's arguments, in the order the SQL names them.
func (b *builder) args() []any { return b.st.args }

// col is a column of b's model, qualified by its alias.
func (b *builder) col(f *field) string { return b.d.Quote(b.alias) + "." + b.d.Quote(f.Column) }

// bare is a column unqualified, as UPDATE's SET names it.
func (b *builder) bare(f *field) string { return b.d.Quote(f.Column) }

// from is the FROM of b's statement: its table and the joins its
// conditions, orders and columns follow.
func (b *builder) from() string {
	s := b.d.Quote(b.m.Table)
	if b.alias != b.m.Table {
		s += " AS " + b.d.Quote(b.alias)
	}
	for _, j := range *b.joins {
		s += j.sql
	}
	return s
}

// follow is the builder of the model a foreign key leads to, joining it
// once per path.
func (b *builder) follow(r *relation) (*builder, error) {
	t, err := r.target()
	if err != nil {
		return nil, err
	}
	path := strings.TrimPrefix(b.path+"__"+tags.Snake(r.Name), "__")
	alias := path
	if alias == b.joinRoot() {
		alias += "_"
	}
	fk, ok := b.m.field(r.Column)
	if !ok {
		return nil, fmt.Errorf("orm: %s has no column %q", b.m.Name, r.Column)
	}
	nb := &builder{d: b.d, m: t, alias: alias, path: path, st: b.st, joins: b.joins, outer: b.outer, neg: b.neg}
	for _, j := range *b.joins {
		if j.path == path {
			return nb, nil
		}
	}
	*b.joins = append(*b.joins, joinClause{path: path, sql: " LEFT JOIN " + b.d.Quote(t.Table) + " AS " + b.d.Quote(alias) +
		" ON " + nb.col(t.PK) + " = " + b.col(fk)})
	return nb, nil
}

// joinRoot is the alias of the FROM's own table, which no join may take.
func (b *builder) joinRoot() string {
	if b.path == "" {
		return b.alias
	}
	return ""
}

// ref is a name as SQL: an annotation's expression, a field's column, or
// a path through foreign keys to one (author__name).
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
	parts := strings.Split(name, "__")
	cur := b
	for i, part := range parts {
		last := i == len(parts)-1
		if f, ok := cur.m.field(part); ok && last {
			return cur.col(f), nil
		}
		r, ok := cur.m.relation(part)
		if !ok {
			break
		}
		if !r.one() {
			return "", fmt.Errorf("orm: %s.%s holds many rows: it can't be read as one value", cur.m.Name, r.Name)
		}
		if last {
			f, _ := cur.m.field(r.Column)
			return cur.col(f), nil
		}
		next, err := cur.follow(r)
		if err != nil {
			return "", err
		}
		cur = next
	}
	return "", fmt.Errorf("orm: %s has no field %q", b.m.Name, name)
}

// lookup is the SQL of one Q entry: a field (after any foreign keys) and
// its transforms and lookup, or an EXISTS over a relation that holds many
// rows (posts__title__icontains).
func (b *builder) lookup(key string, v any) (string, error) {
	parts := strings.Split(key, "__")
	lookup := "exact"
	if len(parts) > 1 && lookups[parts[len(parts)-1]] {
		lookup = parts[len(parts)-1]
		parts = parts[:len(parts)-1]
	}
	if _, ok := b.ann[parts[0]]; ok {
		col, err := b.ref(parts[0])
		if err != nil {
			return "", err
		}
		return b.transformed(col, parts[1:], lookup, v, key)
	}
	cur := b
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if f, ok := cur.m.field(part); ok {
			s, err := cur.transformed(cur.col(f), parts[i+1:], lookup, v, key)
			return cur.nullSafe(s, f, lookup, v), err
		}
		r, ok := cur.m.relation(part)
		if !ok {
			return "", fmt.Errorf("orm: %s has no field %q", cur.m.Name, part)
		}
		rest := parts[i+1:]
		if r.one() {
			if len(rest) == 0 {
				f, _ := cur.m.field(r.Column)
				s, err := cur.compare(cur.col(f), lookup, v, key)
				return cur.nullSafe(s, f, lookup, v), err
			}
			next, err := cur.follow(r)
			if err != nil {
				return "", err
			}
			cur = next
			continue
		}
		return cur.exists(r, rest, lookup, v, key)
	}
	return "", fmt.Errorf("orm: %s has no field %q", b.m.Name, key)
}

// nullSafe keeps a negated lookup true of a NULL column, as Django's
// exclude does: NOT (bio LIKE 'a%') is unknown where bio is NULL, so a
// column that can be NULL (a nullable field, or any reached through a
// LEFT JOIN) is compared as (bio LIKE 'a%' AND bio IS NOT NULL).
func (b *builder) nullSafe(s string, f *field, lookup string, v any) string {
	if !b.neg || s == "" || lookup == "isnull" || v == nil {
		return s
	}
	if b.path == "" && f.Type.Kind() != reflect.Pointer {
		return s
	}
	return "(" + s + " AND " + b.col(f) + " IS NOT NULL)"
}

// transformed is col through the transforms named, compared by lookup.
func (b *builder) transformed(col string, steps []string, lookup string, v any, key string) (string, error) {
	for _, name := range steps {
		f, ok := transform(name)
		if !ok {
			return "", fmt.Errorf("orm: %q in %q is no field, relation or transform of %s", name, key, b.m.Name)
		}
		s, err := f.Of(rawSQL(col)).exprSQL(b)
		if err != nil {
			return "", err
		}
		col = s
	}
	return b.compare(col, lookup, v, key)
}

// exists is a lookup across a relation holding many rows: EXISTS a
// related row matching the rest of the key. With no rest, isnull asks
// whether there is none.
func (b *builder) exists(r *relation, rest []string, lookup string, v any, key string) (string, error) {
	t, err := r.target()
	if err != nil {
		return "", err
	}
	b.st.n++
	alias := "nexus_" + strconv.Itoa(b.st.n)
	child := &builder{d: b.d, m: t, alias: alias, path: alias, st: b.st, joins: &[]joinClause{}, outer: b.outer}
	var from, link string
	switch r.Kind {
	case relRev:
		col, err := r.childColumn(b.m, t)
		if err != nil {
			return "", err
		}
		from = b.d.Quote(t.Table) + " AS " + b.d.Quote(alias)
		link = child.col(col) + " = " + b.col(b.m.PK)
	case relM2M:
		through := alias + "_through"
		from = b.d.Quote(r.Through) + " AS " + b.d.Quote(through) + " JOIN " + b.d.Quote(t.Table) + " AS " + b.d.Quote(alias) +
			" ON " + child.col(t.PK) + " = " + b.d.Quote(through) + "." + b.d.Quote(r.ThroughRemote)
		link = b.d.Quote(through) + "." + b.d.Quote(r.ThroughLocal) + " = " + b.col(b.m.PK)
	}
	if len(rest) == 0 {
		if lookup != "isnull" {
			return "", fmt.Errorf("orm: %q compares %s.%s, which holds many rows: name one of its fields", key, b.m.Name, r.Name)
		}
		s := "EXISTS (SELECT 1 FROM " + from + " WHERE " + link + ")"
		if yes, _ := v.(bool); yes {
			s = "NOT " + s
		}
		return s, nil
	}
	cond, err := child.lookup(strings.Join(rest, "__")+"__"+lookup, v)
	if err != nil {
		return "", err
	}
	for _, j := range *child.joins {
		from += j.sql
	}
	return "EXISTS (SELECT 1 FROM " + from + " WHERE " + link + " AND " + cond + ")", nil
}

// rawSQL is SQL already written, passed back into a template.
type rawSQL string

func (r rawSQL) exprSQL(*builder) (string, error) { return string(r), nil }

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
		if sq, ok := v.(subquerier); ok {
			s, err := sq.subquerySQL(b, false)
			if err != nil {
				return "", err
			}
			return col + " IN (" + s + ")", nil
		}
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
