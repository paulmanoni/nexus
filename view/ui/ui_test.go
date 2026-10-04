package ui

import (
	"context"
	"html"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"github.com/dop251/goja"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
)

func render(t *testing.T, c templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func withChildren(c templ.Component, children string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		return c.Render(templ.WithChildren(ctx, templ.Raw(children)), w)
	})
}

func expect(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in\n%s", w, got)
		}
	}
}

func refuse(t *testing.T, got string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(got, b) {
			t.Errorf("unexpected %q in\n%s", b, got)
		}
	}
}

// send stands in for view.Send: a script whose Call is escaped for an
// attribute, as view's are.
func send(js string) templ.ComponentScript {
	return templ.ComponentScript{Call: html.EscapeString(js)}
}

func TestButton(t *testing.T) {
	got := render(t, withChildren(Button(ButtonProps{
		ID: "save", Variant: Danger, Size: Sm, Icon: Icon("check"), Hotkey: "mod+s",
		OnClick: send(`__nx.live.send(this,"Save",[])`),
	}), "Save"))
	expect(t, got,
		`<button type="button"`, `id="save"`, `data-ui-hotkey="mod+s"`,
		`bg-[color:var(--ui-danger)]`, `h-8 px-3`,
		`onclick="__nx.live.send(this,&#34;Save&#34;,[])"`,
		`ui-btn-spinner hidden`, `ui-btn-icon`, `Save</button>`)
	refuse(t, got, " disabled>", " disabled ", "aria-busy=")

	got = render(t, withChildren(Button(ButtonProps{Loading: true}), "Run"))
	expect(t, got, `disabled`, `aria-busy="true"`, `bg-[color:var(--ui-primary)]`)

	got = render(t, withChildren(Button(ButtonProps{Href: "/orders", Nav: true, Variant: Outline}), "Orders"))
	expect(t, got, `<a href="/orders"`, `data-nx-nav`, `border-[color:var(--ui-border)]`)
	refuse(t, got, "<button", "onclick")

	got = render(t, Button(ButtonProps{IconOnly: true, Title: "Delete", Icon: Icon("trash")}))
	expect(t, got, `aria-label="Delete"`, `size-9`, `<span class="sr-only">Delete</span>`)
}

func TestLoading(t *testing.T) {
	s := Loading(send(`__nx.live.send(this,"Run",[])`), "preview", "query")
	got := render(t, templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := io.WriteString(w, html.UnescapeString(s.Call))
		return err
	}))
	if want := `window.nxui&&nxui.loading(this,["preview","query"]);__nx.live.send(this,"Run",[])`; got != want {
		t.Fatalf("Loading = %s, want %s", got, want)
	}
	if s := Loading(send("x()")); html.UnescapeString(s.Call) != `window.nxui&&nxui.loading(this,[]);x()` {
		t.Fatalf("Loading without ids = %s", s.Call)
	}
	expect(t, render(t, withChildren(Button(ButtonProps{OnClick: Loading(send("go()"), "out")}), "Go")),
		`onclick="window.nxui&amp;&amp;nxui.loading(this,[&#34;out&#34;]);go()"`)
	if a := LoadingAttr("a", "b"); a["data-ui-loading"] != "a b" {
		t.Fatalf("LoadingAttr = %v", a)
	}
}

func TestField(t *testing.T) {
	got := render(t, withChildren(Field(FieldProps{Name: "email", Label: "Email", Help: "We never share it", Required: true}),
		render(t, Input(InputProps{Name: "email", Type: "email", Value: "a@b.c"}))))
	expect(t, got, `<label for="email"`, `Email`, `*</span>`, `We never share it`,
		`<input type="email"`, `name="email"`, `id="email"`, `value="a@b.c"`, `id="email-error"`, `hidden`)
	refuse(t, got, `aria-invalid="`)

	got = render(t, Field(FieldProps{Name: "email", Error: "taken"}))
	expect(t, got, `taken</p>`)
	refuse(t, got, " hidden>")

	got = render(t, Input(InputProps{Name: "n", Invalid: true}))
	expect(t, got, `aria-invalid="true"`, `aria-describedby="n-error"`)

	got = render(t, Select(SelectProps{Name: "kind", Value: "cat", Placeholder: "Pick one",
		Options: []Option{{Value: "dog", Label: "Dog"}, {Value: "cat", Label: "Cat"}, {Value: "fish", Disabled: true}}}))
	expect(t, got, `Pick one</option>`, `<option value="cat" selected>Cat</option>`, `<option value="fish" disabled>fish</option>`)
	refuse(t, got, `<option value="dog" selected`)

	expect(t, render(t, Textarea(TextareaProps{Name: "note", Value: "<hi>"})), `rows="3"`, `&lt;hi&gt;</textarea>`)
	expect(t, render(t, Checkbox(CheckboxProps{Name: "ok", Label: "Agree", Checked: true})), `type="checkbox"`, `value="on"`, `checked`, `Agree`)
}

