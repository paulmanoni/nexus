package template

import (
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"reflect"
	"strconv"
)

// evalExpr parses a Go expression string and evaluates it against
// scope. Returns an error if parsing or evaluation fails — the
// interpreter turns errors into inline [!err: …] markers so the bug
// surfaces in the rendered output rather than being swallowed.
//
// Supported subset (Go syntax via go/parser):
//
//	literals:       42, 3.14, "hi", true, nil
//	identifiers:    Posts (resolved against scope chain)
//	field access:   post.Title  (struct field / map[string]any key / method)
//	calls:          titlecase(post.Title), len(posts)
//	indexing:       posts[0], usersByID["paul"]
//	binary ops:     + - * / % == != < <= > >= && ||
//	unary ops:      -x, !x
//	parentheses:    (a + b) * c
//
// Out of scope for v1: type conversions, slice expressions, composite
// literals, function literals, pointer ops, channel ops, type
// assertions. Helper functions cover the cases that need them.
func evalExpr(src string, scope *scope) (any, error) {
	expr, err := goparser.ParseExpr(src)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", src, err)
	}
	return evalNode(expr, scope)
}

func evalNode(n ast.Expr, scope *scope) (any, error) {
	switch e := n.(type) {
	case *ast.BasicLit:
		return evalLit(e)
	case *ast.Ident:
		return evalIdent(e, scope)
	case *ast.SelectorExpr:
		return evalSelector(e, scope)
	case *ast.CallExpr:
		return evalCall(e, scope)
	case *ast.IndexExpr:
		return evalIndex(e, scope)
	case *ast.BinaryExpr:
		return evalBinary(e, scope)
	case *ast.UnaryExpr:
		return evalUnary(e, scope)
	case *ast.ParenExpr:
		return evalNode(e.X, scope)
	}
	return nil, fmt.Errorf("unsupported expression %T", n)
}

func evalLit(lit *ast.BasicLit) (any, error) {
	switch lit.Kind {
	case token.INT:
		v, err := strconv.ParseInt(lit.Value, 0, 64)
		if err != nil {
			return nil, err
		}
		return v, nil
	case token.FLOAT:
		return strconv.ParseFloat(lit.Value, 64)
	case token.STRING:
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return nil, err
		}
		return s, nil
	case token.CHAR:
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return nil, err
		}
		if len(s) == 0 {
			return nil, fmt.Errorf("empty rune literal")
		}
		return []rune(s)[0], nil
	}
	return nil, fmt.Errorf("unsupported literal %v", lit.Kind)
}

// evalIdent resolves a bare identifier. Reserved names (true, false,
// nil) short-circuit; everything else flows through the scope chain.
func evalIdent(id *ast.Ident, scope *scope) (any, error) {
	switch id.Name {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "nil":
		return nil, nil
	}
	v, ok := scope.lookup(id.Name)
	if !ok {
		return nil, fmt.Errorf("undefined identifier %q", id.Name)
	}
	return v, nil
}

// evalSelector handles x.Y. The receiver x is evaluated first; then
// Y is resolved as a struct field, map key (string keys only), or
// method on x. Pointers are auto-dereferenced.
func evalSelector(sel *ast.SelectorExpr, scope *scope) (any, error) {
	recv, err := evalNode(sel.X, scope)
	if err != nil {
		return nil, err
	}
	return memberAccess(recv, sel.Sel.Name)
}

func memberAccess(recv any, name string) (any, error) {
	if recv == nil {
		return nil, fmt.Errorf("nil receiver for .%s", name)
	}
	v := reflect.ValueOf(recv)
	// Try method on the addressable value first — methods take
	// precedence over fields when names collide (matches Go semantics).
	if m := v.MethodByName(name); m.IsValid() {
		return m.Interface(), nil
	}
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil, fmt.Errorf("nil pointer for .%s", name)
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		f := v.FieldByName(name)
		if !f.IsValid() {
			return nil, fmt.Errorf("no field %s on %s", name, v.Type())
		}
		if !f.CanInterface() {
			return nil, fmt.Errorf("field %s is unexported", name)
		}
		return f.Interface(), nil
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf(".%s on map requires string keys (got %s)", name, v.Type().Key())
		}
		mv := v.MapIndex(reflect.ValueOf(name))
		if !mv.IsValid() {
			return nil, nil // missing key → nil; consistent with template semantics
		}
		return mv.Interface(), nil
	}
	return nil, fmt.Errorf("cannot access .%s on %T", name, recv)
}

