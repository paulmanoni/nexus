package viewgen

import (
	"fmt"
	"go/scanner"
	"go/token"
	"slices"
	"sort"
	"strings"

	"github.com/a-h/templ/parser/v2"
	"github.com/a-h/templ/parser/v2/visitor"
)

// A live page whose fields are all view.Assign renders only what changed
// (view/assign.go). The compiler warns where a page that uses Assigns
// isn't, or can't be, tracked:
//
//	a field that isn't an Assign      the page renders in full on every event
//	the template reading such a field  the read that keeps it so
//	the template calling a service     (view.Use of a non-state type) what it
//	                                   returns isn't tracked: the parts that
//	                                   read it won't re-render when it changes
//
// Warnings never stop a build.

// Fields is what Scan reads of a struct type: its view.Assign fields and
// the others (signals aside).
type Fields struct {
	Assigns []string
	Plain   []Field
	Embeds  []Field // structs embedded by value: their fields are the struct's
}

// Field is a struct field that isn't an Assign or a signal — or, among
// Embeds, a struct embedded by value: Name is its type's name, Import its
// package's path when it is another package's.
type Field struct {
	Name   string
	Type   string
	At     *PositionError
	Import string
}

// fieldsOf is the fields of struct type name with the structs it embeds
// by value flattened in, as the runtime tracks a page: an embedded struct's
// Assigns are the page's, and so are its plain fields. An embedded struct
// that can't be found (outside the module) counts as one plain field. Nil
// when name isn't a struct of the package.
func (p *Package) fieldsOf(name string) *Fields {
	return p.flatten(name, map[string]bool{})
}

func (p *Package) flatten(name string, seen map[string]bool) *Fields {
	base := p.AllStructs[name]
	if base == nil {
		base = p.Structs[name]
	}
	if base == nil || seen[p.ImportPath+"."+name] {
		return nil
	}
	seen[p.ImportPath+"."+name] = true
	out := &Fields{Assigns: slices.Clone(base.Assigns), Plain: slices.Clone(base.Plain)}
	for _, e := range base.Embeds {
		var inner *Fields
		switch {
		case e.Import == "":
			inner = p.flatten(e.Name, seen)
		case p.LookupStruct != nil:
			inner = p.LookupStruct(e.Import, e.Name)
		}
		if inner == nil {
			out.Plain = append(out.Plain, e)
			continue
		}
		out.Assigns = append(out.Assigns, inner.Assigns...)
		out.Plain = append(out.Plain, inner.Plain...)
	}
	return out
}

// tracked reports whether a page of this type tracks its changes.
func (f *Fields) tracked() bool { return f != nil && len(f.Assigns) > 0 && len(f.Plain) == 0 }

// trackWarnings checks a method component of a type that uses Assigns.
func (c *component) trackWarnings(t *parser.HTMLTemplate) {
	if c.recvType == "" {
		return
	}
	info := c.pkg.fieldsOf(c.recvType)
	if info == nil || len(info.Assigns) == 0 {
		return
	}
	c.livePages[c.recvType] = true
	plain := map[string]bool{}
	for _, f := range info.Plain {
		plain[f.Name] = true
	}
	view := c.viewSelector()
	v := visitor.New()
	check := func(e parser.Expression) {
		toks := goTokens(e)
		for i := 0; i+2 < len(toks); i++ {
			a, b, n := toks[i], toks[i+1], toks[i+2]
			if a.tok != token.IDENT || b.tok != token.PERIOD || n.tok != token.IDENT {
				continue
			}
			switch {
			case c.recvName != "" && a.lit == c.recvName && plain[n.lit]:
				c.warn(n.line, n.col, "%s.%s is not a view.Assign: %s renders in full on every event — make it a view.Assign so only what changed renders",
					c.recvName, n.lit, c.recvType)
			case view != "" && a.lit == view && n.lit == "Use" && info.tracked():
				typ := bracketed(toks[i+3:])
				if typ == "" || c.isState(typ) {
					continue
				}
				c.warn(a.line, a.col, "%s calls %s in Render: what it returns isn't tracked, so the parts that read it won't re-render when it changes — load it into a view.Assign in Mount, Info or an event",
					c.recvType, typ)
			}
		}
	}
	wrap := func(e *parser.Expression) { check(*e) }
	def := *v
	v.StringExpression = func(n *parser.StringExpression) error { wrap(&n.Expression); return def.StringExpression(n) }
	v.GoCode = func(n *parser.GoCode) error { wrap(&n.Expression); return def.GoCode(n) }
	v.TemplElementExpression = func(n *parser.TemplElementExpression) error {
		wrap(&n.Expression)
		return def.TemplElementExpression(n)
	}
	v.CallTemplateExpression = func(n *parser.CallTemplateExpression) error {
		wrap(&n.Expression)
		return def.CallTemplateExpression(n)
	}
	v.IfExpression = func(n *parser.IfExpression) error {
		wrap(&n.Expression)
		for i := range n.ElseIfs {
			wrap(&n.ElseIfs[i].Expression)
		}
		return def.IfExpression(n)
	}
	v.SwitchExpression = func(n *parser.SwitchExpression) error {
		wrap(&n.Expression)
		for i := range n.Cases {
			wrap(&n.Cases[i].Expression)
		}
		return def.SwitchExpression(n)
	}
	v.ForExpression = func(n *parser.ForExpression) error { wrap(&n.Expression); return def.ForExpression(n) }
	v.ExpressionAttribute = func(n *parser.ExpressionAttribute) error {
		wrap(&n.Expression)
		return def.ExpressionAttribute(n)
	}
	v.BoolExpressionAttribute = func(n *parser.BoolExpressionAttribute) error {
		wrap(&n.Expression)
		return def.BoolExpressionAttribute(n)
	}
	v.SpreadAttributes = func(n *parser.SpreadAttributes) error { wrap(&n.Expression); return def.SpreadAttributes(n) }
	v.ConditionalAttribute = func(n *parser.ConditionalAttribute) error {
		wrap(&n.Expression)
		return def.ConditionalAttribute(n)
	}
	for _, n := range t.Children {
		_ = n.Visit(v)
	}
}

