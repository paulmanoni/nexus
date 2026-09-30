package view

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus"
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
	app, stop, err := nexus.InProcess(nexus.Config{}, Live[*shelterLive]("/shelter").Provide(func() *shelterLive { return &shelterLive{} }))
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
	app, stop, err := nexus.InProcess(nexus.Config{}, Live[*panicky]("/p").Provide(func() *panicky { return &panicky{} }))
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
