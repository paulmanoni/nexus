package dev

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/trace"
)

func builtinPanels() []Panel {
	return []Panel{
		{Name: "Request", Render: requestPanel},
		{Name: "SQL", Render: sqlPanel},
		{Name: "Timeline", Render: timelinePanel},
		{Name: "Logs", Render: logsPanel},
	}
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }

func requestPanel(r *Request) Section {
	s := Section{Summary: fmt.Sprintf("%d · %s", r.Status, ms(r.Duration))}
	if r.Status >= 500 {
		s.Tone = "error"
	} else if r.Status >= 400 {
		s.Tone = "warn"
	}
	var routes []string
	for _, sp := range r.Spans {
		if sp.ParentID == "" || sp.Kind == string(trace.KindRequestStart) {
			if sp.Endpoint != "" && !slices.Contains(routes, sp.Endpoint) {
				routes = append(routes, sp.Endpoint)
			}
		}
	}
	s.Stats = []Stat{
		{Label: "Method", Value: r.Method},
		{Label: "Path", Value: r.Path},
		{Label: "Status", Value: strconv.Itoa(r.Status), Tone: s.Tone},
		{Label: "Time", Value: ms(r.Duration)},
	}
	if len(routes) > 0 {
		s.Stats = append(s.Stats, Stat{Label: "Handler", Value: strings.Join(routes, ", ")})
	}
	t := &Table{Columns: []Column{{Title: "Where"}, {Title: "Name"}, {Title: "Value", Code: true}}}
	for _, k := range sortedKeys(r.Query) {
		for _, v := range r.Query[k] {
			t.Rows = append(t.Rows, []any{"query", k, v})
		}
	}
	for _, k := range sortedKeys(r.Header) {
		for _, v := range r.Header[k] {
			if secretHeader(k) {
				v = "••••••"
			}
			t.Rows = append(t.Rows, []any{"header", k, v})
		}
	}
	s.Table = t
	return s
}

func secretHeader(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Cookie", "Authorization", "Proxy-Authorization", "X-Xsrf-Token", "X-Csrf-Token":
		return true
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// query is a statement a request ran, as its span recorded it.
type query struct {
	sql     string
	ms      float64
	err     string
	start   int64
	refused bool // the ORM refused it: never sent
}

func queries(r *Request) []query {
	var out []query
	for _, sp := range r.Spans {
		if sp.Name != "sql" {
			continue
		}
		q := query{err: sp.Error, start: sp.StartMs, ms: float64(sp.DurationMs), refused: sp.Attrs["orm.refused"] == true}
		if s, ok := sp.Attrs["sql"].(string); ok {
			q.sql = s
		}
		if v, ok := sp.Attrs["ms"].(string); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				q.ms = f
			}
		}
		out = append(out, q)
	}
	return out
}

func sqlPanel(r *Request) Section {
	qs := queries(r)
	seen := map[string]int{}
	var total, slowest float64
	refusedN := 0
	for _, q := range qs {
		if q.refused {
			refusedN++
			q.ms = 0
			continue
		}
		seen[q.sql]++
		total += q.ms
		slowest = max(slowest, q.ms)
	}
	dup := 0
	for _, n := range seen {
		if n > 1 {
			dup += n
		}
	}
	s := Section{Summary: fmt.Sprintf("%d queries · %.1f ms", len(qs)-refusedN, total)}
	s.Stats = []Stat{
		{Label: "Queries", Value: strconv.Itoa(len(qs) - refusedN)},
		{Label: "Total", Value: fmt.Sprintf("%.1f ms", total)},
		{Label: "Slowest", Value: fmt.Sprintf("%.1f ms", slowest)},
	}
	if dup > 0 {
		s.Tone = "warn"
		s.Stats = append(s.Stats, Stat{Label: "Repeated", Value: strconv.Itoa(dup), Tone: "warn"})
	}
	if refusedN > 0 {
		s.Tone = "error"
		s.Summary += fmt.Sprintf(" · %d refused", refusedN)
		s.Stats = append(s.Stats, Stat{Label: "Refused by the ORM", Value: strconv.Itoa(refusedN), Tone: "error"})
	}
	for _, sp := range r.Spans {
		if q, ok := sp.Attrs["orm.repeated"].(string); ok {
			s.Text = "N+1: " + q + "\n" + fmt.Sprint(sp.Attrs["orm.hint"])
			s.Tone = "error"
		}
	}
	t := &Table{Columns: []Column{{Title: "#", Num: true}, {Title: "ms", Bar: true}, {Title: "Runs", Num: true}, {Title: "SQL", Code: true, Lang: "sql"}}}
	for i, q := range qs {
		tone := ""
		switch {
		case q.err != "":
			tone = "error"
		case seen[q.sql] > 1:
			tone = "warn"
		}
		sql := q.sql
		if q.err != "" {
			sql += "\n-- " + q.err
		}
		ms, runs := fmt.Sprintf("%.2f", q.ms), seen[q.sql]
		if q.refused {
			ms = "0"
		}
		t.Rows = append(t.Rows, []any{i + 1, ms, runs, sql})
		t.Tones = append(t.Tones, tone)
	}
	s.Table = t
	return s
}

