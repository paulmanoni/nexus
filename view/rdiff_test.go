package view

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

type board struct {
	title string
	rows  []string
	open  map[string]bool
	note  string
}

// renderBoard writes a board the way instrumented templ code does: statics
// through the recorder, a loop of rows, a branch per row, a nested
// component, and plain markup a library wrote (the note).
func renderBoard(ctx context.Context, buf io.Writer, b board) {
	r := Record(ctx, buf)
	_ = r.S(buf, 1, `<html><body><h1 class="title text-lg font-semibold">`)
	io.WriteString(buf, b.title)
	_ = r.S(buf, 2, `</h1><ul class="space-y-2 rounded-md border">`)
	r.ForStart(buf)
	for _, row := range b.rows {
		r.Item(buf)
		_ = r.S(buf, 3, `<li class="flex items-center gap-2 px-3 py-2" id="`)
		io.WriteString(buf, row)
		_ = r.S(buf, 4, `"><span class="w-24 truncate">`)
		io.WriteString(buf, row)
		_ = r.S(buf, 5, `</span>`)
		r.Open(buf)
		if b.open[row] {
			_ = r.S(buf, 6, `<button class="btn btn-light">close</button>`)
		} else {
			_ = r.S(buf, 7, `<button class="btn btn-primary">open é 👋</button>`)
		}
		r.Close(buf)
		_ = r.S(buf, 8, `</li>`)
	}
	r.ForEnd(buf)
	_ = r.S(buf, 9, `</ul>`)
	r.Open(buf)
	io.WriteString(buf, `<footer class="text-muted-foreground">`+b.note+`</footer>`)
	r.Close(buf)
	_ = r.S(buf, 10, `</body></html>`)
}

func recordBoard(b board) (string, *rframe) {
	var buf bytes.Buffer
	ctx, rec := withRecorder(context.Background(), &buf)
	renderBoard(ctx, rec.w, b)
	root := rec.tree()
	return buf.String(), root
}

// A connection's worth of random changes round-trips through the tree
// differ, the Go mirror and the browser runtime (goja): every reply rebuilds
// exactly the render, and the changes travel small.
func TestTreeRoundTrip(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunString("globalThis.queueMicrotask = (f) => f();\n" + RuntimeJS()); err != nil {
		t.Fatal(err)
	}
	newTree, _ := goja.AssertFunction(vm.Get("__nx").ToObject(vm).Get("_tree"))
	tv, err := newTree(goja.Undefined())
	if err != nil {
		t.Fatal(err)
	}
	jsApply, _ := goja.AssertFunction(tv.ToObject(vm).Get("apply"))

	var server treeDiffer
	var browser treeMirror
	rng := rand.New(rand.NewSource(7))
	b := board{title: "Pets", rows: []string{"a", "b", "c", "d"}, open: map[string]bool{}, note: strings.Repeat("a long note ", 40)}
	var diffBytes, fullBytes int
	for i := 0; i < 300; i++ {
		next := b
		next.rows = append([]string(nil), b.rows...)
		next.open = map[string]bool{}
		for k, v := range b.open {
			next.open[k] = v
		}
		switch rng.Intn(6) {
		case 0:
			next.title = fmt.Sprintf("Pets %d", i)
		case 1:
			p := rng.Intn(len(next.rows) + 1)
			next.rows = append(next.rows[:p], append([]string{fmt.Sprintf("n%dé", i)}, next.rows[p:]...)...)
		case 2:
			if len(next.rows) > 1 {
				p := rng.Intn(len(next.rows))
				next.rows = append(next.rows[:p], next.rows[p+1:]...)
			}
		case 3:
			if len(next.rows) > 0 {
				r := next.rows[rng.Intn(len(next.rows))]
				next.open[r] = !next.open[r]
			}
		case 4:
			next.note += fmt.Sprintf(" %d", i)
		}
		html, root := recordBoard(next)
		if root.html() != html {
			t.Fatalf("the tree does not render the markup:\n got %s\nwant %s", root.html(), html)
		}
		msg, full := server.next(root)
		wire, _ := marshal(liveReply{Tree: msg, Full: full, Reset: i == 0})
		var decoded liveReply
		if err := json.Unmarshal(wire, &decoded); err != nil {
			t.Fatal(err)
		}
		got, err := browser.apply(decoded.Tree, decoded.Full, decoded.Reset)
		if err != nil || got != html {
			t.Fatalf("Go mirror, step %d (%v):\n got %s\nwant %s\nwire %s", i, err, got, html, wire)
		}
		res, err := jsApply(goja.Undefined(), vm.ToValue(string(wire)))
		if err != nil {
			t.Fatalf("browser runtime, step %d: %v\nwire %s", i, err, wire)
		}
		if res.String() != html {
			t.Fatalf("browser runtime, step %d:\n got %s\nwant %s", i, res.String(), html)
		}
		if i > 0 {
			diffBytes += len(wire)
			fullBytes += len(html)
		}
		b = next
	}
	if diffBytes*10 > fullBytes {
		t.Fatalf("changes took %d bytes against %d of markup", diffBytes, fullBytes)
	}
}

// A row toggled in a long list travels as that row's branch, and a statics
// id the connection already holds.
func TestTreeChangeIsSmall(t *testing.T) {
	rows := make([]string, 100)
	for i := range rows {
		rows[i] = fmt.Sprintf("pet-%d", i)
	}
	var server treeDiffer
	_, root := recordBoard(board{title: "Pets", rows: rows, open: map[string]bool{}})
	server.next(root)
	_, root = recordBoard(board{title: "Pets", rows: rows, open: map[string]bool{"pet-40": true}})
	msg, full := server.next(root)
	wire, _ := marshal(msg)
	if full || len(wire) > 120 {
		t.Fatalf("toggling one row sent %d bytes: %s", len(wire), wire)
	}
	_, root = recordBoard(board{title: "Pets", rows: append(append([]string{}, rows[:10]...), append([]string{"new"}, rows[10:]...)...), open: map[string]bool{"pet-40": true}})
	msg, _ = server.next(root)
	wire, _ = marshal(msg)
	if len(wire) > 120 || strings.Contains(string(wire), "<li") {
		t.Fatalf("inserting a row sent %d bytes: %s", len(wire), wire)
	}
}
