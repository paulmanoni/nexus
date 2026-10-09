package nexus

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/dev"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/middleware"
	"github.com/paulmanoni/nexus/v2/trace"
)

func toolbarApp(t *testing.T, nexusDev string) *App {
	t.Helper()
	t.Setenv(dev.Env, nexusDev)
	sql := func(ctx context.Context, q string) {
		_, sp := trace.StartSpan(ctx, "sql", trace.Str("sql", q))
		sp.Set("ms", "0.50")
		sp.End(nil)
	}
	page := func(c *httpx.Ctx, l *slog.Logger) {
		ctx := c.Request.Context()
		sql(ctx, "SELECT * FROM pets WHERE owner_id = ?")
		sql(ctx, "SELECT * FROM pets WHERE owner_id = ?")
		sql(ctx, "SELECT * FROM owners")
		l.WarnContext(ctx, "slow owner lookup", "owner", 7)
		dev.Note(ctx, "Cache", "pets:7 miss")
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<!doctype html><html><body><p>pets</p></body></html>"))
	}
	app, stop, err := InProcess(config.Runtime{TraceCapacity: 1000},
		AsRest("GET", "/pets", page),
		AsRest("GET", "/api/pets", func(c *httpx.Ctx) { c.JSON(http.StatusOK, httpx.H{"pets": 1}) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	return app
}

func toolbarGet(app *App, path, dest string, accept ...string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", path, nil)
	if dest != "" {
		r.Header.Set("Sec-Fetch-Dest", dest)
	}
	if len(accept) > 0 {
		r.Header.Set("Accept", accept[0])
	}
	app.ServeHTTP(w, r)
	return w
}

func TestDevToolbar(t *testing.T) {
	dev.AddPanel(dev.Panel{Name: "Cache", Render: func(r *dev.Request) dev.Section {
		return dev.Section{Summary: fmt.Sprintf("%d lookups", len(r.Notes("Cache")))}
	}})
	app := toolbarApp(t, "1")

	w := toolbarGet(app, "/pets", "document")
	id := w.Header().Get(dev.HeaderName)
	if id == "" || !strings.Contains(w.Body.String(), `data-nexus-toolbar="`+id+`"`) || !strings.Contains(w.Body.String(), "toolbar.js\" data-nexus-toolbar=\""+id+"\" defer></script></body>") {
		t.Fatalf("page: id %q, body %s", id, w.Body)
	}

	// The drawer's view, rendered by templ.
	h := toolbarGet(app, "/__nexus/toolbar/requests/"+id, "").Body.String()
	for _, want := range []string{`data-handle="`, `3q"`, `data-tone="warn"`, `<section data-panel="SQL" hidden>`, `class="tl-row"`, "slow owner lookup", `class="bar sql"`} {
		if !strings.Contains(h, want) {
			t.Errorf("view lacks %q:\n%s", want, h)
		}
	}

	d := toolbarGet(app, "/__nexus/toolbar/requests/"+id, "", "application/json")
	var got struct {
		Status  int `json:"status"`
		Queries int `json:"queries"`
		Panels  []struct {
			Name    string `json:"name"`
			Summary string `json:"summary"`
			Tone    string `json:"tone"`
			Table   *struct {
				Rows [][]any `json:"rows"`
			} `json:"table"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(d.Body.Bytes(), &got); err != nil {
		t.Fatalf("detail %d: %s", d.Code, d.Body)
	}
	if got.Status != 200 || got.Queries != 3 {
		t.Fatalf("detail = %s", d.Body)
	}
	byName := map[string]int{}
	for i, p := range got.Panels {
		byName[p.Name] = i
	}
	sqlP := got.Panels[byName["SQL"]]
	if !strings.HasPrefix(sqlP.Summary, "3 queries") || sqlP.Tone != "warn" || len(sqlP.Table.Rows) != 3 {
		t.Errorf("SQL panel = %+v", sqlP)
	}
	logs := got.Panels[byName["Logs"]]
	if logs.Summary != "1 records" || logs.Tone != "warn" || logs.Table.Rows[0][2] != "slow owner lookup" {
		t.Errorf("Logs panel = %+v", logs)
	}
	if c := got.Panels[byName["Cache"]]; c.Summary != "1 lookups" {
		t.Errorf("custom panel = %+v", c)
	}

	// A fetch is recorded, not injected; neither is a non-HTML answer.
	if w := toolbarGet(app, "/pets", "empty"); strings.Contains(w.Body.String(), "toolbar.js") || w.Header().Get(dev.HeaderName) == "" {
		t.Errorf("fetched page: %s", w.Body)
	}
	if w := toolbarGet(app, "/api/pets", "document"); strings.Contains(w.Body.String(), "toolbar.js") || strings.TrimSpace(w.Body.String()) != `{"pets":1}` {
		t.Errorf("JSON: %s", w.Body)
	}
	if w := toolbarGet(app, "/__nexus/toolbar/toolbar.js", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "__nxToolbar") {
		t.Errorf("script: %d", w.Code)
	}
}

func TestDevToolbarOffOutsideDev(t *testing.T) {
	app := toolbarApp(t, "")
	w := toolbarGet(app, "/pets", "document")
	if w.Header().Get(dev.HeaderName) != "" || strings.Contains(w.Body.String(), "toolbar.js") {
		t.Fatalf("toolbar outside nexus dev: %s", w.Body)
	}
	if w := toolbarGet(app, "/__nexus/toolbar/toolbar.js", ""); w.Code != http.StatusNotFound {
		t.Fatalf("script served outside nexus dev: %d", w.Code)
	}
}

// gzipPages compresses HTML for clients that accept it, as an app's own
// edge middleware might.
func gzipPages() middleware.Middleware {
	return middleware.Middleware{Name: "gzip", Stage: middleware.Edge, HTTP: func(c *httpx.Ctx) {
		if !strings.Contains(c.Request.Header.Get("Accept-Encoding"), "gzip") {
			c.Next()
			return
		}
		orig := c.Writer
		gw := &gzipWriter{ResponseWriter: orig}
		c.Writer = &httpx.ResponseWriter{ResponseWriter: gw}
		defer func() {
			if gw.gz != nil {
				gw.gz.Close()
			}
			c.Writer = orig
		}()
		c.Next()
	}}
}

type gzipWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (w *gzipWriter) WriteHeader(code int) {
	if strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		w.Header().Set("Content-Encoding", "gzip")
		w.gz = gzip.NewWriter(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipWriter) Write(b []byte) (int, error) {
	if w.gz != nil {
		return w.gz.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func TestDevToolbarUnderAnAppsCompression(t *testing.T) {
	t.Setenv(dev.Env, "1")
	page := func(c *httpx.Ctx) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<!doctype html><html><body><p>pets</p></body></html>"))
	}
	app, stop, err := InProcess(config.Runtime{TraceCapacity: 100}, Middleware(gzipPages()), AsRest("GET", "/pets", page))
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/pets", nil)
	r.Header.Set("Sec-Fetch-Dest", "document")
	r.Header.Set("Accept-Encoding", "gzip, deflate, br")
	app.ServeHTTP(w, r)
	if w.Header().Get("Content-Encoding") != "" || !strings.Contains(w.Body.String(), "toolbar.js") {
		t.Fatalf("page under compression: encoding %q, body %q", w.Header().Get("Content-Encoding"), w.Body.String())
	}
	id := w.Header().Get(dev.HeaderName)
	d := toolbarGet(app, "/__nexus/toolbar/requests/"+id, "")
	if !strings.Contains(d.Body.String(), "gzip, deflate, br") {
		t.Errorf("the Request panel shows the browser's Accept-Encoding: %s", d.Body)
	}

	// A fetch keeps its compression.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/pets", nil)
	r.Header.Set("Sec-Fetch-Dest", "empty")
	r.Header.Set("Accept-Encoding", "gzip")
	app.ServeHTTP(w, r)
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("fetch lost its compression: %q", w.Header().Get("Content-Encoding"))
	}
}

func TestDevToolbarTracksAPagesLaterWork(t *testing.T) {
	app := toolbarApp(t, "1")
	page := toolbarGet(app, "/pets", "document").Header().Get(dev.HeaderName)

	// A live event of the page, as view runs one over its socket.
	ctx, done := dev.Track(trace.WithBus(context.Background(), app.bus), page, "LIVE", "PetsPage.Search")
	ctx, _, finish := trace.NewRootSpan(ctx, "PetsPage.Search", "PetsPage", "PetsPage.Search", "live")
	_, sp := trace.StartSpan(ctx, "sql", trace.Str("sql", "SELECT * FROM pets WHERE name LIKE ?"))
	sp.End(nil)
	finish(200, nil)
	done(200, nil)

	var got struct {
		Next  int `json:"next"`
		Items []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"items"`
	}
	w := toolbarGet(app, "/__nexus/toolbar/requests/"+page+"/children?after=0", "")
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Next != 1 || len(got.Items) != 1 ||
		!strings.HasPrefix(got.Items[0].Label, "LIVE PetsPage.Search · 200") || !strings.HasSuffix(got.Items[0].Label, "· 1q") {
		t.Fatalf("children = %s", w.Body)
	}
	if w := toolbarGet(app, "/__nexus/toolbar/requests/"+page+"/children?after=1", ""); !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Errorf("after the last: %s", w.Body)
	}
	d := toolbarGet(app, "/__nexus/toolbar/requests/"+got.Items[0].ID, "", "application/json")
	if !strings.Contains(d.Body.String(), `"queries":1`) || !strings.Contains(d.Body.String(), "name LIKE ?") {
		t.Errorf("entry = %s", d.Body)
	}

	// Work of a page the toolbar doesn't know runs untracked.
	if ctx2, done2 := dev.Track(context.Background(), "unknown", "LIVE", "X"); dev.RequestFrom(ctx2) != nil {
		t.Error("tracked under an unknown page")
	} else {
		done2(200, nil)
	}
}

func TestDevToolbarBuildState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NEXUS_DEV_STATE", filepath.Join(dir, "state.json"))
	app := toolbarApp(t, "1")
	if err := os.WriteFile(filepath.Join(dir, "build.json"), []byte(`{"state":"failed","since":1,"output":"main.go:3: undefined: x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w := toolbarGet(app, "/__nexus/toolbar/build", "")
	if !strings.Contains(w.Body.String(), `"state":"failed"`) || !strings.Contains(w.Body.String(), "undefined: x") {
		t.Fatalf("build = %s", w.Body)
	}
}