// evalCall handles f(args...). Resolution order for f:
//  1. Go builtin (currently only len)
//  2. Helper from scope's helpers map
//  3. Method/value resolved as a callable from scope
//
// Args are evaluated left-to-right. reflect.Call is used to invoke.
func evalCall(call *ast.CallExpr, scope *scope) (any, error) {
	// Builtins handled by name, NOT routed through reflect.Call.
	if id, ok := call.Fun.(*ast.Ident); ok {
		switch id.Name {
		case "len":
			return evalBuiltinLen(call, scope)
		}
	}

	fn, err := resolveCallable(call.Fun, scope)
	if err != nil {
		return nil, err
	}
	fnV := reflect.ValueOf(fn)
	if fnV.Kind() != reflect.Func {
		return nil, fmt.Errorf("not callable: %T", fn)
	}

	args := make([]reflect.Value, len(call.Args))
	for i, a := range call.Args {
		v, err := evalNode(a, scope)
		if err != nil {
			return nil, fmt.Errorf("arg %d: %w", i, err)
		}
		args[i] = toReflectArg(v, fnV.Type(), i)
	}
	out := fnV.Call(args)
	switch len(out) {
	case 0:
		return nil, nil
	case 1:
		return out[0].Interface(), nil
	case 2:
		// Convention: (value, error). If the error is non-nil, surface it.
		if e, ok := out[1].Interface().(error); ok && e != nil {
			return nil, e
		}
		return out[0].Interface(), nil
	}
	return nil, fmt.Errorf("call returned %d values; templates support 0, 1, or (value, error)", len(out))
}

// resolveCallable handles both bare-ident calls (helper lookup) and
// selector calls (method or stored function value).
func resolveCallable(fun ast.Expr, scope *scope) (any, error) {
	if id, ok := fun.(*ast.Ident); ok {
		if h, ok := scope.helper(id.Name); ok {
			return h, nil
		}
		// Fall through to scope lookup — fields may store funcs.
		if v, ok := scope.lookup(id.Name); ok {
			return v, nil
		}
		return nil, fmt.Errorf("unknown helper or identifier %q", id.Name)
	}
	return evalNode(fun, scope)
}

// toReflectArg converts a Go value to the type expected by parameter
// i of fnType. Best-effort: identical types pass through, convertible
// types (int → int64, etc.) are converted via reflect; otherwise the
// value is passed as-is and Go's call-time type check fires.
func toReflectArg(v any, fnType reflect.Type, i int) reflect.Value {
	if v == nil {
		// Build a zero-value of the expected param type so reflect.Call
		// doesn't panic on a nil interface{} arg.
		if i < fnType.NumIn() {
			return reflect.New(fnType.In(i)).Elem()
		}
		return reflect.Zero(reflect.TypeOf((*any)(nil)).Elem())
	}
	rv := reflect.ValueOf(v)
	if i < fnType.NumIn() {
		want := fnType.In(i)
		if rv.Type() == want {
			return rv
		}
		if rv.Type().ConvertibleTo(want) {
			return rv.Convert(want)
		}
	}
	return rv
}

func evalBuiltinLen(call *ast.CallExpr, scope *scope) (any, error) {
	if len(call.Args) != 1 {
		return nil, fmt.Errorf("len takes 1 arg; got %d", len(call.Args))
	}
	v, err := evalNode(call.Args[0], scope)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return int64(0), nil
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return int64(0), nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.String, reflect.Slice, reflect.Array, reflect.Map, reflect.Chan:
		return int64(rv.Len()), nil
	}
	return nil, fmt.Errorf("len: unsupported type %s", rv.Type())
}