type timelineRow struct {
	Name        string
	SQL, Err    bool
	Ms          int64
	Left, Width float64
}

func (r timelineRow) style() templ.SafeCSS {
	return templ.SafeCSS(fmt.Sprintf("left:%.2f%%;width:%.2f%%", r.Left, r.Width))
}

func timelinePanel(r *Request) Section {
	total := max(r.Duration.Milliseconds(), 1)
	for _, sp := range r.Spans {
		total = max(total, sp.StartMs+sp.DurationMs)
	}
	rows := make([]timelineRow, 0, len(r.Spans))
	for _, sp := range r.Spans {
		row := timelineRow{Name: sp.Name, SQL: sp.Name == "sql", Err: sp.Error != "", Ms: sp.DurationMs,
			Left:  float64(sp.StartMs) / float64(total) * 100,
			Width: max(float64(sp.DurationMs)/float64(total)*100, 0.4)}
		if q, ok := sp.Attrs["sql"].(string); ok {
			row.Name = q
		}
		rows = append(rows, row)
	}
	return Section{Summary: fmt.Sprintf("%d spans", len(rows)), View: timeline(rows)}
}

func logsPanel(r *Request) Section {
	logs := r.Logs()
	s := Section{Summary: fmt.Sprintf("%d records", len(logs))}
	t := &Table{Columns: []Column{{Title: "Time"}, {Title: "Level"}, {Title: "Message"}, {Title: "Attributes", Code: true}}}
	for _, l := range logs {
		tone := ""
		if l.Level >= 8 {
			tone, s.Tone = "error", "error"
		} else if l.Level >= 4 {
			tone = "warn"
			if s.Tone == "" {
				s.Tone = "warn"
			}
		}
		attrs, _ := json.Marshal(l.Attrs)
		if len(l.Attrs) == 0 {
			attrs = nil
		}
		t.Rows = append(t.Rows, []any{l.Time.Format("15:04:05.000"), l.Level.String(), l.Message, string(attrs)})
		t.Tones = append(t.Tones, tone)
	}
	s.Table = t
	return s
}

var (
	//go:embed toolbar.js
	toolbarJS []byte
	//go:embed toolbar.css
	toolbarCSS []byte
)

// MountToolbar serves the toolbar's script and its view of each request.
func MountToolbar(r httpx.Router) {
	r.GET("/__nexus/toolbar/toolbar.js", func(c *httpx.Ctx) {
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "text/javascript; charset=utf-8", toolbarJS)
	})
	r.GET("/__nexus/toolbar/toolbar.css", func(c *httpx.Ctx) {
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "text/css; charset=utf-8", toolbarCSS)
	})
	// Where nexus dev's rebuild stands, for the handle and the drawer.
	r.GET("/__nexus/toolbar/build", func(c *httpx.Ctx) {
		b := BuildState()
		c.Writer.Header().Set("Cache-Control", "no-store")
		c.JSON(http.StatusOK, httpx.H{"state": b.State, "seconds": int(time.Since(b.Since).Seconds()), "output": b.Output})
	})
	// What a page did after its load, from the after'th entry on.
	r.GET("/__nexus/toolbar/requests/:id/children", func(c *httpx.Ctx) {
		page := requests.get(c.Param("id"))
		if page == nil {
			c.JSON(http.StatusNotFound, httpx.H{"error": "request not kept"})
			return
		}
		after, _ := strconv.Atoi(c.Query("after"))
		page.mu.Lock()
		ids := slices.Clone(page.children[min(max(after, 0), len(page.children)):])
		next := len(page.children)
		page.mu.Unlock()
		items := []httpx.H{}
		for _, id := range ids {
			if r := requests.get(id); r != nil {
				items = append(items, httpx.H{"id": r.ID, "label": label(r)})
			}
		}
		c.JSON(http.StatusOK, httpx.H{"next": next, "items": items})
	})
	// A request's view: the drawer's markup, or JSON for tools.
	r.GET("/__nexus/toolbar/requests/:id", func(c *httpx.Ctx) {
		req := requests.get(c.Param("id"))
		if req == nil {
			gone := "request not kept: the toolbar keeps the latest " + strconv.Itoa(historySize)
			c.JSON(http.StatusNotFound, httpx.H{"error": gone})
			return
		}
		views := renderPanels(req)
		if strings.Contains(c.Request.Header.Get("Accept"), "application/json") {
			c.JSON(http.StatusOK, renderJSON(c.Request.Context(), req, views))
			return
		}
		c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := requestView(req, views, len(queries(req))).Render(c.Request.Context(), c.Writer); err != nil {
			c.JSON(http.StatusInternalServerError, httpx.H{"error": err.Error()})
		}
	})
}

