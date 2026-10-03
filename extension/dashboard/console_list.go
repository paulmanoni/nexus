package dashboard

import (
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/registry"
)

// Large apps register hundreds of services and thousands of endpoints, so
// list pages never render everything: rows are searched, sorted and paged
// on the server, and the URL carries that view (?q=&sort=&p=…) so a live
// refresh re-renders exactly the slice on screen.

const pageSize = 100

// listQuery is a list page's view: search, sort, page and the extra
// scoping params a page adds (transport, group, errors-only).
type listQuery struct {
	Path string
	Q    string
	Sort string
	Desc bool
	Page int
	Args map[string]string
}

func readListQuery(c *httpx.Ctx, path string, defaultSort string, extra ...string) listQuery {
	q := listQuery{Path: path, Q: strings.TrimSpace(c.Query("q")), Sort: c.Query("sort"), Args: map[string]string{}}
	if q.Sort == "" {
		q.Sort = defaultSort
	}
	if strings.HasPrefix(q.Sort, "-") {
		q.Desc = true
		q.Sort = q.Sort[1:]
	}
	q.Page, _ = strconv.Atoi(c.Query("p"))
	if q.Page < 1 {
		q.Page = 1
	}
	for _, k := range extra {
		if v := c.Query(k); v != "" {
			q.Args[k] = v
		}
	}
	return q
}

// href is this view with some params changed ("" removes one). Changing
// anything but the page goes back to page 1.
func (q listQuery) href(set ...string) string {
	v := url.Values{}
	if q.Q != "" {
		v.Set("q", q.Q)
	}
	if s := q.sortParam(); s != "" {
		v.Set("sort", s)
	}
	for k, a := range q.Args {
		v.Set(k, a)
	}
	page := q.Page
	resetPage := false
	for i := 0; i+1 < len(set); i += 2 {
		k, val := set[i], set[i+1]
		if k == "p" {
			page, _ = strconv.Atoi(val)
			continue
		}
		resetPage = true
		if val == "" {
			v.Del(k)
		} else {
			v.Set(k, val)
		}
	}
	if resetPage {
		page = 1
	}
	if page > 1 {
		v.Set("p", strconv.Itoa(page))
	}
	if len(v) == 0 {
		return q.Path
	}
	return q.Path + "?" + v.Encode()
}

func (q listQuery) sortParam() string {
	if q.Sort == "" {
		return ""
	}
	if q.Desc {
		return "-" + q.Sort
	}
	return q.Sort
}

// sortHref toggles a column: first click sorts it (numbers descending),
// the next reverses it.
func (q listQuery) sortHref(col string, numeric bool) string {
	next := col
	if q.Sort == col {
		if !q.Desc {
			next = "-" + col
		}
	} else if numeric {
		next = "-" + col
	}
	return q.href("sort", next)
}

func (q listQuery) sortMark(col string) string {
	if q.Sort != col {
		return ""
	}
	if q.Desc {
		return " ↓"
	}
	return " ↑"
}

// matches reports whether every word of the search hits the text.
func (q listQuery) matches(parts ...string) bool {
	if q.Q == "" {
		return true
	}
	text := strings.ToLower(strings.Join(parts, " "))
	for _, w := range strings.Fields(strings.ToLower(q.Q)) {
		if !strings.Contains(text, w) {
			return false
		}
	}
	return true
}

// pager is the slice of a filtered list on screen.
type pager struct {
	Total, From, To, Page, Pages int
}

func paginate(total, page int) pager {
	pages := (total + pageSize - 1) / pageSize
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	from := (page - 1) * pageSize
	to := from + pageSize
	if to > total {
		to = total
	}
	return pager{Total: total, From: from, To: to, Page: page, Pages: pages}
}

// ---- endpoints ----

// endpointGroup is one entry in the Endpoints rail: a module, or the
// service when the app declares no modules.
type endpointGroup struct {
	Key       string
	Endpoints int
	Errors    int64
}

func groupOf(e registry.Endpoint) string {
	if e.Module != "" {
		return e.Module
	}
	if e.Service != "" {
		return e.Service
	}
	return "default"
}

type endpointsView struct {
	Q          listQuery
	GroupLabel string // "Modules", or "Services" when the app declares none
	Groups     []endpointGroup
	All        int
	Rows       []registry.Endpoint
	Pager      pager
}