func TestTabs(t *testing.T) {
	got := render(t, Tabs(TabsProps{Tabs: []Tab{
		{ID: "t-all", Label: "All", Count: "12", Active: true, OnSelect: send("all()")},
		{Label: "Open", Panel: "open-panel"},
		{Label: "Docs", Href: "/docs", Nav: true},
	}}))
	expect(t, got, `role="tablist"`, `data-ui-tabs`,
		`id="t-all"`, `aria-selected="true"`, `onclick="all()"`, `12</span>`,
		`aria-controls="open-panel"`, `aria-selected="false"`,
		`<a role="tab" href="/docs"`, `data-nx-nav`)
	expect(t, render(t, TabPanel("open-panel", false)), `id="open-panel" role="tabpanel" data-ui-panel hidden`)
}

func TestDialog(t *testing.T) {
	got := render(t, withChildren(Dialog(DialogProps{Title: "Delete order?", OnClose: send("close()"), Footer: templ.Raw("<b>f</b>")}), "body"))
	expect(t, got, `data-ui-dialog`, `role="dialog"`, `aria-modal="true"`, `Delete order?`,
		`data-ui-dialog-close onclick="close()"`, `body`, `<footer`, `<b>f</b>`)
	refuse(t, got, " hidden>", " hidden ", "data-nx-ignore")

	got = render(t, Dialog(DialogProps{ID: "help", Title: "Help", Persistent: true, Size: Lg}))
	expect(t, got, `id="help"`, `hidden`, `data-nx-ignore`, `data-persistent`, `aria-labelledby="help-title"`,
		`data-ui-dialog-close data-ui-close`, `max-w-3xl`)
	expect(t, render(t, Dialog(DialogProps{ID: "x", Open: true})), `data-nx-ignore`)
	refuse(t, render(t, Dialog(DialogProps{ID: "x", Open: true})), " hidden")
}

func TestMenus(t *testing.T) {
	items := []MenuItem{
		{Label: "Open", Href: "/orders/7", Nav: true},
		{Label: "Copy link", Copy: "/orders/7", Icon: Icon("link")},
		{Separator: true},
		{ID: "del-7", Label: "Delete", Danger: true, OnClick: send("del(7)")},
		{Label: "Archive", Disabled: true},
	}
	got := render(t, RowActions(items...))
	expect(t, got, `data-ui-menu-root`, `data-ui-menu-trigger`, `aria-label="Actions"`, `aria-haspopup="menu"`,
		`role="menu" data-ui-menu hidden`,
		`<a role="menuitem" href="/orders/7"`, `data-nx-nav`,
		`data-ui-copy="/orders/7"`, `role="separator"`,
		`id="del-7"`, `onclick="del(7)"`, `text-[color:var(--ui-danger)]`,
		`disabled`)

	got = render(t, withChildren(Dropdown(ButtonProps{ID: "more"}, items[:1]...), "More"))
	expect(t, got, `id="more"`, `data-ui-menu-trigger`, `aria-expanded="false"`, `More`, `border-[color:var(--ui-border)]`)
}

