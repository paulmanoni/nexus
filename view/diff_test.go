package view

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

func page(rows []string) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><title>t</title></head><body><ul>`)
	for _, r := range rows {
		fmt.Fprintf(&b, `<li id="%s">%s <button onclick="x">go</button></li>`, r, r)
	}
	b.WriteString(`</ul><p>é — 👋 done</p></body></html>`)
	return b.String()
}

// Random edits to a page round-trip through diff and apply, in Go and in
// the browser runtime (goja) — which counts tokens in UTF-16.
func TestDiffRoundTrip(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunString(view_runtimeForTests()); err != nil {
		t.Fatal(err)
	}
	applyJS, _ := goja.AssertFunction(vm.Get("__nx").ToObject(vm).Get("_applyPatch"))
	rng := rand.New(rand.NewSource(1))
	rows := []string{"a", "b", "c", "d", "e", "f"}
	for i := 0; i < 200; i++ {
		before := page(rows)
		next := append([]string(nil), rows...)
		for j := rng.Intn(4); j >= 0; j-- {
			switch k := rng.Intn(3); {
			case k == 0 && len(next) > 0:
				p := rng.Intn(len(next))
				next = append(next[:p], next[p+1:]...)
			case k == 1:
				p := rng.Intn(len(next) + 1)
				next = append(next[:p], append([]string{fmt.Sprintf("n%dé", i)}, next[p:]...)...)
			default:
				if len(next) > 0 {
					next[rng.Intn(len(next))] += "!"
				}
			}
		}
		after := page(next)
		patch, ok := diff(tokenize(before), tokenize(after))
		if !ok {
			t.Fatal("diff gave up on a small change")
		}
		got, ok := apply(tokenize(before), patch)
		if !ok || got != after {
			t.Fatalf("Go apply:\n got %s\nwant %s", got, after)
		}
		pj, _ := json.Marshal(patch)
		res, err := applyJS(goja.Undefined(), vm.ToValue(before), vm.ToValue(string(pj)))
		if err != nil {
			t.Fatal(err)
		}
		if res.String() != after {
			t.Fatalf("browser apply:\n got %s\nwant %s", res.String(), after)
		}
		rows = next
	}
}

func TestPatchIsSmall(t *testing.T) {
	rows := make([]string, 200)
	for i := range rows {
		rows[i] = fmt.Sprintf("row%d", i)
	}
	before := page(rows)
	rows[100] = "changed"
	after := page(rows)
	patch, ok := diff(tokenize(before), tokenize(after))
	if !ok || !worthPatching(patch, after) {
		t.Fatal("a one-row change must patch")
	}
	b, _ := json.Marshal(patch)
	if len(b) > 200 {
		t.Fatalf("patch is %d bytes for a one-row change: %s", len(b), b)
	}
	if _, ok := diff(tokenize(before), tokenize(strings.Repeat("<x>", 5000))); ok && worthPatching(nil, "") {
		t.Fatal("a total rewrite must not patch")
	}
}

func view_runtimeForTests() string {
	return "globalThis.queueMicrotask = (f) => f();\n" + RuntimeJS()
}