func (st *consoleState) endpointsView(q listQuery) endpointsView {
	v := endpointsView{Q: q, All: len(st.Endpoints), GroupLabel: "Services"}
	transport, group := q.Args["t"], q.Args["g"]
	groups := map[string]*endpointGroup{}
	var rows []registry.Endpoint
	for _, e := range st.Endpoints {
		s := st.stat(e)
		g := groupOf(e)
		eg := groups[g]
		if eg == nil {
			eg = &endpointGroup{Key: g}
			groups[g] = eg
		}
		eg.Endpoints++
		eg.Errors += s.Errors
		if e.Module != "" {
			v.GroupLabel = "Modules"
		}
		switch {
		case transport == "view":
			if e.Tags[registry.ViewTag] == "" {
				continue
			}
		case transport != "" && string(e.Transport) != transport:
			continue
		}
		if group != "" && g != group {
			continue
		}
		if !q.matches(e.Name, e.Path, e.Service, e.Module, e.Description, string(e.Transport), e.Method, strings.Join(e.Middleware, " "), e.Tags[registry.ViewTag], e.Tags[registry.ViewComponentTag]) {
			continue
		}
		rows = append(rows, e)
	}
	for _, g := range groups {
		v.Groups = append(v.Groups, *g)
	}
	sort.Slice(v.Groups, func(i, j int) bool { return v.Groups[i].Key < v.Groups[j].Key })

	less := func(a, b registry.Endpoint) bool {
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.Name < b.Name
	}
	switch q.Sort {
	case "reqs":
		less = func(a, b registry.Endpoint) bool { return st.stat(a).Count < st.stat(b).Count }
	case "errs":
		less = func(a, b registry.Endpoint) bool { return st.stat(a).Errors < st.stat(b).Errors }
	case "last":
		less = func(a, b registry.Endpoint) bool { return st.stat(a).LastAt.Before(st.stat(b).LastAt) }
	case "name":
		less = func(a, b registry.Endpoint) bool { return endpointTitle(a) < endpointTitle(b) }
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if q.Desc {
			return less(rows[j], rows[i])
		}
		return less(rows[i], rows[j])
	})
	v.Pager = paginate(len(rows), q.Page)
	v.Rows = rows[v.Pager.From:v.Pager.To]
	return v
}

// ---- services ----

type serviceRow struct {
	registry.Service
	Endpoints    int
	Reqs, Errors int64
}

type servicesView struct {
	Q     listQuery
	Rows  []serviceRow
	Pager pager
}

func (st *consoleState) servicesView(q listQuery) servicesView {
	type agg struct {
		n          int
		reqs, errs int64
	}
	by := map[string]*agg{}
	for _, e := range st.Endpoints {
		a := by[e.Service]
		if a == nil {
			a = &agg{}
			by[e.Service] = a
		}
		s := st.stat(e)
		a.n++
		a.reqs += s.Count
		a.errs += s.Errors
	}
	// A service's resources are recorded on either side (its ResourceDeps,
	// or the resource's AttachedTo); show the union.
	uses := map[string][]string{}
	for _, r := range st.Resources {
		for _, svc := range r.AttachedTo {
			uses[svc] = append(uses[svc], r.Name)
		}
	}
	var rows []serviceRow
	for _, s := range st.Services {
		s.ResourceDeps = union(s.ResourceDeps, uses[s.Name])
		if !q.matches(s.Name, s.Description, s.Deployment, strings.Join(s.ResourceDeps, " "), strings.Join(s.ServiceDeps, " ")) {
			continue
		}
		r := serviceRow{Service: s}
		if a := by[s.Name]; a != nil {
			r.Endpoints, r.Reqs, r.Errors = a.n, a.reqs, a.errs
		}
		rows = append(rows, r)
	}
	less := func(a, b serviceRow) bool { return a.Name < b.Name }
	switch q.Sort {
	case "eps":
		less = func(a, b serviceRow) bool { return a.Endpoints < b.Endpoints }
	case "reqs":
		less = func(a, b serviceRow) bool { return a.Reqs < b.Reqs }
	case "errs":
		less = func(a, b serviceRow) bool { return a.Errors < b.Errors }
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if q.Desc {
			return less(rows[j], rows[i])
		}
		return less(rows[i], rows[j])
	})
	p := paginate(len(rows), q.Page)
	return servicesView{Q: q, Rows: rows[p.From:p.To], Pager: p}
}

// ---- resources ----

type resourcesView struct {
	Q     listQuery
	Rows  []registry.ResourceSnapshot
	Down  int
	Pager pager
}

func (st *consoleState) resourcesView(q listQuery) resourcesView {
	var rows []registry.ResourceSnapshot
	v := resourcesView{Q: q}
	onlyDown := q.Args["down"] != ""
	for _, r := range st.Resources {
		if !r.Healthy {
			v.Down++
		}
		if onlyDown && r.Healthy {
			continue
		}
		if !q.matches(r.Name, string(r.Kind), r.Description, strings.Join(r.AttachedTo, " ")) {
			continue
		}
		rows = append(rows, r)
	}
	// Down first: the list is for noticing what is broken.
	sort.SliceStable(rows, func(i, j int) bool { return !rows[i].Healthy && rows[j].Healthy })
	v.Pager = paginate(len(rows), q.Page)
	v.Rows = rows[v.Pager.From:v.Pager.To]
	return v
}

// ---- traces ----

func filterRequests(rows []requestRow, q listQuery) ([]requestRow, pager) {
	var out []requestRow
	for _, r := range rows {
		if q.matches(r.Method, r.Path, r.Endpoint, r.Service, r.Transport, strconv.Itoa(r.Status), r.Error) {
			out = append(out, r)
		}
	}
	p := paginate(len(out), q.Page)
	return out[p.From:p.To], p
}

func union(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, x := range append(append([]string{}, a...), b...) {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}
