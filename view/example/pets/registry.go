package pets

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/view"
	kit "github.com/paulmanoni/nexus/v2/view/ui"
)

// Registry is a live page built from the view/ui kit: a data table whose
// paging, search and sorting run on the server, kind tabs, row actions, a
// confirm dialog and a toast.
type Registry struct {
	Rows    []Pet
	Adopted map[string]bool
	Kinds   []KindCount
	Total   int // pets matching the kind and the query

	Kind  string
	Query string
	Sort  string
	Desc  bool
	Page  int
	Size  int

	Confirm string // the pet whose removal awaits confirmation
	Flash   string
	FlashN  int
}

// RegistryQuery is the table's search form.
type RegistryQuery struct {
	Q    string `form:"q"`
	Size int    `form:"size"`
}

func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) Mount(ctx context.Context, store *Store) error {
	r.Page, r.Size, r.Sort = 1, 5, "name"
	r.load(store)
	return nil
}

// Search is the search form's event: the query and the page size.
func (r *Registry) Search(ctx context.Context, store *Store, in RegistryQuery) error {
	r.Query = strings.TrimSpace(in.Q)
	if in.Size == 5 || in.Size == 10 {
		r.Size = in.Size
	}
	r.Page = 1
	r.load(store)
	return nil
}

func (r *Registry) SetPage(ctx context.Context, store *Store, page int) error {
	r.Page = page
	r.load(store)
	return nil
}

// SortBy sorts by a column; the sorted column again flips the order.
func (r *Registry) SortBy(ctx context.Context, store *Store, key string) error {
	if key != "name" && key != "kind" {
		return fmt.Errorf("no column %q", key)
	}
	r.Desc = key == r.Sort && !r.Desc
	r.Sort = key
	r.load(store)
	return nil
}

func (r *Registry) SetKind(ctx context.Context, store *Store, kind string) error {
	r.Kind, r.Page = kind, 1
	r.load(store)
	return nil
}

func (r *Registry) AskRemove(ctx context.Context, store *Store, name string) error {
	if !store.Has(name) {
		return fmt.Errorf("no pet named %q", name)
	}
	r.Confirm = name
	return nil
}

func (r *Registry) CancelRemove(ctx context.Context) error {
	r.Confirm = ""
	return nil
}

func (r *Registry) Remove(ctx context.Context, store *Store) error {
	if r.Confirm == "" || !store.Remove(r.Confirm) {
		return nexus.Invalid().Global("nothing to remove")
	}
	r.Flash, r.FlashN, r.Confirm = r.Confirm+" was removed", r.FlashN+1, ""
	r.load(store)
	view.Broadcast(ctx, adoptions, "")
	return nil
}

// load fills the page's rows from the store: filtered, sorted, paged.
func (r *Registry) load(store *Store) {
	all := store.All()
	r.Kinds = kindChart(all, nil).Kinds
	q := strings.ToLower(r.Query)
	var rows []Pet
	for _, p := range all {
		if (r.Kind == "" || p.Kind == r.Kind) && (q == "" || strings.Contains(strings.ToLower(p.Name), q) || strings.Contains(p.Kind, q)) {
			rows = append(rows, p)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].Name, rows[j].Name
		if r.Sort == "kind" {
			a, b = rows[i].Kind+a, rows[j].Kind+b
		}
		if r.Desc {
			return a > b
		}
		return a < b
	})
	r.Total = len(rows)
	last := max((r.Total+r.Size-1)/r.Size, 1)
	r.Page = min(max(r.Page, 1), last)
	from := (r.Page - 1) * r.Size
	r.Rows = rows[min(from, len(rows)):min(from+r.Size, len(rows))]
	r.Adopted = store.Adopted()
}

// table is the DataTable's state and events; sorting and paging cover the
// table with a loader until the reply.
func (r *Registry) table() kit.TableProps {
	return kit.TableProps{
		ID:      "pets",
		Columns: []kit.Column{{Key: "name", Label: "Name", Sortable: true}, {Key: "kind", Label: "Kind", Sortable: true}, {Label: "Status"}},
		Rows:    len(r.Rows), Total: r.Total, Page: r.Page, PageSize: r.Size, PageSizes: []int{5, 10},
		Sort: r.Sort, Desc: r.Desc, Query: r.Query,
		OnSearch:          view.Change(r.Search),
		SearchPlaceholder: "Search pets…",
		OnSort:            func(key string) templ.ComponentScript { return kit.Loading(view.Send(r.SortBy, key), "pets") },
		OnPage:            func(n int) templ.ComponentScript { return kit.Loading(view.Send(r.SetPage, n), "pets") },
		Actions:           true,
	}
}

func (r *Registry) tabs() []kit.Tab {
	total := 0
	for _, k := range r.Kinds {
		total += k.Count
	}
	tabs := []kit.Tab{{ID: "kind-all", Label: "All", Count: strconv.Itoa(total), Active: r.Kind == "",
		OnSelect: kit.Loading(view.Send(r.SetKind, ""), "pets")}}
	for _, k := range r.Kinds {
		tabs = append(tabs, kit.Tab{ID: "kind-" + k.Kind, Label: k.Kind, Count: strconv.Itoa(k.Count), Active: r.Kind == k.Kind,
			OnSelect: kit.Loading(view.Send(r.SetKind, k.Kind), "pets")})
	}
	return tabs
}

func (r *Registry) actions(p Pet) []kit.MenuItem {
	return []kit.MenuItem{
		{Label: "Open the board", Icon: kit.Icon("chevron-right"), Href: "/board", Nav: true},
		{Label: "Copy link", Icon: kit.Icon("link"), Copy: "/board#pet-" + p.Name},
		{Separator: true},
		{ID: "remove-" + p.Name, Label: "Remove", Icon: kit.Icon("trash"), Danger: true, OnClick: view.Send(r.AskRemove, p.Name)},
	}
}