// evalIndex handles arr[i] and m[k]. Out-of-range slice indexing
// returns nil rather than panicking; missing map keys return nil.
// Both match the conservative-templating principle: lookups are
// "best effort" and never crash a render.
func evalIndex(idx *ast.IndexExpr, scope *scope) (any, error) {
	recv, err := evalNode(idx.X, scope)
	if err != nil {
		return nil, err
	}
	key, err := evalNode(idx.Index, scope)
	if err != nil {
		return nil, err
	}
	if recv == nil {
		return nil, nil
	}
	rv := reflect.ValueOf(recv)
	for rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Slice, reflect.Array, reflect.String:
		n := rv.Len()
		i, err := toInt(key)
		if err != nil {
			return nil, err
		}
		if i < 0 || i >= int64(n) {
			return nil, nil
		}
		if rv.Kind() == reflect.String {
			return string(rv.String()[i]), nil
		}
		return rv.Index(int(i)).Interface(), nil
	case reflect.Map:
		kv := reflect.ValueOf(key)
		if !kv.IsValid() {
			return nil, nil
		}
		if kv.Type() != rv.Type().Key() && kv.Type().ConvertibleTo(rv.Type().Key()) {
			kv = kv.Convert(rv.Type().Key())
		}
		mv := rv.MapIndex(kv)
		if !mv.IsValid() {
			return nil, nil
		}
		return mv.Interface(), nil
	}
	return nil, fmt.Errorf("cannot index %T", recv)
}

func evalBinary(b *ast.BinaryExpr, scope *scope) (any, error) {
	// Short-circuit boolean ops: don't evaluate RHS if LHS settles it.
	switch b.Op {
	case token.LAND:
		lv, err := evalNode(b.X, scope)
		if err != nil {
			return nil, err
		}
		if !truthy(lv) {
			return false, nil
		}
		rv, err := evalNode(b.Y, scope)
		if err != nil {
			return nil, err
		}
		return truthy(rv), nil
	case token.LOR:
		lv, err := evalNode(b.X, scope)
		if err != nil {
			return nil, err
		}
		if truthy(lv) {
			return true, nil
		}
		rv, err := evalNode(b.Y, scope)
		if err != nil {
			return nil, err
		}
		return truthy(rv), nil
	}

	lv, err := evalNode(b.X, scope)
	if err != nil {
		return nil, err
	}
	rv, err := evalNode(b.Y, scope)
	if err != nil {
		return nil, err
	}
	return applyBinary(b.Op, lv, rv)
}

func applyBinary(op token.Token, l, r any) (any, error) {
	switch op {
	case token.EQL:
		return equal(l, r), nil
	case token.NEQ:
		return !equal(l, r), nil
	case token.ADD:
		// + works on numbers and strings.
		if ls, ok := l.(string); ok {
			if rs, ok := r.(string); ok {
				return ls + rs, nil
			}
		}
		return numOp(op, l, r)
	case token.SUB, token.MUL, token.QUO, token.REM:
		return numOp(op, l, r)
	case token.LSS, token.LEQ, token.GTR, token.GEQ:
		return numCmp(op, l, r)
	}
	return nil, fmt.Errorf("unsupported binary op %s", op)
}

func evalUnary(u *ast.UnaryExpr, scope *scope) (any, error) {
	v, err := evalNode(u.X, scope)
	if err != nil {
		return nil, err
	}
	switch u.Op {
	case token.NOT:
		return !truthy(v), nil
	case token.SUB:
		if v == nil {
			return int64(0), nil
		}
		switch x := v.(type) {
		case int64:
			return -x, nil
		case int:
			return -int64(x), nil
		case float64:
			return -x, nil
		}
		// Try via reflect for other numeric kinds.
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return -rv.Int(), nil
		case reflect.Float32, reflect.Float64:
			return -rv.Float(), nil
		}
	}
	return nil, fmt.Errorf("unsupported unary op %s on %T", u.Op, v)
}

// numOp performs a numeric binary op, normalizing both operands to
// either int64 (if both whole) or float64. Mixed int+float promotes
// to float. Division by zero returns an error.
func numOp(op token.Token, l, r any) (any, error) {
	if isFloat(l) || isFloat(r) {
		lf, err := toFloat(l)
		if err != nil {
			return nil, err
		}
		rf, err := toFloat(r)
		if err != nil {
			return nil, err
		}
		switch op {
		case token.ADD:
			return lf + rf, nil
		case token.SUB:
			return lf - rf, nil
		case token.MUL:
			return lf * rf, nil
		case token.QUO:
			if rf == 0 {
				return nil, fmt.Errorf("division by zero")
			}
			return lf / rf, nil
		case token.REM:
			return nil, fmt.Errorf("%% requires integer operands")
		}
	}
	li, err := toInt(l)
	if err != nil {
		return nil, err
	}
	ri, err := toInt(r)
	if err != nil {
		return nil, err
	}
	switch op {
	case token.ADD:
		return li + ri, nil
	case token.SUB:
		return li - ri, nil
	case token.MUL:
		return li * ri, nil
	case token.QUO:
		if ri == 0 {
			return nil, fmt.Errorf("division by zero")
		}
		return li / ri, nil
	case token.REM:
		if ri == 0 {
			return nil, fmt.Errorf("division by zero")
		}
		return li % ri, nil
	}
	return nil, fmt.Errorf("unsupported numeric op %s", op)
}

