package view

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
)

type petForm struct {
	Name string `form:"name"`
	Age  int    `form:"age"`
}

type shelterLive struct {
	Pets  []string
	Draft petForm
}

func (s *shelterLive) Validate(ctx context.Context, in petForm) error {
	s.Draft = in
	return check(in)
}

func (s *shelterLive) Add(ctx context.Context, in petForm) error {
	if err := check(in); err != nil {
		s.Draft = in
		return err
	}
	s.Pets = append(s.Pets, fmt.Sprintf("%s(%d)", in.Name, in.Age))
	s.Draft = petForm{}
	return nil
}

func check(in petForm) error {
	errs := nexus.NewErrors()
	if strings.TrimSpace(in.Name) == "" {
		errs.Field("name", "a name is required")
	}
	if in.Age < 0 {
		errs.Field("age", "an age is not negative")
	}
	if errs.Any() {
		return errs
	}
	return nil
}

func (s *shelterLive) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		errs := Errors(ctx)
		_, err := fmt.Fprintf(w, `<form onsubmit="%s" oninput="%s"><input name="name" value="%s"/><p id="name-err">%s</p><p id="age-err">%s</p></form><p id="pets">%s</p>`,
			Submit(s.Add).Call, Change(s.Validate).Call, s.Draft.Name, errs.Field("name"), errs.Field("age"), strings.Join(s.Pets, ","))
		return err
	})
}

func bootShelter(t *testing.T) *httptest.Server {
	t.Helper()
	app, stop, err := nexus.InProcess(config.Runtime{}, Live[*shelterLive]("/shelter").Provide(func() *shelterLive { return &shelterLive{} }))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

func TestLiveForms(t *testing.T) {
	srv := bootShelter(t)
	c := dialLive(t, srv, "/shelter/_live")
	first := reply(t, c)
	for _, want := range []string{
		`onsubmit="__nx.live.submit(event,this,&#34;Add&#34;)"`,
		`oninput="__nx.live.change(event,this,&#34;Validate&#34;)"`,
	} {
		if !strings.Contains(first.HTML, want) {
			t.Fatalf("render lacks %s:\n%s", want, first.HTML)
		}
	}
	submit := func(conn *websocket.Conn, ref int, event string, form map[string][]string) liveReply {
		if err := conn.WriteJSON(liveEvent{Ref: ref, Event: event, Form: form}); err != nil {
			t.Fatal(err)
		}
		return reply(t, conn)
	}

	r := submit(c, 1, "Add", map[string][]string{"name": {" "}, "age": {"-2"}})
	if r.Ref != 1 || !r.Invalid || r.Error != "" ||
		!strings.Contains(r.HTML, `<p id="name-err">a name is required</p>`) ||
		!strings.Contains(r.HTML, `<p id="age-err">an age is not negative</p>`) {
		t.Fatalf("an invalid submit = %+v", r)
	}

	r = submit(c, 2, "Validate", map[string][]string{"name": {"Mochi"}, "age": {"3"}})
	if r.Ref != 2 || r.Invalid || !strings.Contains(r.HTML, `<p id="name-err"></p>`) || !strings.Contains(r.HTML, `value="Mochi"`) {
		t.Fatalf("validation as you type = %+v", r)
	}

	r = submit(c, 3, "Add", map[string][]string{"name": {"Mochi"}, "age": {"3"}})
	if r.Ref != 3 || r.Invalid || !strings.Contains(r.HTML, `<p id="pets">Mochi(3)</p>`) || !strings.Contains(r.HTML, `value=""`) {
		t.Fatalf("a valid submit = %+v", r)
	}

	if r = submit(c, 4, "Add", map[string][]string{"age": {"old"}}); r.Error == "" || r.Ref != 4 {
		t.Fatalf("a form that does not bind = %+v", r)
	}
}

type panicky struct{}

func (p *panicky) Boom(ctx context.Context) error { panic("kaboom") }

func (p *panicky) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := io.WriteString(w, `<p>ok</p>`)
		return err
	})
}

// A panicking event is reported to the page; the connection survives.
func TestLivePanicIsAnError(t *testing.T) {
	app, stop, err := nexus.InProcess(config.Runtime{}, Live[*panicky]("/p").Provide(func() *panicky { return &panicky{} }))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	defer func() { srv.Close(); _ = stop(context.Background()) }()
	c := dialLive(t, srv, "/p/_live")
	reply(t, c)
	if err := c.WriteJSON(liveEvent{Ref: 7, Event: "Boom"}); err != nil {
		t.Fatal(err)
	}
	if r := reply(t, c); r.Ref != 7 || !strings.Contains(r.Error, "panic: kaboom") {
		t.Fatalf("reply = %+v", r)
	}
	if err := c.WriteJSON(liveEvent{Ref: 8, Event: "Boom"}); err != nil {
		t.Fatal(err)
	}
	if r := reply(t, c); r.Ref != 8 {
		t.Fatalf("the connection did not survive: %+v", r)
	}
}

func TestBindFormValues(t *testing.T) {
	sent := map[string][]string{"name": {"Rex"}, "tags": {"a", "b"}}
	v, err := bindForm(reflect.TypeFor[url.Values](), sent)
	if err != nil {
		t.Fatal(err)
	}
	got := v.Interface().(url.Values)
	if got.Get("name") != "Rex" || len(got["tags"]) != 2 {
		t.Fatalf("got %v", got)
	}
	if _, err := bindForm(reflect.TypeFor[int](), sent); err == nil || !strings.Contains(err.Error(), "url.Values") {
		t.Fatalf("a non-struct should be refused, naming the choices: %v", err)
	}
}

type genericLive[T any] struct{ v T }

func (g *genericLive[T]) Ping(ctx context.Context) error { return nil }

func TestSendNamesGenericMethods(t *testing.T) {
	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, "generic type") {
			t.Fatalf("want a generic-type explanation, got %q", msg)
		}
	}()
	g := &genericLive[int]{}
	// A generic method value taken inside generic code is a closure.
	sendFrom(g)
}

func sendFrom[T any](g *genericLive[T]) { Send(g.Ping) }

func TestSendToNamesMethodsOfGenericTypes(t *testing.T) {
	g := &genericLive[int]{}
	if got := SendTo(g, "Ping", 7).Call; !strings.Contains(got, "&#34;Ping&#34;") || !strings.Contains(got, "[7]") {
		t.Fatalf("SendTo: %s", got)
	}
	if got := SubmitTo(g, "Ping").Call; !strings.Contains(got, "submit") {
		t.Fatalf("SubmitTo: %s", got)
	}
	defer func() {
		if msg := fmt.Sprint(recover()); !strings.Contains(msg, `no exported method "Nope"`) {
			t.Fatalf("want a missing-method panic, got %q", msg)
		}
	}()
	SendTo(g, "Nope")
}
