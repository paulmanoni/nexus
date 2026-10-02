package dashboard

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/trace"
)

// vueBundle is the topology canvas's built entry, read from the Vite
// manifest ui/dist/.vite/manifest.json. Zero when the bundle was never built.
type vueBundle struct {
	JS  string
	CSS []string
}

func vueEntry() vueBundle {
	raw, err := fs.ReadFile(uiFS, "ui/dist/.vite/manifest.json")
	if err != nil {
		return vueBundle{}
	}
	var m map[string]struct {
		File    string   `json:"file"`
		CSS     []string `json:"css"`
		IsEntry bool     `json:"isEntry"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return vueBundle{}
	}
	for _, e := range m {
		if e.IsEntry {
			b := vueBundle{JS: Prefix + "/" + e.File}
			for _, c := range e.CSS {
				b.CSS = append(b.CSS, Prefix+"/"+c)
			}
			return b
		}
	}
	return vueBundle{}
}

// requestRow is one request in the Traces tab, folded from its events.
type requestRow struct {
	TraceID    string
	At         time.Time
	Transport  string
	Method     string
	Path       string
	Service    string
	Endpoint   string
	Status     int
	DurationMs int64
	Error      string
	Done       bool
}

// maxTraceRows bounds the fold of the trace buffer (the buffer is bounded
// too, by trace_capacity; this only guards a very large one).
const maxTraceRows = 5000

// recentRequests folds the trace buffer into one row per request, newest
// first.
func recentRequests(bus *trace.Bus, onlyErrors bool) []requestRow {
	byTrace := map[string]*requestRow{}
	var order []string
	for _, e := range bus.Recent() {
		if e.TraceID == "" {
			continue
		}
		r, ok := byTrace[e.TraceID]
		if !ok {
			if e.Kind != trace.KindRequestStart && e.Kind != trace.KindRequestOp && e.Kind != trace.KindRequestEnd {
				continue
			}
			r = &requestRow{TraceID: e.TraceID, At: e.Timestamp}
			byTrace[e.TraceID] = r
			order = append(order, e.TraceID)
		}
		if r.Transport == "" {
			r.Transport = e.Transport
		}
		if r.Method == "" {
			r.Method = e.Method
		}
		if r.Path == "" {
			r.Path = e.Path
		}
		// request.op names the operation that actually ran (a GraphQL
		// field, not "POST /graphql"); other events only fill gaps.
		if e.Kind == trace.KindRequestOp || r.Service == "" {
			if e.Service != "" {
				r.Service = e.Service
			}
		}
		if e.Kind == trace.KindRequestOp || r.Endpoint == "" {
			if e.Endpoint != "" {
				r.Endpoint = e.Endpoint
			}
		}
		switch e.Kind {
		case trace.KindRequestStart:
			if !e.Timestamp.IsZero() {
				r.At = e.Timestamp
			}
		case trace.KindRequestEnd:
			r.Done = true
			r.Status = e.Status
			r.DurationMs = e.DurationMs
			if e.Error != "" {
				r.Error = e.Error
			}
		}
	}
	out := make([]requestRow, 0, len(order))
	for i := len(order) - 1; i >= 0 && len(out) < maxTraceRows; i-- {
		r := byTrace[order[i]]
		if onlyErrors && !r.failed() {
			continue
		}
		out = append(out, *r)
	}
	return out
}

func (r requestRow) failed() bool { return r.Error != "" || r.Status >= 500 }

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// traceExtent is the waterfall's time axis in ms (never zero).
func traceExtent(spans []*trace.SpanNode) int64 {
	var end int64 = 1
	for _, s := range spans {
		if e := s.StartMs + s.DurationMs; e > end {
			end = e
		}
	}
	return end
}

// spanDepth is how far a span nests below the root, for indentation.
func spanDepth(spans []*trace.SpanNode, s *trace.SpanNode) int {
	parent := map[string]string{}
	for _, x := range spans {
		parent[x.SpanID] = x.ParentID
	}
	d := 0
	for p := s.ParentID; p != "" && d < 32; p = parent[p] {
		if _, ok := parent[p]; !ok {
			break
		}
		d++
	}
	return d
}

func pct(part, whole int64) string {
	if whole <= 0 {
		return "0%"
	}
	return strconv.FormatFloat(float64(part)*100/float64(whole), 'f', 3, 64) + "%"
}

func fmtNum(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64) + "M"
	case n >= 10_000:
		return strconv.FormatFloat(float64(n)/1_000, 'f', 1, 64) + "k"
	}
	return strconv.FormatInt(n, 10)
}

func fmtMs(ms int64) string {
	switch {
	case ms >= 60_000:
		return strconv.FormatFloat(float64(ms)/60_000, 'f', 1, 64) + "m"
	case ms >= 1_000:
		return strconv.FormatFloat(float64(ms)/1_000, 'f', 2, 64) + "s"
	}
	return strconv.FormatInt(ms, 10) + "ms"
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func clock(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("15:04:05")
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func errRate(count, errs int64) string {
	if count == 0 {
		return "—"
	}
	return strconv.FormatFloat(float64(errs)*100/float64(count), 'f', 1, 64) + "%"
}

// transportLabel is the short verb column: GET / QUERY / WS.
func transportLabel(e registry.Endpoint) string {
	switch e.Transport {
	case registry.WebSocket:
		return "WS"
	case registry.GraphQL:
		if e.Method != "" {
			return strings.ToUpper(e.Method)
		}
		return "GQL"
	}
	if e.Method == "" {
		return "REST"
	}
	return strings.ToUpper(e.Method)
}

// verbClass colours the verb chip by transport, as the canvas does.
func verbClass(e registry.Endpoint) string {
	switch {
	case e.Transport == registry.WebSocket:
		return "text-ws border-ws/40"
	case e.Transport == registry.GraphQL && e.Method == "mutation":
		return "text-mutation border-mutation/40"
	case e.Transport == registry.GraphQL:
		return "text-query border-query/40"
	}
	return "text-rest border-rest/40"
}

func endpointPath(e registry.Endpoint) string {
	if e.Transport == registry.GraphQL {
		return e.Path + " · " + e.Name
	}
	if e.Transport == registry.WebSocket {
		return e.Path + " · " + e.Name
	}
	return e.Path
}

// gates lists the endpoint's auth-ish middleware, for the Auth column.
func gates(e registry.Endpoint) []string {
	var out []string
	for _, m := range e.Middleware {
		if strings.HasPrefix(m, "auth:") || strings.HasPrefix(m, "session:") {
			out = append(out, m)
		}
	}
	return out
}

// searchText is what the client-side filter matches a row against.
func searchText(parts ...string) string {
	return strings.ToLower(strings.Join(parts, " "))
}

// typeName renders a schema type compactly: [User!], string?, map[string]int.
func typeName(t *registry.TypeRef) string {
	if t == nil {
		return ""
	}
	var s string
	switch t.Kind {
	case "array":
		s = "[" + typeName(t.Of) + "]"
	case "map":
		s = "map[" + typeName(t.KeyOf) + "]" + typeName(t.Of)
	case "object":
		s = "object"
		if t.Ref != "" {
			s = t.Ref
		}
	case "ref":
		s = t.Ref
	default:
		s = t.Primitive
		if s == "" {
			s = t.Ref
		}
		if s == "" {
			s = t.Kind
		}
	}
	if t.Optional {
		s += "?"
	}
	return s
}

// object resolves a schema to the struct it describes, through lists,
// optionals and refs into the named-type pool. Nil when it isn't one.
func (st *consoleState) object(t *registry.TypeRef) *registry.NamedType {
	for i := 0; t != nil && i < 8; i++ {
		switch {
		case t.Object != nil:
			return t.Object
		case t.Kind == "ref":
			if nt, ok := st.Refs[t.Ref]; ok {
				return &nt
			}
			return nil
		}
		t = t.Of
	}
	return nil
}

// argFields is the endpoint's input fields, from its args schema.
func (st *consoleState) argFields(e registry.Endpoint) []registry.FieldSchema {
	if o := st.object(e.ArgsSchema); o != nil {
		return o.Fields
	}
	return nil
}

var pathParam = regexp.MustCompile(`[:*]([A-Za-z0-9_]+)`)

func pathParams(path string) []string {
	var out []string
	for _, m := range pathParam.FindAllStringSubmatch(path, -1) {
		out = append(out, m[1])
	}
	return out
}

// graphqlTemplate prefills the tester with an operation for the field:
// variables from its args, a selection from its return type's scalars.
func (st *consoleState) graphqlTemplate(e registry.Endpoint) string {
	kind := e.Method
	if kind == "" {
		kind = "query"
	}
	var decl, pass []string
	for _, a := range e.Args {
		decl = append(decl, "$"+a.Name+": "+a.Type)
		pass = append(pass, a.Name+": $"+a.Name)
	}
	var b strings.Builder
	b.WriteString(kind + " " + strings.ToUpper(e.Name[:1]) + e.Name[1:])
	if len(decl) > 0 {
		b.WriteString("(" + strings.Join(decl, ", ") + ")")
	}
	b.WriteString(" {\n  " + e.Name)
	if len(pass) > 0 {
		b.WriteString("(" + strings.Join(pass, ", ") + ")")
	}
	if sel := st.selection(e.ReturnSchema); sel != "" {
		b.WriteString(" " + sel)
	} else if !scalarReturn(e.ReturnType) {
		b.WriteString(" { __typename }")
	}
	b.WriteString("\n}\n")
	return b.String()
}

func (st *consoleState) selection(t *registry.TypeRef) string {
	o := st.object(t)
	if o == nil {
		return ""
	}
	var names []string
	for _, f := range o.Fields {
		// Objects need their own selection; only scalars are listed.
		switch leaf(&f.Type).Kind {
		case "ref", "object", "map", "any":
			continue
		}
		name := f.GraphQLName
		if name == "" {
			name = f.JSONName
		}
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return ""
	}
	return "{ " + strings.Join(names, " ") + " }"
}

// leaf unwraps lists down to the element type.
func leaf(t *registry.TypeRef) *registry.TypeRef {
	for t.Of != nil && t.Kind == "array" {
		t = t.Of
	}
	return t
}

func scalarReturn(t string) bool {
	t = strings.Trim(t, "[]!")
	switch t {
	case "", "String", "Int", "Float", "Boolean", "ID", "MaskedID", "Time", "JSON", "Upload":
		return true
	}
	return false
}

// graphqlVariables prefills the tester's variables with each arg's default.
func graphqlVariables(e registry.Endpoint) string {
	if len(e.Args) == 0 {
		return "{}"
	}
	vars := map[string]any{}
	for _, a := range e.Args {
		vars[a.Name] = a.Default
	}
	b, _ := json.MarshalIndent(vars, "", "  ")
	return string(b)
}

// restBody prefills a REST tester body from the args schema's JSON fields.
func (st *consoleState) restBody(e registry.Endpoint) string {
	switch strings.ToUpper(e.Method) {
	case "POST", "PUT", "PATCH", "DELETE":
	default:
		return ""
	}
	fields := st.argFields(e)
	if len(fields) == 0 {
		return ""
	}
	body := map[string]any{}
	for _, f := range fields {
		if f.Path != "" || f.Query != "" || f.JSONName == "" || f.JSONName == "-" {
			continue
		}
		body[f.JSONName] = zeroOf(&f.Type)
	}
	if len(body) == 0 {
		return ""
	}
	b, _ := json.MarshalIndent(body, "", "  ")
	return string(b)
}

func zeroOf(t *registry.TypeRef) any {
	switch t.Kind {
	case "array":
		return []any{}
	case "map", "object":
		return map[string]any{}
	}
	switch t.Primitive {
	case "string":
		return ""
	case "bool", "boolean":
		return false
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "float32", "float64", "number", "integer":
		return 0
	}
	return nil
}

func detailsJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// sortedKeys orders a details map for display.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func detailValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "—"
	case bool, int, int64, float64, uint64:
		return fmt.Sprint(x)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// endpointsOf lists a service's endpoints.
func (st *consoleState) endpointsOf(service string) []registry.Endpoint {
	var out []registry.Endpoint
	for _, e := range st.Endpoints {
		if e.Service == service {
			out = append(out, e)
		}
	}
	return out
}

func (st *consoleState) serviceTraffic(service string) (reqs, errs int64) {
	for _, e := range st.endpointsOf(service) {
		s := st.stat(e)
		reqs += s.Count
		errs += s.Errors
	}
	return
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func itoa(n int) string     { return strconv.Itoa(n) }
func i64(n int64) string    { return strconv.FormatInt(n, 10) }
func u64(n uint64) string   { return strconv.FormatUint(n, 10) }
func boolStr(b bool) string { return strconv.FormatBool(b) }

func errClass(n int64) string {
	if n > 0 {
		return "text-err"
	}
	return ""
}

func fieldName(f registry.FieldSchema) string {
	switch {
	case f.Path != "":
		return f.Path
	case f.Query != "":
		return f.Query
	case f.JSONName != "" && f.JSONName != "-":
		return f.JSONName
	case f.GraphQLName != "":
		return f.GraphQLName
	}
	return f.Name
}

func fieldSource(f registry.FieldSchema) string {
	switch {
	case f.Path != "":
		return "path"
	case f.Query != "":
		return "query"
	}
	return "body"
}

func limitText(rpm, burst int, perIP bool) string {
	if rpm == 0 {
		return "unlimited"
	}
	s := strconv.Itoa(rpm) + "/min"
	if burst > 0 {
		s += " · burst " + strconv.Itoa(burst)
	}
	if perIP {
		s += " · per IP"
	}
	return s
}

func queryEscape(s string) string { return url.QueryEscape(s) }

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func pathEscape(s string) string { return url.PathEscape(s) }

// endpointTitle names an endpoint in lists: a REST route by its path (its
// registered name already repeats the verb), anything else by its op name.
func endpointTitle(e registry.Endpoint) string {
	if e.Transport == registry.REST && e.Path != "" {
		return e.Path
	}
	return e.Name
}

// endpointSubtitle is the line under the title: the description, else where
// a non-REST op is mounted.
func endpointSubtitle(e registry.Endpoint) string {
	if e.Description != "" {
		return e.Description
	}
	if e.Transport == registry.REST {
		return ""
	}
	return endpointPath(e)
}

// spanKind labels a span by what it is: "request" or "span".
func spanKind(k string) string {
	k = strings.TrimSuffix(strings.TrimSuffix(k, ".start"), ".end")
	if k == "" {
		return "span"
	}
	return k
}

// emptyMessage is a list's empty-state line: "no matches" while searching.
func emptyMessage(q listQuery, none string) string {
	if q.Q != "" || len(q.Args) > 0 {
		return "Nothing matches this filter."
	}
	return none
}

func healthText(ok bool) string {
	if ok {
		return "healthy"
	}
	return "down"
}

// detailsSummary is a resource's details on one line: "driver=postgres host=…".
func detailsSummary(m map[string]any) string {
	var parts []string
	for _, k := range sortedKeys(m) {
		v := detailValue(m[k])
		if v == "" || v == "—" {
			continue
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "  ")
}

// moreTitle is the tooltip on a "+N" chip: the first few hidden names.
func moreTitle(rest []string) string {
	const show = 12
	if len(rest) <= show {
		return strings.Join(rest, ", ")
	}
	return strings.Join(rest[:show], ", ") + " and " + strconv.Itoa(len(rest)-show) + " more"
}

// rejectRow is one 401/403 from extension/auth (an auth.reject event).
type rejectRow struct {
	At       time.Time
	Status   int
	Service  string
	Endpoint string
	Reason   string
	Identity string
	Error    string
	TraceID  string
}

// rejectKind is the event extension/auth publishes for every rejection.
const rejectKind trace.Kind = "auth.reject"

const maxRejectRows = 200

// recentRejects lists the auth rejections still in the trace buffer, newest
// first.
func recentRejects(bus *trace.Bus) []rejectRow {
	if bus == nil {
		return nil
	}
	events := bus.Recent()
	var out []rejectRow
	for i := len(events) - 1; i >= 0 && len(out) < maxRejectRows; i-- {
		e := events[i]
		if e.Kind != rejectKind {
			continue
		}
		r := rejectRow{At: e.Timestamp, Status: e.Status, Service: e.Service, Endpoint: e.Endpoint, Error: e.Error, TraceID: e.TraceID}
		if s, ok := e.Meta["reason"].(string); ok {
			r.Reason = s
		}
		if s, ok := e.Meta["identity"].(string); ok {
			r.Identity = s
		}
		out = append(out, r)
	}
	return out
}

// rejectAttrs links a rejection to its trace when it has one.
func rejectAttrs(r rejectRow) templ.Attributes {
	if r.TraceID == "" {
		return nil
	}
	return templ.Attributes{"data-href": Prefix + "/ui/traces/" + r.TraceID}
}

func rejectClass(r rejectRow) string {
	if r.TraceID == "" {
		return ""
	}
	return "cursor-pointer"
}

func statusText(n int) string {
	if n == 0 {
		return "denied"
	}
	return strconv.Itoa(n)
}
