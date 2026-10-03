package view

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/registry"
)

type chartProps struct {
	Points []int `json:"points"`
}

var chart = NewIsland[chartProps]("Chart")

func chartPage() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		ctx = templ.WithChildren(ctx, templ.Raw(`<p>loading</p>`))
		return chart(chartProps{Points: []int{1, 2}}, Visible()).Render(ctx, w)
	})
}

func getPage(t *testing.T, cfg config.Runtime, opts ...nexus.Option) (int, string) {
	t.Helper()
	app, stop, err := nexus.InProcess(cfg, append(opts, Page("GET", "/", chartPage))...)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// A production page loads the islands from the build, under the frontend's
// mount.
func TestIslandFromBuild(t *testing.T) {
	dist := fstest.MapFS{
		"dist/.vite/manifest.json": {Data: []byte(`{
  "nexus-islands": {"file":"assets/nexus-islands-B2xQ9fLk.js","name":"nexus-islands","isEntry":true},
  "virtual:nexus-island/Chart": {"file":"assets/Chart-B2xQ9fLk.js","name":"Chart","isDynamicEntry":true}
}`)},
	}
	status, page := getPage(t, config.Runtime{Environment: "production"},
		nexus.ServeFrontend(dist, "dist", nexus.FrontendAt("/static")))
	want := `<nx-island data-c="Chart" data-l="/static/assets/nexus-islands-B2xQ9fLk.js" ` +
		`data-p="{&#34;points&#34;:[1,2]}" data-when="visible"><p>loading</p></nx-island>`
	if status != 200 || page != want {
		t.Fatalf("status %d\n got %s\nwant %s", status, page, want)
	}
}

// While the dev server serves the project's islands, pages load them from it.
func TestIslandFromDevServer(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	hot := `{"version":1,"origin":"http://127.0.0.1:5173","base":"/","entries":["index.html"],` +
		`"pid":` + strconv.Itoa(os.Getpid()) + `,"islands":true}`
	if err := os.MkdirAll(filepath.Join(dir, "dist", ".vite"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dist", ".vite", "nexus-hot.json"), []byte(hot), 0o644); err != nil {
		t.Fatal(err)
	}
	status, page := getPage(t, config.Runtime{Environment: "development"},
		nexus.ServeFrontend(os.DirFS(dir), "dist"))
	if status != 200 || !strings.Contains(page, `data-l="http://127.0.0.1:5173/@id/virtual:nexus-islands"`) {
		t.Fatalf("status %d: %s", status, page)
	}
}

// Without islands to load, the page still renders: the fallback stays and
// the element says why.
func TestIslandWithoutAFrontend(t *testing.T) {
	status, page := getPage(t, config.Runtime{Environment: "production"})
	if status != 200 || !strings.Contains(page, `data-error="no frontend`) || !strings.Contains(page, "<p>loading</p>") ||
		strings.Contains(page, "data-l=") {
		t.Fatalf("no frontend: status %d: %s", status, page)
	}
	dist := fstest.MapFS{"dist/.vite/manifest.json": {Data: []byte(`{"src/main.ts":{"file":"assets/main.js","isEntry":true}}`)}}
	status, page = getPage(t, config.Runtime{Environment: "production"}, nexus.ServeFrontend(dist, "dist"))
	if status != 200 || !strings.Contains(page, "has no islands loader") {
		t.Fatalf("a build without islands: status %d: %s", status, page)
	}
}

func TestIslandPropsAreAnObject(t *testing.T) {
	var buf bytes.Buffer
	err := NewIsland[[]int]("List")([]int{1}).Render(context.Background(), &buf)
	if err == nil || !strings.Contains(err.Error(), "JSON object") {
		t.Fatalf("err = %v", err)
	}
}

// A build without the island an app renders says so.
func TestIslandMissingFromBuild(t *testing.T) {
	dist := fstest.MapFS{"dist/.vite/manifest.json": {Data: []byte(`{
  "nexus-islands": {"file":"assets/nexus-islands-B2xQ9fLk.js","name":"nexus-islands","isEntry":true}
}`)}}
	status, page := getPage(t, config.Runtime{Environment: "production"}, nexus.ServeFrontend(dist, "dist"))
	if status != 200 || !strings.Contains(page, "has no island Chart") {
		t.Fatalf("status %d: %s", status, page)
	}
}

func TestNewIslandNames(t *testing.T) {
	for _, bad := range []string{"", "Chart.vue", "../x", "a//b", "/a"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewIsland(%q) must panic", bad)
				}
			}()
			NewIsland[chartProps](bad)
		}()
	}
	NewIsland[chartProps]("admin/Chart-2") // a path is fine
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "declared twice") {
			t.Fatalf("recover = %v", r)
		}
	}()
	NewIsland[string]("Chart")
}