type panelView struct {
	Name string `json:"name"`
	Section
	Error string `json:"error,omitempty"`
}

func renderPanels(r *Request) []panelView {
	var views []panelView
	for _, p := range allPanels() {
		views = append(views, renderPanel(p, r))
	}
	return views
}

func renderJSON(ctx context.Context, r *Request, views []panelView) httpx.H {
	for i, v := range views {
		if v.View != nil {
			var b strings.Builder
			if err := v.View.Render(ctx, &b); err == nil {
				views[i].HTML = b.String() + v.HTML
			}
		}
	}
	return httpx.H{
		"id":      r.ID,
		"method":  r.Method,
		"path":    r.Path,
		"status":  r.Status,
		"ms":      float64(r.Duration.Microseconds()) / 1000,
		"queries": len(queries(r)),
		"panels":  views,
	}
}

func worstTone(views []panelView) string {
	t := ""
	for _, v := range views {
		switch {
		case v.Tone == "error" || v.Error != "":
			return "error"
		case v.Tone == "warn":
			t = "warn"
		}
	}
	return t
}

func rowTone(t *Table, i int) string {
	if i < len(t.Tones) {
		return t.Tones[i]
	}
	return ""
}

// renderPanel runs a panel, a panic in it shown as its error.
func renderPanel(p Panel, r *Request) (v panelView) {
	v.Name = p.Name
	defer func() {
		if e := recover(); e != nil {
			v.Error = fmt.Sprint(e)
		}
	}()
	if p.Render != nil {
		v.Section = p.Render(r)
	}
	return v
}

func statusTone(status int) string {
	switch {
	case status >= 500:
		return "error"
	case status >= 400:
		return "warn"
	case status >= 300:
		return "redirect"
	}
	return "ok"
}

// barWidth is a Bar column cell's share of the column's largest number.
func barWidth(t *Table, col int, cell any) templ.SafeCSS {
	num := func(v any) float64 {
		f, _ := strconv.ParseFloat(strings.TrimSpace(fmt.Sprint(v)), 64)
		return f
	}
	top := 0.0
	for _, row := range t.Rows {
		if col < len(row) {
			top = max(top, num(row[col]))
		}
	}
	w := 0.0
	if top > 0 {
		w = num(cell) / top * 100
	}
	return templ.SafeCSS(fmt.Sprintf("width:%.1f%%", max(w, 2)))
}

type sqlToken struct{ text, kind string }

var sqlKeywords = map[string]bool{}

func init() {
	for _, k := range strings.Fields(`SELECT FROM WHERE AND OR NOT IN IS NULL AS ON JOIN LEFT RIGHT INNER OUTER FULL CROSS
		GROUP BY ORDER HAVING LIMIT OFFSET INSERT INTO VALUES UPDATE SET DELETE RETURNING DISTINCT COUNT SUM AVG MIN MAX
		CASE WHEN THEN ELSE END EXISTS LIKE ILIKE BETWEEN UNION ALL ASC DESC WITH CREATE TABLE INDEX DROP ALTER
		PRIMARY KEY DEFAULT TRUE FALSE COALESCE CAST LATERAL`) {
		sqlKeywords[k] = true
	}
}

// sqlTokens splits a statement for highlighting: keywords, strings,
// numbers and placeholders get a class; everything else is plain.
func sqlTokens(q string) []sqlToken {
	var out []sqlToken
	plain := func(s string) {
		if n := len(out); n > 0 && out[n-1].kind == "" {
			out[n-1].text += s
			return
		}
		out = append(out, sqlToken{text: s})
	}
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for j < len(q) && q[j] != c {
				j++
			}
			j = min(j+1, len(q))
			kind := "str"
			if c != '\'' {
				kind = "ident"
			}
			out = append(out, sqlToken{q[i:j], kind})
			i = j
		case c == '?' || c == '$' && i+1 < len(q) && q[i+1] >= '0' && q[i+1] <= '9':
			j := i + 1
			for j < len(q) && q[j] >= '0' && q[j] <= '9' {
				j++
			}
			out = append(out, sqlToken{q[i:j], "arg"})
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < len(q) && (q[j] >= '0' && q[j] <= '9' || q[j] == '.') {
				j++
			}
			out = append(out, sqlToken{q[i:j], "numlit"})
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(q) && (q[j] == '_' || q[j] >= 'a' && q[j] <= 'z' || q[j] >= 'A' && q[j] <= 'Z' || q[j] >= '0' && q[j] <= '9') {
				j++
			}
			if w := q[i:j]; sqlKeywords[strings.ToUpper(w)] {
				out = append(out, sqlToken{w, "kw"})
			} else {
				plain(w)
			}
			i = j
		default:
			plain(q[i : i+1])
			i++
		}
	}
	return out
}

// label is how a request reads in the toolbar's list.
func label(r *Request) string {
	return fmt.Sprintf("%s %s · %d · %s · %dq", r.Method, r.Path, r.Status, ms(r.Duration), len(queries(r)))
}