func TestDataTable(t *testing.T) {
	p := TableProps{
		ID:      "orders",
		Columns: []Column{{Key: "number", Label: "Number", Sortable: true}, {Key: "total", Label: "Total", Align: "right", Sortable: true}},
		Rows:    2, Total: 45, Page: 3, PageSize: 10, PageSizes: []int{10, 25},
		Sort: "total", Desc: true, Query: "ab",
		OnSearch: send("search()"),
		OnSort:   func(k string) templ.ComponentScript { return send("sort('" + k + "')") },
		OnPage:   func(n int) templ.ComponentScript { return send("page(" + strconv.Itoa(n) + ")") },
		Actions:  true,
	}
	rows := render(t, withChildren(TableRow(RowProps{ID: "o-1", Href: "/orders/1", Actions: []MenuItem{{Label: "Open", Href: "/orders/1"}}}), "<td>1</td>"))
	expect(t, rows, `id="o-1"`, `data-ui-href="/orders/1"`, `<td>1</td>`, `data-ui-menu-trigger`)

	got := render(t, withChildren(DataTable(p), rows))
	expect(t, got,
		`id="orders"`, `id="orders-search"`, `role="search"`, `oninput="search()"`, `onchange="search()"`,
		`id="orders-q"`, `name="q"`, `value="ab"`, `<input type="hidden" name="size" value="10">`,
		`aria-sort="none"`, `aria-sort="descending"`, `onclick="sort(&#39;total&#39;)"`,
		`<span class="sr-only">Actions</span>`, `<td>1</td>`,
		`21–22`, `45</b>`,
		`id="orders-size"`, `<input type="hidden" name="q" value="ab">`, `<option value="25">25</option>`, `<option value="10" selected>`,
		`onclick="page(2)"`, `aria-current="page" onclick="page(3)"`, `onclick="page(5)"`, `aria-label="Next page"`)

	p.Rows, p.Total = 0, 0
	got = render(t, withChildren(DataTable(p), ""))
	expect(t, got, `colspan="3"`, `No matches for “ab”.`)
	refuse(t, got, `Showing`)

	got = render(t, DataTable(TableProps{Columns: []Column{{Label: "A"}}, EmptyText: "No orders yet."}))
	expect(t, got, `id="table"`, `No orders yet.`)
	refuse(t, got, `role="search"`, `<button`)
}

func TestPageWindow(t *testing.T) {
	for _, c := range []struct {
		page, last int
		want       []int
	}{
		{1, 1, []int{1}},
		{3, 7, []int{1, 2, 3, 4, 5, 6, 7}},
		{1, 20, []int{1, 2, 0, 20}},
		{10, 20, []int{1, 0, 9, 10, 11, 0, 20}},
		{20, 20, []int{1, 0, 19, 20}},
		{3, 20, []int{1, 2, 3, 4, 0, 20}},
		{99, 20, []int{1, 0, 19, 20}},
	} {
		if got := pageWindow(c.page, c.last); !reflect.DeepEqual(got, c.want) {
			t.Errorf("pageWindow(%d, %d) = %v, want %v", c.page, c.last, got, c.want)
		}
	}
}

func TestSmallComponents(t *testing.T) {
	got := render(t, Toast(ToastProps{ID: "saved-3", Title: "Saved", Text: "Order 7 saved", Variant: Success}))
	expect(t, got, `hidden`, `data-ui-toast="saved-3"`, `data-ui-variant="success"`, `data-ui-title="Saved"`, `data-ui-duration="4000"`, `Order 7 saved`)
	expect(t, render(t, Toast(ToastProps{Text: "hi", Duration: -1})), `data-ui-toast="|hi"`, `data-ui-duration="-1"`)

	expect(t, render(t, Loader(LoaderProps{Label: "Loading orders"})), `role="status"`, `animate-spin`, `Loading orders`)
	expect(t, render(t, Loader(LoaderProps{Overlay: true})), `absolute inset-0`, `sr-only">Loading`)
	expect(t, render(t, Skeleton("h-4 w-32")), `animate-pulse`, `h-4 w-32`)

	got = render(t, withChildren(PageHeader(PageHeaderProps{Title: "Orders", Description: "Every order",
		Crumbs: []Crumb{{Label: "Home", Href: "/"}, {Label: "Orders"}}}), "<button>New</button>"))
	expect(t, got, `<h1`, `Orders</h1>`, `Every order`, `aria-label="Breadcrumb"`, `<a href="/" data-nx-nav`, `aria-current="page"`, `<button>New</button>`)

	expect(t, render(t, withChildren(Badge(Success), "paid")), `bg-[color:var(--ui-success)]/12`, `paid`)
	expect(t, render(t, withChildren(Badge(""), "new")), `bg-[color:var(--ui-secondary)]`)
	expect(t, render(t, withChildren(EmptyState("Nothing"), "<a>add</a>")), `Nothing</p>`, `<a>add</a>`)
	if render(t, Icon("nope")) != "" {
		t.Error("an unknown icon draws something")
	}
}

func TestAttrHelpers(t *testing.T) {
	if CopyLink("/a")["data-ui-copy"] != "/a" || Hotkey("mod+k")["data-ui-hotkey"] != "mod+k" ||
		OpenDialog("d")["data-ui-open"] != "d" || CloseDialog()["data-ui-close"] != true {
		t.Fatal("attribute helpers")
	}
}

