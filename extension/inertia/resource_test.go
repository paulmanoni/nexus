package inertia_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/inertia"
	"github.com/paulmanoni/nexus/registry"
)

type article struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

type articleInput struct {
	Title string `json:"title"`
}

type articleProps struct {
	Article article `json:"article"`
}

type ArticlesController struct{ destroyed, published []int64 }

func (c *ArticlesController) Index(ctx context.Context) (map[string]any, error) {
	return map[string]any{"count": 2}, nil
}
func (c *ArticlesController) New(ctx context.Context) (map[string]any, error) {
	return map[string]any{"blank": true}, nil
}
func (c *ArticlesController) Show(ctx context.Context, id int64) (articleProps, error) {
	return articleProps{Article: article{ID: id, Title: "shown"}}, nil
}
func (c *ArticlesController) Edit(ctx context.Context, id int64) (articleProps, error) {
	return articleProps{Article: article{ID: id, Title: "editing"}}, nil
}
func (c *ArticlesController) Create(ctx context.Context, in articleInput) (*article, error) {
	if in.Title == "" {
		return nil, nexus.NewErrors().Field("title", "is required")
	}
	return &article{ID: 7, Title: in.Title}, nil
}
func (c *ArticlesController) Update(ctx context.Context, id int64, in articleInput) (*article, error) {
	return &article{ID: id, Title: in.Title}, nil
}
func (c *ArticlesController) Destroy(ctx context.Context, id int64) error {
	c.destroyed = append(c.destroyed, id)
	return nil
}

// Custom actions: a member write, a collection page, a JSON opt-out.
func (c *ArticlesController) Publish(ctx context.Context, id int64) error {
	c.published = append(c.published, id)
	return nil
}
func (c *ArticlesController) Stats(ctx context.Context) (map[string]any, error) {
	return map[string]any{"total": 9}, nil
}
func (c *ArticlesController) Suggest(ctx context.Context, q struct {
	Term string `query:"q"`
}) ([]string, error) {
	return []string{q.Term + "!"}, nil
}

// NotesController has no Show and no Index: redirects fall back.
type NotesController struct{}

func (c *NotesController) Create(ctx context.Context, in articleInput) (*article, error) {
	return &article{ID: 3}, nil
}
func (c *NotesController) Update(ctx context.Context, id int64, in articleInput) (*article, error) {
	return &article{ID: id}, nil
}
func (c *NotesController) Destroy(ctx context.Context, id int64) error { return nil }

