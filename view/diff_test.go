package view

import (
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
		fmt.Fprintf(&b, `<li id="%s" class="flex items-center gap-2 rounded-md">%s <button onclick="x">go</button></li>`, r, r)
	}
	b.WriteString(`</ul><p>é — 👋 done</p></body></html>`)
	return b.String()
}

// A connection's worth of random edits round-trips through the differ, the
// Go mirror and the browser runtime (goja, counting in UTF-16) — including
// references to the held render and to the connection's dictionary.
func TestDiffRoundTrip(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunString("globalThis.queueMicrotask = (f) => f();\n" + RuntimeJS()); err != nil {
		t.Fatal(err)
	}
	newPatcher, _ := goja.AssertFunction(vm.Get("__nx").ToObject(vm).Get("_patcher"))
	pv, err := newPatcher(goja.Undefined())
	if err != nil {
		t.Fatal(err)
	}
	js := pv.ToObject(vm)
	jsFull, _ := goja.AssertFunction(js.Get("full"))
	jsApply, _ := goja.AssertFunction(js.Get("apply"))

	var server differ
	var browser mirror
	rng := rand.New(rand.NewSource(1))
	rows := []string{"a", "b", "c", "d", "e", "f"}
	patches := 0
	for i := 0; i < 300; i++ {
		next := append([]string(nil), rows...)
		for j := rng.Intn(4); j >= 0; j-- {
			switch k := rng.Intn(3); {
			case k == 0 && len(next) > 0:
				p := rng.Intn(len(next))
				next = append(next[:p], next[p+1:]...)
			case k == 1:
				p := rng.Intn(len(next) + 1)
				next = append(next[:p], append([]string{fmt.Sprintf("n%dé", i%7)}, next[p:]...)...)
			default:
				if len(next) > 0 {
					next[rng.Intn(len(next))] += "!"
				}
			}
		}
		html := page(next)
		patch, n, full := server.next(html)
		if full {
			browser.full(html)
			if _, err := jsFull(goja.Undefined(), vm.ToValue(html)); err != nil {
				t.Fatal(err)
			}
		} else {
			patches++
			got, ok := browser.apply(patch, n)
			if !ok || got != html {
				t.Fatalf("Go mirror, step %d:\n got %s\nwant %s", i, got, html)
			}
			pj, _ := marshal(patch)
			res, err := jsApply(goja.Undefined(), vm.ToValue(string(pj)))
			if err != nil {
				t.Fatal(err)
			}
			if res.String() != html {
				t.Fatalf("browser runtime, step %d:\n got %s\nwant %s", i, res.String(), html)
			}
		}
		rows = next
	}
	if patches < 250 {
		t.Fatalf("only %d of 300 updates travelled as patches", patches)
	}
}

func boardPage(rows []string, adopted map[string]bool, count int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<html><body><p><b id="adopted">%d</b> adopted</p><ul class="space-y-2">`, count)
	for _, r := range rows {
		fmt.Fprintf(&b, `<li id="pet-%s" class="flex items-center gap-2"><span class="w-24">%s</span>`, r, r)
		if adopted[r] {
			fmt.Fprintf(&b, `<span class="text-muted-foreground">adopted</span><button id="return-%s" class="focus-visible:ring-[3px] hover:bg-accent bg-background border shadow-xs h-9 px-4 rounded-md inline-flex items-center justify-center text-sm font-medium" onclick="__nx.live.send(this,&#34;Return&#34;,[&#34;%s&#34;])">return</button>`, r, r)
		} else {
			fmt.Fprintf(&b, `<button id="adopt-%s" class="focus-visible:ring-[3px] hover:bg-primary/90 bg-primary text-primary-foreground shadow-xs h-9 px-4 rounded-md inline-flex items-center justify-center text-sm font-medium" onclick="__nx.live.send(this,&#34;Adopt&#34;,[&#34;%s&#34;])">adopt</button>`, r, r)
		}
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul></body></html>`)
	return b.String()
}

// On a 100-row page the common updates travel as their values: a count, a
// new row, and a branch switch whose markup the connection has seen before.
func TestPatchSizes(t *testing.T) {
	rows := make([]string, 100)
	for i := range rows {
		rows[i] = fmt.Sprintf("Pet%d", i)
	}
	size := func(d *differ, html string) int {
		p, _, full := d.next(html)
		if full {
			t.Fatal("expected a patch")
		}
		b, _ := marshal(p)
		return len(b)
	}
	start := func() *differ {
		d := &differ{}
		d.next(boardPage(rows, nil, 0))
		return d
	}

	if n := size(start(), boardPage(rows, nil, 1)); n > 20 {
		t.Errorf("a count change: %d bytes", n)
	}
	if n := size(start(), boardPage(append(append([]string{}, rows...), "Toby"), nil, 0)); n > 150 {
		t.Errorf("an appended row: %d bytes", n)
	}
	// The first adoption sends the return button's markup; once seen, an
	// adoption elsewhere — even after it left the page — sends only values.
	d := start()
	first := size(d, boardPage(rows, map[string]bool{"Pet10": true}, 1))
	size(d, boardPage(rows, nil, 0))
	again := size(d, boardPage(rows, map[string]bool{"Pet70": true}, 1))
	if again > first/2 || again > 160 {
		t.Errorf("adoption: first %d bytes, later %d bytes — markup seen before must not travel again", first, again)
	}
	t.Logf("count ≤20 B, appended row ≤150 B, adoption %d B first then %d B", first, again)
}
