package nexus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2/config"
)

type ctlUser struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// userInput is unexported on purpose: body mode must not need an exported type.
type userInput struct {
	Name string `json:"name" validate:"required"`
}

type UsersController struct{ calls []string }

func NewUsersController() *UsersController { return &UsersController{} }

func (c *UsersController) Index(ctx context.Context) ([]ctlUser, error) {
	return []ctlUser{{ID: 1, Name: "ada"}}, nil
}

func (c *UsersController) Show(ctx context.Context, id int64) (*ctlUser, error) {
	if id == 404 {
		return nil, NotFound
	}
	return &ctlUser{ID: id, Name: "ada"}, nil
}

func (c *UsersController) Create(ctx context.Context, in userInput) (*ctlUser, error) {
	return &ctlUser{ID: 7, Name: in.Name}, nil
}

func (c *UsersController) Update(ctx context.Context, id int64, in userInput) (*ctlUser, error) {
	return &ctlUser{ID: id, Name: in.Name}, nil
}

func (c *UsersController) Destroy(ctx context.Context, id int64) error {
	c.calls = append(c.calls, fmt.Sprint("destroy-", id))
	return nil
}

func (c *UsersController) Suspend(ctx context.Context, id int64) (*ctlUser, error) {
	return &ctlUser{ID: id, Name: "suspended"}, nil
}

func (c *UsersController) Search(ctx context.Context, q struct {
	Term string `query:"q"`
}) ([]ctlUser, error) {
	return []ctlUser{{ID: 2, Name: q.Term}}, nil
}

