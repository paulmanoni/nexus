package jsgen

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/dop251/goja"

	"github.com/paulmanoni/nexus/v2/view"
)

// Each case is one expression written twice: as the Go the server
// evaluates, and as source the translator compiles for the browser. The
// compiled JavaScript runs in goja with the real browser runtime and must
// give the value Go gives.
type coherenceCase struct {
	src  string
	caps map[string]any
	goV  func() any
}

// testScope gives every test signal a distinct id, as a component would.
var testScope = view.Enter(context.Background(), "Coherence")

func sig[T any](v T) *view.Signal[T] { return view.State(testScope, v) }

func coherenceCases() []coherenceCase {
	q := sig("  Hello, Straße  ")
	empty := sig("")
	emoji := sig("héllo 👋")
	count := sig(41)
	open := sig(false)
	space := sig("\u0085 x \u00a0")
	bom := sig("\ufeffx")
	return []coherenceCase{
		{`q.Get()`, map[string]any{"q": q}, func() any { return q.Get() }},
		{`strings.TrimSpace(q.Get())`, map[string]any{"q": q}, func() any { return strings.TrimSpace(q.Get()) }},
		{`strings.ToUpper(q.Get())`, map[string]any{"q": q}, func() any { return strings.ToUpper(q.Get()) }},
		{`strings.ToLower(q.Get())`, map[string]any{"q": q}, func() any { return strings.ToLower(q.Get()) }},
		{`strings.TrimSpace(space.Get())`, map[string]any{"space": space}, func() any { return strings.TrimSpace(space.Get()) }},
		{`strings.TrimSpace(bom.Get())`, map[string]any{"bom": bom}, func() any { return strings.TrimSpace(bom.Get()) }},
		{`len(emoji.Get())`, map[string]any{"emoji": emoji}, func() any { return len(emoji.Get()) }},
		{`len(empty.Get()) == 0`, map[string]any{"empty": empty}, func() any { return len(empty.Get()) == 0 }},
		{`strings.Contains(q.Get(), "Stra")`, map[string]any{"q": q}, func() any { return strings.Contains(q.Get(), "Stra") }},
		{`strings.HasPrefix(emoji.Get(), "hé")`, map[string]any{"emoji": emoji}, func() any { return strings.HasPrefix(emoji.Get(), "hé") }},
		{`strings.HasSuffix(emoji.Get(), "👋")`, map[string]any{"emoji": emoji}, func() any { return strings.HasSuffix(emoji.Get(), "👋") }},
		{`strconv.Itoa(count.Get() + 1)`, map[string]any{"count": count}, func() any { return strconv.Itoa(count.Get() + 1) }},
		{`count.Get()*2 - 3 >= 79`, map[string]any{"count": count}, func() any { return count.Get()*2-3 >= 79 }},
		{`!open.Get() && count.Get() > 40 || false`, map[string]any{"count": count, "open": open}, func() any { return !open.Get() && count.Get() > 40 || false }},
		{`"Hi, " + label + "!"`, map[string]any{"label": "Ana"}, func() any { return "Hi, " + "Ana" + "!" }},
		{`-count.Get() < 0`, map[string]any{"count": count}, func() any { return -count.Get() < 0 }},
	}
}

func TestCoherence(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunString(view.RuntimeJS()); err != nil {
		t.Fatalf("runtime.js: %v", err)
	}
	for _, c := range coherenceCases() {
		js, err := Bind(c.src)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		fn, err := vm.RunString(js)
		if err != nil {
			t.Errorf("%s: compiled %s: %v", c.src, js, err)
			continue
		}
		call, _ := goja.AssertFunction(fn)
		res, err := call(goja.Undefined(), capsValue(t, vm, c.caps))
		if err != nil {
			t.Errorf("%s: running %s: %v", c.src, js, err)
			continue
		}
		want := c.goV()
		got := res.Export()
		if !sameValue(want, got) {
			t.Errorf("%s\n Go: %#v\n JS: %#v (%s)", c.src, want, got, js)
		}
	}
}

// capsValue builds the captures object the runtime would: signals become
// live handles via __nx.signal, other values pass as JSON.
func capsValue(t *testing.T, vm *goja.Runtime, caps map[string]any) goja.Value {
	t.Helper()
	b, err := json.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vm.RunString(`(function (raw) {
		const out = {};
		for (const k in raw) {
			const v = raw[k];
			out[k] = v && typeof v === "object" && typeof v.$sig === "string" ? __nx.signal(v.$sig, v.v) : v;
		}
		return out;
	})(` + string(b) + `)`)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// sameValue compares a Go result with goja's export: JavaScript numbers come
// back as int64 or float64.
func sameValue(goV, jsV any) bool {
	switch g := goV.(type) {
	case int:
		switch j := jsV.(type) {
		case int64:
			return int64(g) == j
		case float64:
			return float64(g) == j
		}
		return false
	}
	return reflect.DeepEqual(goV, jsV) || fmt.Sprint(goV) == fmt.Sprint(jsV) && reflect.TypeOf(goV) == reflect.TypeOf(jsV)
}

// A handler compiled to JavaScript changes a signal the way the Go does.
func TestHandlerCoherence(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunString("globalThis.queueMicrotask = (f) => f();\n" + view.RuntimeJS()); err != nil {
		t.Fatal(err)
	}
	src := `view.Do(func(e view.Event) {
		n := count.Get() + step
		if n > 10 {
			n = 10
		}
		count.Set(n)
		label.Set(strings.ToUpper(e.Target.Value))
	})`
	js, err := Handler(src)
	if err != nil {
		t.Fatal(err)
	}
	fn, err := vm.RunString(js)
	if err != nil {
		t.Fatalf("compiled %s: %v", js, err)
	}
	count, label := sig(8), sig("")
	caps := capsValue(t, vm, map[string]any{"count": count, "label": label, "step": 3})
	event, _ := vm.RunString(`({target: {value: "straße"}})`)
	call, _ := goja.AssertFunction(fn)
	if _, err := call(goja.Undefined(), caps, event); err != nil {
		t.Fatal(err)
	}
	got, _ := vm.RunString(`[__nx.signal("` + count.ID() + `").get(), __nx.signal("` + label.ID() + `").get()]`)
	want := []any{int64(10), strings.ToUpper("straße")}
	if !reflect.DeepEqual(got.Export(), want) {
		t.Fatalf("after the handler: %#v, want %#v", got.Export(), want)
	}
}
