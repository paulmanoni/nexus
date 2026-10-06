package viewgen

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A live page that uses view.Assign but isn't — or can't be — tracked is
// pointed out: the field that keeps it rendering in full, the template's
// read of it, and a service called from Render on a tracked page.
func TestTrackWarnings(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":  "module example.com/app\n\ngo 1.26\n",
		"main.go": "package main\n\nfunc main() {}\n",
		"state/state.go": `package state

import "github.com/paulmanoni/nexus/v2/view"

type Search struct{ Query *view.Signal[string] }
`,
		"orders/orders.go": `package orders

import "github.com/paulmanoni/nexus/v2/view"

type Orders struct {
	Status view.Assign[string]
	Total  int
	Note   *view.Signal[string]
}

type Board struct {
	Rows  view.Assign[[]string]
	Feed  view.Stream[string]
	Photo view.Upload
}

type Plain struct{ Count int }

type List[R any] struct {
	Rows  view.Assign[[]R]
	spec  *Spec ` + "`view:\"-\"`" + `
	Total int
}

type Spec struct{ Title string }

type Store struct{}

func (*Store) Count() int { return 0 }
`,
		"orders/orders.templ": `package orders

import (
	"strconv"

	"example.com/app/state"
	"github.com/paulmanoni/nexus/v2/view"
)

templ (o *Orders) Render() {
	<h1>{ o.Status.Get() }</h1>
	<p>{ strconv.Itoa(o.Total) }</p>
}

templ (b *Board) Render() {
	<p>{ strconv.Itoa(view.Use[*Store](ctx).Count()) }</p>
	{{ q := view.Use[*state.Search](ctx).Query }}
	<input value={ q.Get() }/>
	for _, r := range b.Rows.Get() {
		<li>{ r }</li>
	}
}

templ (l *List[R]) Render() {
	<h1>{ l.spec.Title }</h1>
	<p>{ strconv.Itoa(l.Total) }</p>
}

templ (p *Plain) Render() {
	<p>{ strconv.Itoa(p.Count) }</p>
	<p>{ strconv.Itoa(view.Use[*Store](ctx).Count()) }</p>
}
`,
	})
	plan, err := GenerateWith(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range plan.Warnings {
		rel, _ := filepath.Rel(root, filepath.FromSlash(w.File))
		got = append(got, filepath.ToSlash(rel)+":"+strconv.Itoa(w.Line)+":"+strconv.Itoa(w.Col)+": "+w.Msg)
	}
	want := []string{
		"orders/orders.templ:12:22: o.Total is not a view.Assign",
		"orders/orders.templ:16:20: Board calls *Store in Render",
		"orders/orders.templ:26:22: l.Total is not a view.Assign: List renders in full",
		"orders/orders.go:22:2: List.Total keeps the live page List rendering in full",
		"orders/orders.go:7:2: Orders.Total keeps the live page Orders rendering in full",
	}
	if len(got) != len(want) {
		t.Fatalf("warnings:\n%s", strings.Join(got, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(got[i], w) {
			t.Fatalf("warning %d = %s\nwant prefix %s", i, got[i], w)
		}
	}
}

// A struct a page embeds by value is part of it, as at run time: its
// Assigns are the page's, from this package or another of the module, and
// only a plain field in it is pointed out.
func TestTrackWarningsEmbedded(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":  "module example.com/app\n\ngo 1.26\n",
		"main.go": "package main\n\nfunc main() {}\n",
		"kit/list.go": `package kit

import "github.com/paulmanoni/nexus/v2/view"

type List struct {
	view.LiveView
	Page view.Assign[int]
	base string ` + "`view:\"-\"`" + `
}

type Leaky struct {
	Page  view.Assign[int]
	Cache map[string]int
}
`,
		"orders/orders.go": `package orders

import (
	"example.com/app/kit"
	"github.com/paulmanoni/nexus/v2/view"
)

type paging struct {
	Size view.Assign[int]
}

type Orders struct {
	kit.List
	paging
	Rows view.Assign[[]string]
}

type Archive struct {
	kit.Leaky
	Rows view.Assign[[]string]
}
`,
		"orders/orders.templ": `package orders

import "strconv"

templ (o *Orders) Render() {
	<p>{ strconv.Itoa(o.Page.Get()) } { strconv.Itoa(o.Size.Get()) }</p>
}

templ (a *Archive) Render() {
	<p>{ strconv.Itoa(len(a.Cache)) }</p>
}
`,
	})
	plan, err := GenerateWith(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range plan.Warnings {
		rel, _ := filepath.Rel(root, filepath.FromSlash(w.File))
		got = append(got, filepath.ToSlash(rel)+":"+strconv.Itoa(w.Line)+": "+w.Msg)
	}
	want := []string{
		"orders/orders.templ:10: a.Cache is not a view.Assign",
		"kit/list.go:13: Archive.Cache keeps the live page Archive rendering in full",
	}
	if len(got) != len(want) {
		t.Fatalf("warnings:\n%s", strings.Join(got, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(got[i], w) {
			t.Fatalf("warning %d = %s\nwant prefix %s", i, got[i], w)
		}
	}
}