func resourceReq(t *testing.T, method, addr, path, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, _ := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestInertiaResource(t *testing.T) {
	addr := "127.0.0.1:8871"
	ctl := &ArticlesController{}
	var app *nexus.App
	bootInertia(t, addr,
		inertia.Resource[*ArticlesController]("/articles").Supply(ctl).
			Member("POST", "publish", (*ArticlesController).Publish).
			Collection("GET", "stats", (*ArticlesController).Stats).
			Collection("GET", "suggest", (*ArticlesController).Suggest, nexus.NoActionDefaults()),
		nexus.Invoke(func(a *nexus.App) { app = a }),
		inertia.ResourceAs[*NotesController]("Admin/Notes", "/notes").Supply(&NotesController{}),
	)
	xhr := map[string]string{"X-Inertia": "true"}

	pages := []struct{ path, component, prop string }{
		{"/articles", "Articles/Index", `"count":2`},
		{"/articles/new", "Articles/New", `"blank":true`},
		{"/articles/3", "Articles/Show", `"id":3`},
		{"/articles/3/edit", "Articles/Edit", `"title":"editing"`},
		{"/articles/stats", "Articles/Stats", `"total":9`},
	}
	for _, p := range pages {
		res, body := resourceReq(t, "GET", addr, p.path, "", xhr)
		var page struct {
			Component string `json:"component"`
		}
		_ = json.Unmarshal([]byte(body), &page)
		if res.StatusCode != 200 || page.Component != p.component || !strings.Contains(body, p.prop) {
			t.Errorf("GET %s = %d %s, want component %s with %s", p.path, res.StatusCode, body, p.component, p.prop)
		}
	}

	redirects := []struct{ method, path, body, want string }{
		{"POST", "/articles", `{"title":"new"}`, "/articles/7"},
		{"PUT", "/articles/3", `{"title":"x"}`, "/articles/3"},
		{"PATCH", "/articles/3", `{"title":"x"}`, "/articles/3"},
		{"DELETE", "/articles/4", "", "/articles"},
		// A custom write goes back; without a Referer, one segment up.
		{"POST", "/articles/5/publish", "", "/articles/5"},
		// No Show: Create stays on the collection, Update goes up to it.
		{"POST", "/notes", `{"title":"n"}`, "/notes"},
		{"PUT", "/notes/9", `{"title":"n"}`, "/notes"},
	}
	for _, r := range redirects {
		res, body := resourceReq(t, r.method, addr, r.path, r.body, xhr)
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != r.want {
			t.Errorf("%s %s = %d → %q (%s), want 303 → %s", r.method, r.path, res.StatusCode, res.Header.Get("Location"), body, r.want)
		}
	}
	if fmt.Sprint(ctl.destroyed) != "[4]" || fmt.Sprint(ctl.published) != "[5]" {
		t.Errorf("Destroy saw %v, Publish %v, want [4] and [5]", ctl.destroyed, ctl.published)
	}
	res, _ := resourceReq(t, "POST", addr, "/articles/6/publish", "", map[string]string{"X-Inertia": "true", "Referer": "/articles?page=2"})
	if res.Header.Get("Location") != "/articles?page=2" {
		t.Errorf("custom write with a Referer → %q, want it", res.Header.Get("Location"))
	}
	if res, body := resourceReq(t, "GET", addr, "/articles/suggest?q=go", "", nil); res.StatusCode != 200 || strings.TrimSpace(body) != `["go!"]` {
		t.Errorf("NoActionDefaults action = %d %s, want plain JSON", res.StatusCode, body)
	}

	// Custom actions reach the SDK: pages for pageUrl, writes for pageAction.
	tags := map[string]string{}
	for _, e := range app.Registry().Endpoints() {
		tags[e.Method+" "+e.Path] = e.Tags[registry.PageTag] + "|" + e.Tags[registry.PageActionTag]
	}
	for route, want := range map[string]string{
		"GET /articles/stats":        "Articles/Stats|",
		"POST /articles/:id/publish": "|Articles/Publish",
		"GET /articles/suggest":      "|",
		"POST /articles":             "|Articles/Create",
		"GET /articles/:id/edit":     "Articles/Edit|",
	} {
		if tags[route] != want {
			t.Errorf("%s tags = %q, want %q", route, tags[route], want)
		}
	}
	// No Index either: Destroy goes back.
	res, _ = resourceReq(t, "DELETE", addr, "/notes/9", "", map[string]string{"X-Inertia": "true", "Referer": "/notes/list"})
	if res.Header.Get("Location") != "/notes/list" {
		t.Errorf("Destroy without Index → %q, want the Referer", res.Header.Get("Location"))
	}

	// A validation error sends the user back to the form with the errors.
	res, _ = resourceReq(t, "POST", addr, "/articles", `{"title":""}`,
		map[string]string{"X-Inertia": "true", "Referer": "/articles/new"})
	flashed := false
	for _, ck := range res.Cookies() {
		flashed = flashed || ck.Name == "nexus_inertia_errors"
	}
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/articles/new" || !flashed {
		t.Errorf("invalid create = %d → %q (flash %v), want 303 back with errors", res.StatusCode, res.Header.Get("Location"), flashed)
	}
}

type ListsController struct{}

func (c *ListsController) Longlist(ctx context.Context, pk int64) (articleProps, error) {
	return articleProps{Article: article{ID: pk, Title: "longlist"}}, nil
}

type ReportsController struct{}

func (c *ReportsController) Index(ctx context.Context) (map[string]any, error) {
	return map[string]any{"reports": 3}, nil
}

// inertia.Component renders any controller action as a page, and overrides a
// resource's conventional component.
func TestInertiaComponent(t *testing.T) {
	addr := "127.0.0.1:8872"
	bootInertia(t, addr,
		nexus.Module("admin", nexus.Path("/admin"),
			nexus.Controller[*ListsController]("").Supply(&ListsController{}).
				Get("/longlist/:pk/view", (*ListsController).Longlist, inertia.Component("Admin/AdvertLonglist")),
			inertia.Resource[*ReportsController]("/reports").Supply(&ReportsController{}).
				ActionDefaults(func(method, path, action string) []nexus.RestOption {
					if action == "Index" {
						return []nexus.RestOption{inertia.Component("Admin/Reports")}
					}
					return nil
				}),
		),
	)
	xhr := map[string]string{"X-Inertia": "true"}
	for path, want := range map[string]string{
		"/admin/longlist/4/view": "Admin/AdvertLonglist",
		"/admin/reports":         "Admin/Reports",
	} {
		res, body := resourceReq(t, "GET", addr, path, "", xhr)
		var page struct {
			Component string `json:"component"`
		}
		_ = json.Unmarshal([]byte(body), &page)
		if res.StatusCode != 200 || page.Component != want {
			t.Errorf("GET %s = %d %s, want component %s", path, res.StatusCode, body, want)
		}
	}
}