// viewSelector is the name the file imports the view package under.
func (f *fileRewriter) viewSelector() string {
	for sel, line := range f.imports {
		if importPathOf(line) == ViewImport {
			return sel
		}
	}
	return ""
}

// isState reports whether typ, as written in view.Use[typ], is a state
// struct (its signals are per page, and the server never changes them).
func (f *fileRewriter) isState(typ string) bool {
	typ = strings.TrimPrefix(typ, "*")
	if pkg, name, ok := strings.Cut(typ, "."); ok {
		path := importPathOf(f.imports[pkg])
		return path != "" && f.pkg.Lookup != nil && len(f.pkg.Lookup(path, name)) > 0
	}
	return len(f.pkg.States[typ]) > 0
}

func (f *fileRewriter) warn(line, col int, format string, args ...any) {
	f.warnings = append(f.warnings, &PositionError{File: f.file, Line: line, Col: col, Msg: fmt.Sprintf(format, args...)})
}

type goTok struct {
	tok       token.Token
	lit       string
	line, col int // 1-based, in the .templ file
}

// goTokens scans an expression's Go, placing each token in the template.
func goTokens(e parser.Expression) []goTok {
	src := []byte(e.Value)
	fset := token.NewFileSet()
	file := fset.AddFile("", -1, len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, 0)
	var out []goTok
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return out
		}
		if lit == "" {
			lit = tok.String()
		}
		p := fset.Position(pos)
		line, col := int(e.Range.From.Line)+p.Line, p.Column
		if p.Line == 1 {
			col += int(e.Range.From.Col)
		}
		out = append(out, goTok{tok, lit, line, col})
	}
}

// bracketed is the source of the tokens between a leading [ and its ].
func bracketed(toks []goTok) string {
	if len(toks) == 0 || toks[0].tok != token.LBRACK {
		return ""
	}
	var b strings.Builder
	depth := 0
	for _, t := range toks {
		switch t.tok {
		case token.LBRACK:
			depth++
			if depth == 1 {
				continue
			}
		case token.RBRACK:
			depth--
			if depth == 0 {
				return b.String()
			}
		}
		b.WriteString(t.lit)
	}
	return ""
}

// pageWarnings are the warnings on the Go side of the live pages a
// package's templates render: each field that keeps one from tracking.
func pageWarnings(pkg *Package, pages map[string]bool) []*PositionError {
	names := make([]string, 0, len(pages))
	for name := range pages {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []*PositionError
	for _, name := range names {
		info := pkg.fieldsOf(name)
		if info == nil || len(info.Assigns) == 0 {
			continue
		}
		for _, f := range info.Plain {
			w := *f.At
			w.Msg = fmt.Sprintf("%s.%s keeps the live page %s rendering in full on every event: make it a view.Assign[%s], as its other fields are, so only what changed renders",
				name, f.Name, name, f.Type)
			out = append(out, &w)
		}
	}
	return out
}