func ctlDo(t *testing.T, app *App, method, path, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	app.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func TestResourceConventions(t *testing.T) {
	ctl := NewUsersController()
	app, stop, err := InProcess(config.Runtime{},
		Resource[*UsersController]("/users").
			Supply(ctl).
			Member("POST", "suspend", (*UsersController).Suspend).
			Collection("GET", "search", (*UsersController).Search),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	cases := []struct{ method, path, body, want string }{
		{"GET", "/users", "", `"name":"ada"`},
		{"GET", "/users/42", "", `"id":42`},
		{"POST", "/users", `{"name":"grace"}`, `"name":"grace"`},
		{"PUT", "/users/5", `{"name":"lin"}`, `{"id":5,"name":"lin"}`},
		{"PATCH", "/users/5", `{"name":"lin"}`, `{"id":5,"name":"lin"}`},
		// The route segment is the only source of the id: a body id is ignored.
		{"PUT", "/users/5", `{"id":6,"name":"lin"}`, `{"id":5,"name":"lin"}`},
		{"POST", "/users/9/suspend", "", `"name":"suspended"`},
		{"GET", "/users/search?q=kay", "", `"name":"kay"`},
	}
	for _, tc := range cases {
		code, body := ctlDo(t, app, tc.method, tc.path, tc.body)
		if code >= 300 || !strings.Contains(body, tc.want) {
			t.Errorf("%s %s = %d %s, want %s", tc.method, tc.path, code, body, tc.want)
		}
	}
	if code, _ := ctlDo(t, app, "DELETE", "/users/3", ""); code >= 300 || len(ctl.calls) != 1 || ctl.calls[0] != "destroy-3" {
		t.Errorf("DELETE /users/3 = %d, calls %v", code, ctl.calls)
	}
	if code, _ := ctlDo(t, app, "GET", "/users/404", ""); code != 404 {
		t.Errorf("CRUD sentinel from an action = %d, want 404", code)
	}

	var mod string
	for _, e := range app.Registry().Endpoints() {
		if e.Path == "/users/:id" {
			mod = e.Module
		}
	}
	if mod != "users" {
		t.Errorf("dashboard module = %q, want users", mod)
	}
}

type CommentsController struct{}

func (c *CommentsController) Show(ctx context.Context, postID, id int64) (string, error) {
	return fmt.Sprintf("post-%d-comment-%d", postID, id), nil
}

// A resource nested under a parent path gets the parent's parameters too —
// including one contributed by an enclosing router.
func TestResourceNested(t *testing.T) {
	posts := NewRouter("posts", "/posts/:postId")
	posts.Include(Resource[*CommentsController]("/comments").Supply(&CommentsController{}).Router)
	app, stop, err := InProcess(config.Runtime{}, posts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	if code, body := ctlDo(t, app, "GET", "/posts/3/comments/8", ""); code != 200 || !strings.Contains(body, "post-3-comment-8") {
		t.Fatalf("nested show = %d %s", code, body)
	}
}

type GuardedController struct{ seen []string }

func (c *GuardedController) Authorize(ctx context.Context, action string) error {
	c.seen = append(c.seen, action)
	switch action {
	case "Destroy":
		return errors.New("you cannot delete this")
	case "Show":
		if ctx.Value(ctlKey{}) == "hide" {
			return NotFound
		}
	}
	return nil
}

type ctlKey struct{}

func (c *GuardedController) Show(ctx context.Context, id int64) (string, error) {
	return fmt.Sprint("shown-", id), nil
}

// No context parameter: the Authorize wrapper supplies one.
func (c *GuardedController) Destroy(id int64) error { return nil }

func (c *GuardedController) Stats(ctx context.Context) (*ctlUser, error) {
	return &ctlUser{Name: "stats"}, nil
}

func TestControllerAuthorize(t *testing.T) {
	ctl := &GuardedController{}
	app, stop, err := InProcess(config.Runtime{},
		Resource[*GuardedController]("/things").Supply(ctl).Query((*GuardedController).Stats),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	if code, body := ctlDo(t, app, "GET", "/things/4", ""); code != 200 || !strings.Contains(body, "shown-4") {
		t.Fatalf("allowed action = %d %s", code, body)
	}
	code, body := ctlDo(t, app, "DELETE", "/things/4", "")
	if code != 403 || !strings.Contains(body, "you cannot delete this") {
		t.Fatalf("refused action = %d %s, want 403 with the message", code, body)
	}
	// A controller's prefix is REST-only: its GraphQL stays on the app's endpoint.
	gw := httptest.NewRecorder()
	greq := httptest.NewRequest("POST", "/graphql", strings.NewReader(`{"query":"{ stats { name } }"}`))
	greq.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(gw, greq)
	if got := gw.Body.String(); !strings.Contains(got, `"stats"`) {
		t.Fatalf("GraphQL action keeps its method name = %s", got)
	}
	want := []string{"Show", "Destroy", "Stats"}
	if strings.Join(ctl.seen, ",") != strings.Join(want, ",") {
		t.Fatalf("Authorize saw %v, want %v", ctl.seen, want)
	}
	if !errors.Is(forbiddenError(NotFound), NotFound) || CodeOf(forbiddenError(errors.New("no"))) != Forbidden {
		t.Fatal("a refusal keeps the meaning of the error it wraps")
	}
}

type badController struct{}

func (c *badController) Show(ctx context.Context, a, b int64) (string, error) { return "", nil }
func (c *badController) Ping(ctx context.Context) (string, error)             { return "", nil }

type noActions struct{}

func TestControllerBootErrors(t *testing.T) {
	cases := []struct {
		name string
		opt  Option
		want string
	}{
		{"param count", Controller[*badController]("/b").Supply(&badController{}).Get("/:id", (*badController).Show), "path parameter(s)"},
		{"not a method", Controller[*badController]("/b").Get("/x", (*UsersController).Index), "not a method of"},
		{"no actions", Resource[*noActions]("/n"), "defines none of"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stop, err := InProcess(config.Runtime{}, tc.opt)
			if stop != nil {
				defer func() { _ = stop(context.Background()) }()
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want boot error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestControllerName(t *testing.T) {
	for in, want := range map[string]string{
		controllerName(reflect.TypeFor[*UsersController]()):   "users",
		controllerName(reflect.TypeFor[CommentsController]()): "comments",
		controllerName(reflect.TypeFor[*noActions]()):         "noActions",
	} {
		if in != want {
			t.Errorf("controllerName = %q, want %q", in, want)
		}
	}
}

// Arg body mode on GraphQL: the scalar and the body's fields are all
// arguments of the field.
type argBodySvc struct{}

func (s *argBodySvc) Rename(ctx context.Context, id int64, in userInput) (*ctlUser, error) {
	return &ctlUser{ID: id, Name: in.Name}, nil
}

func TestArgBodyModeGraphQL(t *testing.T) {
	app, stop, err := InProcess(config.Runtime{},
		Supply(&argBodySvc{}, &argSvc{}),
		AsQuery((*argSvc).FetchUser, Arg("id")),
		AsMutation((*argBodySvc).Rename, Arg("id")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	got := postGraphQL(t, app, `mutation { rename(id: 4, name: "zed") { id name } }`)
	var res struct {
		Data struct {
			Rename ctlUser `json:"rename"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(got), &res); err != nil || res.Data.Rename.ID != 4 || res.Data.Rename.Name != "zed" {
		t.Fatalf("rename = %s", got)
	}
}

type ListingsController struct{}

func (c *ListingsController) Index(ctx context.Context) (string, error) { return "listings", nil }
func (c *ListingsController) ListingRows(ctx context.Context) (string, error) {
	return "rows", nil
}

// Inside a module, a controller stacks under the module's Path, and its
// GraphQL serves on the module's endpoint rather than <prefix>/graphql.
func TestControllerInsideModule(t *testing.T) {
	app, stop, err := InProcess(config.Runtime{},
		Module("market", Path("/market"),
			Controller[*ListingsController]("/listings").Supply(&ListingsController{}).
				Get("", (*ListingsController).Index).
				Query((*ListingsController).ListingRows),
			NewRouter("reports", "/reports").Rest("GET", "/daily", func() (string, error) { return "daily", nil }),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	if code, body := ctlDo(t, app, "GET", "/market/listings", ""); code != 200 || !strings.Contains(body, "listings") {
		t.Errorf("GET /market/listings = %d %s", code, body)
	}
	if code, body := ctlDo(t, app, "GET", "/market/reports/daily", ""); code != 200 || !strings.Contains(body, "daily") {
		t.Errorf("plain router in a module: GET /market/reports/daily = %d %s", code, body)
	}
	for path, want := range map[string]bool{"/market/graphql": true, "/market/listings/graphql": false, "/listings/graphql": false} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"query":"{ listingRows }"}`))
		r.Header.Set("Content-Type", "application/json")
		app.ServeHTTP(w, r)
		if got := strings.Contains(w.Body.String(), `"listingRows":"rows"`); got != want {
			t.Errorf("POST %s answered listingRows = %v, want %v (%d %s)", path, got, want, w.Code, w.Body.String())
		}
	}
}

type composeCtl struct{}

func (c *composeCtl) Index(ctx context.Context) (string, error) { return "i", nil }

// ActionDefaults calls add up instead of replacing each other.
func TestActionDefaultsCompose(t *testing.T) {
	app, stop, err := InProcess(config.Runtime{},
		Controller[*composeCtl]("/c").Supply(&composeCtl{}).
			ActionDefaults(func(method, path, action string) []RestOption { return []RestOption{Tag("first", action)} }).
			ActionDefaults(func(method, path, action string) []RestOption { return []RestOption{Tag("second", method)} }).
			Get("", (*composeCtl).Index),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	for _, e := range app.Registry().Endpoints() {
		if e.Path == "/c" && (e.Tags["first"] != "Index" || e.Tags["second"] != "GET") {
			t.Errorf("tags = %v, want both defaults applied", e.Tags)
		}
	}
}

type slashCtl struct{}

func (c *slashCtl) Index(ctx context.Context) (string, error) { return "index", nil }
func (c *slashCtl) Show(ctx context.Context, id int64) (string, error) {
	return fmt.Sprint("show-", id), nil
}

func TestControllerTrailingSlash(t *testing.T) {
	app, stop, err := InProcess(config.Runtime{},
		Module("m", Path("/m"),
			Controller[*slashCtl]("/things").Supply(&slashCtl{}).TrailingSlash().
				Get("", (*slashCtl).Index).
				Get("/:id/view", (*slashCtl).Show)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	for path, want := range map[string]string{
		"/m/things": "index", "/m/things/": "index",
		"/m/things/4/view": "show-4", "/m/things/4/view/": "show-4",
	} {
		if code, body := ctlDo(t, app, "GET", path, ""); code != 200 || !strings.Contains(body, want) {
			t.Errorf("GET %s = %d %s, want %s", path, code, body, want)
		}
	}
	if code, _ := ctlDo(t, app, "GET", "/m/things/4/view/extra", ""); code != 404 {
		t.Errorf("a trailing-slash twin must not match deeper paths: got %d", code)
	}
}

// annotCtl stands in for a controller whose actions come from annotations:
// ControllerActions is what the generator emits for them.
type annotCtl struct{}

func (c *annotCtl) Home(ctx context.Context) (string, error)  { return "home", nil }
func (c *annotCtl) Other(ctx context.Context) (string, error) { return "other", nil }
func (c *annotCtl) Stats(ctx context.Context) (*ctlUser, error) {
	return &ctlUser{Name: "stats"}, nil
}

// A Resource declared in Go takes the annotated actions: they mount under
// its module's Path, and the fallback the generated code also returns stays
// silent — even though the Resource has no conventional actions of its own.
func TestResourceTakesAnnotatedActions(t *testing.T) {
	module := Module("admin", Path("/admin"),
		Resource[*annotCtl]("/").Supply(&annotCtl{}),
	)
	generated := Module("admin", ControllerActions(func(c *ControllerRouter[*annotCtl]) {
		c.Get("/", (*annotCtl).Home)
		c.Get("/other", (*annotCtl).Other)
		c.Query((*annotCtl).Stats)
	}))
	for build := 1; build <= 2; build++ { // the same module boots again (tests do)
		app, stop, err := InProcess(config.Runtime{}, module, generated)
		if err != nil {
			t.Fatalf("build %d: %v", build, err)
		}
		for path, want := range map[string]string{"/admin/": "home", "/admin/other": "other"} {
			if code, body := ctlDo(t, app, "GET", path, ""); code != 200 || !strings.Contains(body, want) {
				t.Errorf("build %d: GET %s = %d %s", build, path, code, body)
			}
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/admin/graphql", strings.NewReader(`{"query":"{ stats { name } }"}`))
		r.Header.Set("Content-Type", "application/json")
		app.ServeHTTP(w, r)
		if !strings.Contains(w.Body.String(), `"stats"`) {
			t.Errorf("build %d: GraphQL on the module endpoint = %s", build, w.Body.String())
		}
		_ = stop(context.Background())
	}
}

type standaloneCtl struct{}

func (c *standaloneCtl) Ping(ctx context.Context) (string, error) { return "pong", nil }

// With no controller for the type in the app, the actions register on their
// own under the module they were generated into.
func TestControllerActionsStandalone(t *testing.T) {
	app, stop, err := InProcess(config.Runtime{},
		Supply(&standaloneCtl{}),
		Module("tools", Path("/tools"), ControllerActions(func(c *ControllerRouter[*standaloneCtl]) {
			c.Get("/ping", (*standaloneCtl).Ping)
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	if code, body := ctlDo(t, app, "GET", "/tools/ping", ""); code != 200 || !strings.Contains(body, "pong") {
		t.Fatalf("GET /tools/ping = %d %s", code, body)
	}
	for _, e := range app.Registry().Endpoints() {
		if e.Path == "/tools/ping" && e.Module != "tools" {
			t.Errorf("module = %q, want the enclosing tools module", e.Module)
		}
	}
}

func TestRouterMountedTwiceInOneApp(t *testing.T) {
	r := NewRouter("dup", "/dup").Rest("GET", "", func() (string, error) { return "x", nil })
	_, stop, err := InProcess(config.Runtime{}, r, r)
	if stop != nil {
		defer func() { _ = stop(context.Background()) }()
	}
	if err == nil || !strings.Contains(err.Error(), "mounted twice") {
		t.Fatalf("want a double-mount error, got %v", err)
	}
}