// Importing the package serves its files; Script points at them.
func TestAssetsServed(t *testing.T) {
	app, stop, err := nexus.InProcess(config.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	for path, want := range map[string]string{Prefix + "ui.js": "nxui", Prefix + "ui.css": "--ui-primary"} {
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s = %d", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest("GET", Prefix+"nope.js", nil))
	if rec.Code != 404 {
		t.Errorf("GET nope.js = %d", rec.Code)
	}
	expect(t, render(t, Script()), `href="/_view/ui/ui.css?v=`, `src="/_view/ui/ui.js?v=`, `defer`)
}

// ui.js is served as-is: ASCII only, like the view runtime.
func TestJSIsASCII(t *testing.T) {
	for i, r := range uiJS {
		if r > 127 {
			t.Fatalf("ui.js has %U at byte %d — write it as a \\u escape", r, i)
		}
	}
}

func TestJS(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunString(uiJS); err != nil {
		t.Fatal(err)
	}
	run := func(src string) goja.Value {
		t.Helper()
		v, err := vm.RunString(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		return v
	}
	for src, want := range map[string]bool{
		`nxui._matchHotkey(nxui._parseHotkey("mod+s"), {key: "s", ctrlKey: true}, false)`:                       true,
		`nxui._matchHotkey(nxui._parseHotkey("mod+s"), {key: "s", metaKey: true}, true)`:                        true,
		`nxui._matchHotkey(nxui._parseHotkey("mod+s"), {key: "s", metaKey: true}, false)`:                       false,
		`nxui._matchHotkey(nxui._parseHotkey("mod+s"), {key: "S", ctrlKey: true, shiftKey: true}, false)`:       false,
		`nxui._matchHotkey(nxui._parseHotkey("mod+shift+s"), {key: "S", ctrlKey: true, shiftKey: true}, false)`: true,
		`nxui._matchHotkey(nxui._parseHotkey("?"), {key: "?", shiftKey: true}, false)`:                          true,
		`nxui._matchHotkey(nxui._parseHotkey("esc"), {key: "Escape"}, false)`:                                   true,
		`nxui._matchHotkey(nxui._parseHotkey("mod+enter"), {key: "Enter", ctrlKey: true}, false)`:               true,
		`nxui._matchHotkey(nxui._parseHotkey("mod++"), {key: "+", ctrlKey: true}, false)`:                       true,
		`nxui._matchHotkey(nxui._parseHotkey("/"), {key: "/", altKey: true}, false)`:                            false,
		`nxui._matchHotkey(nxui._parseHotkey(""), {key: ""}, false)`:                                            false,
	} {
		if got := run(src).ToBoolean(); got != want {
			t.Errorf("%s = %v, want %v", src, got, want)
		}
	}
	if got := run(`nxui._copyText("/orders/7", "https://shop.test")`).String(); got != "https://shop.test/orders/7" {
		t.Errorf("copy path = %s", got)
	}
	if got := run(`nxui._copyText("//cdn.test/x", "https://shop.test") + "|" + nxui._copyText("ABC-1", "o")`).String(); got != "//cdn.test/x|ABC-1" {
		t.Errorf("copy text = %s", got)
	}
	// Below and right-aligned with room; above near the bottom; clamped.
	if got := run(`JSON.stringify(nxui._placeMenu({top: 100, bottom: 130, right: 400}, 200, {width: 1000, height: 800}))`).String(); got != `{"left":200,"top":134,"bottom":null,"maxHeight":656}` {
		t.Errorf("below = %s", got)
	}
	if got := run(`JSON.stringify(nxui._placeMenu({top: 700, bottom: 730, right: 100}, 200, {width: 1000, height: 800}))`).String(); got != `{"left":8,"top":null,"bottom":104,"maxHeight":686}` {
		t.Errorf("above = %s", got)
	}
	// Loading twice keeps the first copy.
	if _, err := vm.RunString(`var first = nxui;` + uiJS + `;if (nxui !== first) throw new Error("replaced")`); err != nil {
		t.Fatal(err)
	}
}

// The kit's generated components record live render trees (make view-ui
// after templ generate).
func TestKitIsInstrumented(t *testing.T) {
	files, _ := filepath.Glob("*_templ.go")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "templ_nx_view.Record(") {
			t.Errorf("%s is not instrumented: run make view-ui", f)
		}
	}
}
