package viewtest_test

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
	"github.com/paulmanoni/nexus/v2/nexustest"
	"github.com/paulmanoni/nexus/v2/view"
	"github.com/paulmanoni/nexus/v2/view/viewtest"
)

// Report is a live page: pick a source and key, run it, save it.
type Report struct {
	User   string
	Keys   []string
	Ran    int
	Title  string
	Key    string
	Saved  []string
	Notice string
}

type ReportForm struct {
	Title string `form:"title"`
	Key   string `form:"ds_keys"`
	Agree bool   `form:"agree"`
}

type store struct {
	mu    sync.Mutex
	saved []string
}

func (s *store) add(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, v)
}

func (s *store) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.saved...)
}

func (r *Report) Mount(ctx context.Context, sock *view.Socket, s *store) error {
	if id, ok := auth.IdentityFrom(ctx); ok && id != nil {
		r.User = id.ID
	}
	r.Keys = []string{"main.id", "main.name", "main.email"}
	r.Key = "main.name"
	r.Saved = s.all()
	if sock.Connected() {
		sock.Subscribe("reports")
	}
	return nil
}

func (r *Report) Run(ctx context.Context) error {
	r.Ran++
	return nil
}

func (r *Report) Save(ctx context.Context, s *store, in ReportForm) error {
	errs := nexus.Invalid()
	if strings.TrimSpace(in.Title) == "" {
		errs.Field("title", "a title is required")
	}
	if !in.Agree {
		errs.Field("agree", "agree first")
	}
	if errs.Any() {
		return errs
	}
	s.add(in.Title + ":" + in.Key)
	r.Key = in.Key
	view.Broadcast(ctx, "reports", in.Title)
	return nil
}

func (r *Report) Clear(ctx context.Context) error {
	r.Title = "cleared"
	return nil
}

func (r *Report) Info(ctx context.Context, s *store, msg view.Message) error {
	r.Saved = s.all()
	return nil
}

func (r *Report) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		var b strings.Builder
		b.WriteString(`<!DOCTYPE html><html><head><title>Report</title>`)
		if err := view.Script().Render(ctx, &b); err != nil {
			return err
		}
		fmt.Fprintf(&b, `</head><body><p id="user">%s</p>`, html.EscapeString(r.User))
		fmt.Fprintf(&b, `<button id="run" onclick="%s">run</button><p id="ran">%d</p>`, view.Send(r.Run).Call, r.Ran)
		fmt.Fprintf(&b, `<button id="clear" onclick="%s">clear</button>`, view.Send(r.Clear).Call)
		fmt.Fprintf(&b, `<form id="f" onsubmit="%s"><input name="title" value="%s">`, view.Submit(r.Save).Call, html.EscapeString(r.Title))
		b.WriteString(`<select name="ds_keys">`)
		for _, k := range r.Keys {
			sel := ""
			if k == r.Key {
				sel = " selected"
			}
			fmt.Fprintf(&b, `<option value="%s"%s>%s</option>`, k, sel, k)
		}
		b.WriteString(`</select><label><input type="checkbox" name="agree"> agree</label>`)
		fmt.Fprintf(&b, `<p id="err">%s</p>`, html.EscapeString(view.Errors(ctx).Field("title")+" "+view.Errors(ctx).Field("agree")))
		disabled := ""
		if r.Ran == 0 {
			disabled = " disabled"
		}
		fmt.Fprintf(&b, `<button id="save" type="submit"%s>save</button></form><ul id="saved">`, disabled)
		for _, s := range r.Saved {
			fmt.Fprintf(&b, `<li>%s</li>`, html.EscapeString(s))
		}
		b.WriteString(`</ul>`)
		if err := view.Link("/", templ.Attributes{"id": "home"}).Render(templ.WithChildren(ctx, templ.Raw("home")), &b); err != nil {
			return err
		}
		b.WriteString(`</body></html>`)
		_, err := io.WriteString(w, b.String())
		return err
	})
}

func home() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		var b strings.Builder
		b.WriteString(`<!DOCTYPE html><html><head><title>Home</title>`)
		if err := view.Script().Render(ctx, &b); err != nil {
			return err
		}
		b.WriteString(`</head><body><h1 id="h">Home</h1>`)
		if err := view.Link("/report", templ.Attributes{"id": "to-report"}).Render(templ.WithChildren(ctx, templ.Raw("report")), &b); err != nil {
			return err
		}
		b.WriteString(`<a id="plain" href="/report">plain</a></body></html>`)
		_, err := io.WriteString(w, b.String())
		return err
	})
}

type tokens struct{}

func (tokens) Resolve(_ context.Context, tok string) (*auth.Identity, error) {
	if tok == "staff" {
		return &auth.Identity{ID: "ana"}, nil
	}
	return nil, errors.New("unknown token")
}

func newApp(t *testing.T) *nexustest.App {
	return nexustest.New(t, config.Runtime{},
		auth.Module(auth.Config{
			Authentication: auth.Authentication{Schemes: []auth.Scheme{{Extract: auth.Bearer()}}},
			Backend:        auth.StaticBackend(tokens{}),
		}),
		nexus.Supply(&store{}),
		view.Page("GET", "/", home, nexus.Public()),
		view.Live[*Report]("/report", auth.Required()).Provide(func() *Report { return &Report{} }),
	)
}

func TestLivePage(t *testing.T) {
	app := newApp(t)
	p := viewtest.Mount[*Report](t, app, viewtest.As("staff"))

	p.Expect("#user").Text("ana")
	p.Expect("#save").Disabled()
	p.Click("#run")
	p.Expect("#ran").Text("1")
	p.Expect("#save").Enabled()

	// An invalid submit keeps what was typed and shows the errors.
	p.Fill("title", "Sales").Select("ds_keys", "main.id").Click("#save")
	p.Expect("#err").ContainsText("agree first")
	p.Expect("title").Value("Sales")
	p.Expect("ds_keys").Value("main.id")

	// A valid one resets the form to the new render.
	p.Check("agree").Click("#save").Wait()
	p.Expect("#saved li").Text("Sales:main.id")
	p.Expect("title").Value("")
	p.Expect("ds_keys").Value("main.id") // the server now marks main.id
	p.Expect("agree").Unchecked()

	// The server's value replaces what the user typed when it changes.
	p.Fill("title", "draft").Click("#clear")
	p.Expect("title").Value("cleared")
}

func TestServerPush(t *testing.T) {
	app := newApp(t)
	a := viewtest.Mount[*Report](t, app, viewtest.As("staff"))
	b := viewtest.Mount[*Report](t, app, viewtest.As("staff"))
	a.Click("#run").Fill("title", "Q3").Check("agree").Click("#save")
	b.Expect("#saved li").Text("Q3:main.name") // broadcast to the other page
}

func TestGatesAndNavigation(t *testing.T) {
	app := newApp(t)
	if p := viewtest.Get(t, app, "/report"); p.Status() != 401 {
		t.Fatalf("anonymous /report = %d, want 401", p.Status())
	}

	p := viewtest.Get(t, app, "/", viewtest.As("staff"))
	p.Expect("#h").Text("Home")
	p.Click("#to-report") // view.Link: fetched and patched in, the live page connects
	p.Expect("#user").Text("ana")
	if !strings.HasSuffix(p.URL(), "/report") {
		t.Fatalf("URL %s", p.URL())
	}
	p.Click("#run").Expect("#ran").Text("1")
	p.Back()
	p.Expect("#h").Text("Home")

	p.Click("#plain") // a plain link loads the page
	p.Expect("#ran").Text("0")
}
