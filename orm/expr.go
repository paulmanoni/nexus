package orm

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Expr is a SQL expression a query computes: a field (F), a function call
// (a Func's Of), or a template (SQL). Exprs go where values go — a Q's
// value, Where, Annotate, Update's Set, Aggregate — and nest.
type Expr interface {
	exprSQL(b *builder) (string, error)
}

// F is a field of the model, or an annotation, as an expression:
// Q{"age__gt": orm.F("min_age")} compares two columns, and
// Set{"views": orm.SQL("{0} + 1", orm.F("views"))} adds to one.
func F(name string) Expr { return fieldRef(name) }

type fieldRef string

func (f fieldRef) exprSQL(b *builder) (string, error) {
	col, err := b.ref(string(f))
	if err != nil {
		return "", err
	}
	return col, nil
}

// SQL is an expression written in SQL: {0}, {1}, … stand for args, each
// an Expr or a value sent as an argument ({{ and }} are literal braces).
// It is the escape hatch for what a Func can't say; never build the
// template from user input.
//
//	orm.SQL("{0} + {1}", orm.F("views"), 1)
func SQL(template string, args ...any) Expr { return tmplExpr{template, args} }

type tmplExpr struct {
	tmpl string
	args []any
}

func (t tmplExpr) exprSQL(b *builder) (string, error) { return render(b, t.tmpl, t.args) }

// render fills a template's {n} (and {*}, every argument joined by commas)
// with its arguments.
func render(b *builder, tmpl string, args []any) (string, error) {
	var out strings.Builder
	for i := 0; i < len(tmpl); i++ {
		c := tmpl[i]
		if c == '{' && i+1 < len(tmpl) && tmpl[i+1] == '{' {
			out.WriteByte('{')
			i++
			continue
		}
		if c == '}' && i+1 < len(tmpl) && tmpl[i+1] == '}' {
			out.WriteByte('}')
			i++
			continue
		}
		if c != '{' {
			out.WriteByte(c)
			continue
		}
		end := strings.IndexByte(tmpl[i:], '}')
		if end < 0 {
			return "", fmt.Errorf("orm: unclosed { in %q", tmpl)
		}
		key := tmpl[i+1 : i+end]
		i += end
		if key == "*" {
			parts := make([]string, len(args))
			for j, a := range args {
				s, err := b.operand(a)
				if err != nil {
					return "", err
				}
				parts[j] = s
			}
			out.WriteString(strings.Join(parts, ", "))
			continue
		}
		n, err := strconv.Atoi(key)
		if err != nil || n < 0 || n >= len(args) {
			return "", fmt.Errorf("orm: %q has no argument {%s}", tmpl, key)
		}
		s, err := b.operand(args[n])
		if err != nil {
			return "", err
		}
		out.WriteString(s)
	}
	return out.String(), nil
}

// operand is v in SQL: an Expr rendered, anything else an argument.
func (b *builder) operand(v any) (string, error) {
	if e, ok := v.(Expr); ok {
		return e.exprSQL(b)
	}
	return b.arg(v), nil
}

// Func is a database function, defined once and called with Of. Where the
// databases spell it differently, Template gives each its SQL.
//
//	var Unaccent = orm.Function("unaccent")                 // unaccent(…)
//	var Year = orm.Function("year",
//		orm.Template("postgres", "CAST(EXTRACT(YEAR FROM {0}) AS INTEGER)"),
//		orm.Template("sqlite", "CAST(strftime('%Y', {0}) AS INTEGER)"))  // YEAR(…) on MySQL
//
//	Users.Filter(orm.Where(Unaccent.Of(orm.F("name")), "icontains", "jose"))
//
// A Func registered with Transform is also a step of Q keys:
// Q{"name__unaccent__icontains": "jose"}.
type Func struct {
	name  string
	per   map[string]string // dialect → template; "" for every dialect
	trans bool
}

// FuncOption shapes a Func.
type FuncOption func(*Func)

// Template is the function's SQL on a dialect ("postgres", "mysql",
// "sqlite"; "" for all of them), {0}, {1}, … for its arguments and {*}
// for all of them.
func Template(dialect, template string) FuncOption {
	return func(f *Func) { f.per[strings.ToLower(dialect)] = template }
}

// Function defines a database function. Without a Template it is called
// as name(args…).
func Function(name string, opts ...FuncOption) *Func {
	f := &Func{name: name, per: map[string]string{}}
	for _, o := range opts {
		o(f)
	}
	return f
}

// Name is the function's name.
func (f *Func) Name() string { return f.name }

// Of calls the function: each argument an Expr or a value.
func (f *Func) Of(args ...any) Expr { return call{f, args} }

type call struct {
	f    *Func
	args []any
}

func (c call) exprSQL(b *builder) (string, error) {
	tmpl, ok := c.f.per[b.d.Name()]
	if !ok {
		tmpl, ok = c.f.per[""]
	}
	if !ok {
		tmpl = c.f.name + "({*})"
	}
	return render(b, tmpl, c.args)
}

var transforms sync.Map // lowercased name → *Func

// Transform makes a one-argument function a step of Q keys, by its name,
// between a field and its lookup: Q{"created_at__year__gte": 2026}. It
// returns f, so a declaration can end with it.
func (f *Func) Transform() *Func {
	f.trans = true
	transforms.Store(strings.ToLower(f.name), f)
	return f
}

func transform(name string) (*Func, bool) {
	v, ok := transforms.Load(strings.ToLower(name))
	if !ok {
		return nil, false
	}
	return v.(*Func), true
}

// Functions every model can use, transforms all: Q{"name__lower": "ali"},
// Q{"created_at__year": 2026}, Q{"created_at__date": "2026-10-06"}.
var (
	Lower  = Function("lower").Transform()
	Upper  = Function("upper").Transform()
	Length = Function("length", Template("mysql", "CHAR_LENGTH({0})")).Transform()
	Trim   = Function("trim").Transform()
	Abs    = Function("abs").Transform()
	Year   = Function("year",
		Template("postgres", "CAST(EXTRACT(YEAR FROM {0}) AS INTEGER)"),
		Template("sqlite", "CAST(strftime('%Y', {0}) AS INTEGER)")).Transform()
	Month = Function("month",
		Template("postgres", "CAST(EXTRACT(MONTH FROM {0}) AS INTEGER)"),
		Template("sqlite", "CAST(strftime('%m', {0}) AS INTEGER)")).Transform()
	Day = Function("day",
		Template("postgres", "CAST(EXTRACT(DAY FROM {0}) AS INTEGER)"),
		Template("sqlite", "CAST(strftime('%d', {0}) AS INTEGER)")).Transform()
	Date = Function("date",
		Template("postgres", "CAST({0} AS DATE)"),
		Template("mysql", "DATE({0})"),
		Template("sqlite", "date({0})")).Transform()
	Coalesce = Function("coalesce")
)

// Where is a condition on an expression: Where(Lower.Of(F("name")),
// "exact", "ali"); any Q lookup.
func Where(e Expr, lookup string, value any) Cond { return exprCond{e, lookup, value} }

type exprCond struct {
	e      Expr
	lookup string
	value  any
}

func (c exprCond) sql(b *builder) (string, error) {
	if !lookups[c.lookup] {
		return "", fmt.Errorf("orm: unknown lookup %q", c.lookup)
	}
	col, err := c.e.exprSQL(b)
	if err != nil {
		return "", err
	}
	return b.compare(col, c.lookup, c.value, c.lookup)
}
