package jsgen

import (
	"reflect"
	"strings"
	"testing"
)

func TestCaptures(t *testing.T) {
	for src, want := range map[string][]string{
		`q.Get()`:                              {"q"},
		`strings.TrimSpace(q.Get()) == prefix`: {"prefix", "q"},
		`view.Do(func(e view.Event) { q.Set(e.Target.Value) })`:                 {"q"},
		`view.Do(func(e view.Event) { n := count.Get(); count.Set(n + step) })`: {"count", "step"},
		`len(items) > 0 && !open.Get()`:                                         {"items", "open"},
	} {
		got, err := Captures(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Captures(%s) = %v, want %v", src, got, want)
		}
	}
}

func TestBind(t *testing.T) {
	for src, want := range map[string]string{
		`q.Get()`:                          `(c) => (c.q.get())`,
		`!open.Get()`:                      `(c) => (!(c.open.get()))`,
		`strings.TrimSpace(q.Get()) == ""`: `(c) => ((__nx.strings.trimSpace(c.q.get()) === ""))`,
		`strconv.Itoa(count.Get() * 2)`:    `(c) => (__nx.strconv.itoa((c.count.get() * 2)))`,
		`len(q.Get()) > 3 && ready`:        `(c) => (((__nx.len(c.q.get()) > 3) && c.ready))`,
		`"a\"b" + label`:                   `(c) => (("a\"b" + c.label))`,
		`0x1F + 1_000`:                     `(c) => ((31 + 1000))`,
	} {
		got, err := Bind(src)
		if err != nil {
			t.Fatalf("Bind(%s): %v", src, err)
		}
		if got != want {
			t.Errorf("Bind(%s)\n got %s\nwant %s", src, got, want)
		}
	}
}

func TestHandler(t *testing.T) {
	for src, want := range map[string]string{
		`view.Do(func(e view.Event) { q.Set(e.Target.Value) })`: `(c, e) => { c.q.set(e.target.value); }`,
		`view.Do(func(_ view.Event) { open.Set(!open.Get()) })`: `(c, _e) => { c.open.set(!(c.open.get())); }`,
		`count.Set(count.Get() + 1)`:                            `(c, e) => { c.count.set((c.count.get() + 1)); }`,
		`count.Set(0)`:                                          `(c, e) => { c.count.set(0); }`,
		`view.Do(func(e view.Event) {
			n := count.Get() + 1
			if n > max {
				n = max
			} else if n < 0 {
				return
			}
			count.Set(n)
		})`: `(c, e) => { let n = (c.count.get() + 1); if ((n > c.max)) { n = c.max; } else if ((n < 0)) { return; } c.count.set(n); }`,
	} {
		got, err := Handler(src)
		if err != nil {
			t.Fatalf("Handler(%s): %v", src, err)
		}
		if got != want {
			t.Errorf("Handler(%s)\n got %s\nwant %s", src, got, want)
		}
	}
}

// Constructs outside the vocabulary are errors that say what is wrong.
func TestUnsupported(t *testing.T) {
	for src, want := range map[string]string{
		`a / b`:                "the / operator is not supported",
		`user.Name`:            "field access is supported on the event only",
		`fmt.Sprint(x)`:        "fmt.Sprint is not available",
		`strings.Repeat(x, 2)`: "strings.Repeat is not available",
		`items[0]`:             "a IndexExpr is not supported",
		`q.Len()`:              "q.Len is not available",
		`q.Set()`:              "Set takes 1 argument",
		`'x'`:                  "CHAR literals are not supported",
	} {
		_, err := Bind(src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Bind(%s) = %v, want an error containing %q", src, err, want)
		}
	}
	for src, want := range map[string]string{
		`func(a, b view.Event) {}`:                       "a handler takes one parameter",
		`view.Do(func(e view.Event) { for {} })`:         "a ForStmt is not supported",
		`view.Do(func(e view.Event) { q = 1 })`:          "q is not a local variable",
		`view.Do(func(e view.Event) { e.Target.Files })`: "view.Event has no browser field Files",
		`handle`: "an action is a signal's Set",
	} {
		_, err := Handler(src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Handler(%s) = %v, want an error containing %q", src, err, want)
		}
	}
}

// A signal field of a state struct is captured and compiled as one path.
func TestPaths(t *testing.T) {
	paths := Paths{"s.Query": true}
	caps, _ := CapturesIn(`strings.TrimSpace(s.Query.Get()) == "" && n > 1`, paths)
	if strings.Join(caps, ",") != "n,s.Query" {
		t.Fatalf("captures = %v", caps)
	}
	js, err := BindIn(`s.Query.Get() == ""`, paths)
	if err != nil || js != `(c) => ((c["s.Query"].get() === ""))` {
		t.Fatalf("%s %v", js, err)
	}
	js, err = HandlerIn(`view.Do(func(e view.Event) { s.Query.Set(e.Target.Value) })`, paths)
	if err != nil || js != `(c, e) => { c["s.Query"].set(e.target.value); }` {
		t.Fatalf("%s %v", js, err)
	}
	if _, err := BindIn(`s.Other.Get()`, paths); err == nil {
		t.Fatal("a field that is not a signal path stays unsupported")
	}
}