type searchProps struct {
	Query  *Signal[string] `json:"query"`
	Limit  int             `json:"limit"`
	Points []int           `json:"points"`
}

// A signal in the props travels as a reference the browser keeps live; the
// SDK types it as its value, and the islands server gets the value.
func TestIslandSignalProps(t *testing.T) {
	sig := &Signal[string]{id: "g1", v: "cat"}
	var buf bytes.Buffer
	if err := NewIsland[searchProps]("Search")(searchProps{Query: sig, Limit: 3}).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `{&#34;query&#34;:{&#34;$sig&#34;:&#34;g1&#34;,&#34;v&#34;:&#34;cat&#34;},&#34;limit&#34;:3`) {
		t.Fatalf("props: %s", buf.String())
	}
	var v any
	_ = json.Unmarshal([]byte(`{"query":{"$sig":"g1","v":"cat"},"rows":[{"$sig":"g2","v":1}]}`), &v)
	got, _ := json.Marshal(signalValues(v))
	if string(got) != `{"query":"cat","rows":[1]}` {
		t.Fatalf("signalValues = %s", got)
	}
	ref := registry.WalkType(reflect.TypeFor[searchProps](), map[string]registry.NamedType{})
	refs := map[string]registry.NamedType{}
	registry.WalkType(reflect.TypeFor[searchProps](), refs)
	q := refs["searchProps"].Fields[0]
	if ref.Ref != "searchProps" || q.Type.Kind != "primitive" || q.Type.Primitive != "string" || q.Type.Optional {
		t.Fatalf("query field types as %+v", q.Type)
	}
}

var ssrChart = NewIsland[chartProps]("SSRChart")

// An SSR island is rendered by the islands server in production, and
// renders in the browser only when that server does not answer.
func TestIslandSSR(t *testing.T) {
	var got []string
	ssr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"html":"<svg id=\"ssr\"></svg>"}`)
	}))
	defer ssr.Close()
	dist := fstest.MapFS{"dist/.vite/manifest.json": {Data: []byte(`{
  "nexus-islands": {"file":"assets/nexus-islands-B2xQ9fLk.js","name":"nexus-islands","isEntry":true},
  "virtual:nexus-island/SSRChart": {"file":"assets/SSRChart-B2xQ9fLk.js","name":"SSRChart","isDynamicEntry":true}
}`)}}
	page := func(opts ...nexus.Option) string {
		app, stop, err := nexus.InProcess(config.Runtime{Environment: "production"}, append(opts,
			nexus.ServeFrontend(dist, "dist"),
			Page("GET", "/", func() templ.Component {
				return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
					ctx = templ.WithChildren(ctx, templ.Raw(`<p>loading</p>`))
					return ssrChart(chartProps{Points: []int{3}}, SSR()).Render(ctx, w)
				})
			}))...)
		if err != nil {
			t.Fatal(err)
		}
		defer stop(context.Background())
		srv := httptest.NewServer(app)
		defer srv.Close()
		res, err := http.Get(srv.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}

	out := page(IslandServer(ssr.URL))
	if !strings.Contains(out, ` data-ssr><svg id="ssr"></svg></nx-island>`) || strings.Contains(out, "loading") {
		t.Fatalf("rendered on the server: %s", out)
	}
	if len(got) != 1 || got[0] != `POST /render {"name":"SSRChart","props":{"points":[3]}}` {
		t.Fatalf("islands server got %q", got)
	}

	ssr.Close()
	out = page(IslandServer(ssr.URL))
	if strings.Contains(out, "data-ssr") || !strings.Contains(out, "<p>loading</p>") {
		t.Fatalf("server down: %s", out)
	}
}