func numCmp(op token.Token, l, r any) (bool, error) {
	if isFloat(l) || isFloat(r) {
		lf, err := toFloat(l)
		if err != nil {
			return false, err
		}
		rf, err := toFloat(r)
		if err != nil {
			return false, err
		}
		switch op {
		case token.LSS:
			return lf < rf, nil
		case token.LEQ:
			return lf <= rf, nil
		case token.GTR:
			return lf > rf, nil
		case token.GEQ:
			return lf >= rf, nil
		}
	}
	// Strings compare lexicographically when both are strings.
	if ls, ok := l.(string); ok {
		if rs, ok := r.(string); ok {
			switch op {
			case token.LSS:
				return ls < rs, nil
			case token.LEQ:
				return ls <= rs, nil
			case token.GTR:
				return ls > rs, nil
			case token.GEQ:
				return ls >= rs, nil
			}
		}
	}
	li, err := toInt(l)
	if err != nil {
		return false, err
	}
	ri, err := toInt(r)
	if err != nil {
		return false, err
	}
	switch op {
	case token.LSS:
		return li < ri, nil
	case token.LEQ:
		return li <= ri, nil
	case token.GTR:
		return li > ri, nil
	case token.GEQ:
		return li >= ri, nil
	}
	return false, fmt.Errorf("unsupported comparison %s", op)
}

// equal does Go's == semantics for the types templates encounter.
// Nil compares equal only to nil — and "nil" here covers BOTH a true
// nil interface and a typed nil (e.g. (*Post)(nil) returned from a
// struct field lookup). Without that, `P != nil` against a struct
// field holding a nil pointer would wrongly evaluate true and a
// short-circuit `P != nil && P.Title` would deref nil.
// Numbers compare across kinds (int64 == float64 works when values agree).
func equal(a, b any) bool {
	aNil := isNilValue(a)
	bNil := isNilValue(b)
	if aNil || bNil {
		return aNil && bNil
	}
	if isNum(a) && isNum(b) {
		af, _ := toFloat(a)
		bf, _ := toFloat(b)
		return af == bf
	}
	return reflect.DeepEqual(a, b)
}

// isNilValue returns true for both an untyped nil interface and a
// typed nil that travels inside an interface (the common case when
// reflection reads a nil pointer/map/slice field).
func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Slice, reflect.Map, reflect.Chan, reflect.Func:
		return rv.IsNil()
	}
	return false
}

func isFloat(v any) bool {
	switch v.(type) {
	case float32, float64:
		return true
	}
	return false
}

func isNum(v any) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return true
	}
	return false
}

func toInt(v any) (int64, error) {
	if v == nil {
		return 0, nil
	}
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	case int32:
		return int64(x), nil
	case float64:
		return int64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(rv.Uint()), nil
	case reflect.Float32, reflect.Float64:
		return int64(rv.Float()), nil
	}
	return 0, fmt.Errorf("cannot convert %T to int", v)
}

func toFloat(v any) (float64, error) {
	if v == nil {
		return 0, nil
	}
	switch x := v.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), nil
	case reflect.Float32, reflect.Float64:
		return rv.Float(), nil
	}
	return 0, fmt.Errorf("cannot convert %T to float", v)
}

// truthy defines truthiness for branch conditions. Tuned for template
// ergonomics: empty strings, zero numbers, nil pointers/interfaces,
// and empty slices/maps all read as false. Structs are always true
// (the zero value isn't a useful "false" signal in template land).
func truthy(v any) bool {
	if v == nil {
		return false
	}
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool:
		return rv.Bool()
	case reflect.String:
		return rv.String() != ""
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint() != 0
	case reflect.Float32, reflect.Float64:
		return rv.Float() != 0
	case reflect.Slice, reflect.Map, reflect.Array, reflect.Chan:
		return rv.Len() > 0
	case reflect.Ptr, reflect.Interface:
		return !rv.IsNil()
	}
	return true
}